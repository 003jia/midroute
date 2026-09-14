// Package router 实现模型别名解析、候选选择与安全重试（对齐 PRD US-007 / FR-14..18）。
package router

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"midroute/internal/connectors"
	"midroute/internal/domain"
	"midroute/internal/repository"
)

// Decision 一次路由决策（不含 Prompt/回答/密钥，对齐 FR-15）。
type Decision struct {
	RequestID   string   `json:"request_id"`
	Alias       string   `json:"alias"`
	PolicyID    string   `json:"policy_id,omitempty"`
	Selected    string   `json:"selected"` // accountID@modelID
	Candidates  []string `json:"candidates"`
	ReasonCodes []string `json:"reason_codes"`
	CreatedAt   int64    `json:"created_at"`
}

// ResolvedTarget 已解析的可执行目标。
type ResolvedTarget struct {
	AccountID string
	ModelID   string // 上游模型 ID
	Target    connectors.Target
	Conn      connectors.Connector
}

// Resolver 从账户 ID 解析出目标（含凭据解密）。
type Resolver func(ctx context.Context, accountID string) (*ResolvedTarget, error)

// AttemptInfo 一次尝试的开始信息（不含正文/密钥）。
type AttemptInfo struct {
	RequestID    string
	AccountID    string
	ProviderID   string
	LogicalModel string
	ActualModel  string
	Protocol     string
}

// AttemptResult 一次尝试的终态。
type AttemptResult struct {
	Status           string // success|failed|cancelled|partial|unknown
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	Metering         string // exact|reported|estimated|incomplete|unknown
	LatencyMS        int64
	StatusCode       int
	ErrorClass       string
	ReasonCode       string
}

// AttemptHook 尝试记录钩子（由 usage.Recorder 实现；nil 时不做记录）。
type AttemptHook interface {
	StartAttempt(ctx context.Context, info AttemptInfo) string
	FinishAttempt(ctx context.Context, requestID, attemptID string, res AttemptResult)
}

// Router 路由引擎。
type Router struct {
	store    *repository.Store
	resolve  Resolver
	log      *slog.Logger
	Attempts AttemptHook
}

// New 创建 Router。
func New(store *repository.Store, resolve Resolver, log *slog.Logger) *Router {
	return &Router{store: store, resolve: resolve, log: log}
}

// ErrNoCandidate 无可用候选。
var ErrNoCandidate = errors.New("router: no usable candidate")

// ResolveCandidates 导出候选解析（alias → 可用候选 + 策略 ID），
// 供 Responses 等自带转发循环的协议层复用（MR-013）。
func (r *Router) ResolveCandidates(ctx context.Context, alias string) ([]domain.Candidate, string, error) {
	return r.resolveCandidates(ctx, alias)
}

// resolveCandidates 根据逻辑模型名得到候选（账户+上游模型）。
func (r *Router) resolveCandidates(ctx context.Context, model string) ([]domain.Candidate, string, error) {
	policy, err := r.store.GetRoutingPolicyByAlias(ctx, model)
	if err == nil {
		if !policy.Enabled {
			return nil, "", ErrNoCandidate
		}
		if len(policy.Candidates) == 0 {
			return nil, "", ErrNoCandidate
		}
		// 过滤不可用账户
		usable := make([]domain.Candidate, 0, len(policy.Candidates))
		for _, c := range policy.Candidates {
			if ok, err := r.accountUsable(ctx, c.AccountID); err == nil && ok {
				usable = append(usable, c)
			}
		}
		if len(usable) == 0 {
			return nil, policy.ID, ErrNoCandidate
		}
		sort.SliceStable(usable, func(i, j int) bool { return usable[i].Priority < usable[j].Priority })
		return usable, policy.ID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, "", err
	}
	// 无策略：按模型名直连（上游模型或别名）
	return r.defaultCandidates(ctx, model)
}

// defaultCandidates 无策略时，找到拥有该模型且可用的账户。
func (r *Router) defaultCandidates(ctx context.Context, model string) ([]domain.Candidate, string, error) {
	accounts, err := r.store.ListAccounts(ctx)
	if err != nil {
		return nil, "", err
	}
	var cands []domain.Candidate
	for _, a := range accounts {
		if a.Status != "active" || a.Mode == string(domain.ModeMonitorOnly) {
			continue
		}
		// 简单判断：账户可用模型表中存在该模型
		if ok, _ := r.accountHasModel(ctx, a.ID, model); ok {
			cands = append(cands, domain.Candidate{AccountID: a.ID, ModelID: model, Priority: 1, Weight: 1})
		}
	}
	if len(cands) == 0 {
		return nil, "", ErrNoCandidate
	}
	return cands, "", nil
}

func (r *Router) accountUsable(ctx context.Context, accountID string) (bool, error) {
	a, err := r.store.GetAccount(ctx, accountID)
	if err != nil {
		return false, err
	}
	if a.Status != "active" {
		return false, nil
	}
	// 仅监测账户永不成为路由候选（合约 v1 §accounts.mode；MR-004）
	if a.Mode == string(domain.ModeMonitorOnly) {
		return false, nil
	}
	return true, nil
}

func (r *Router) accountHasModel(ctx context.Context, accountID, modelID string) (bool, error) {
	// 简化：直接查询 account_models
	var n int
	err := r.store.CountAccountModel(ctx, accountID, modelID, &n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// Forward 非流式转发：解析→选路→转发→按安全错误失败切换。
func (r *Router) Forward(ctx context.Context, alias string, req *connectors.ChatRequest, requestID string) (*connectors.ChatResponse, *Decision, error) {
	cands, policyID, err := r.resolveCandidates(ctx, alias)
	if err != nil {
		return nil, nil, err
	}
	decision := &Decision{RequestID: requestID, Alias: alias, PolicyID: policyID, CreatedAt: time.Now().Unix()}
	lastErr := ErrNoCandidate
	for i, c := range cands {
		target, err := r.resolve(ctx, c.AccountID)
		if err != nil {
			lastErr = err
			continue
		}
		target.ModelID = c.ModelID
		target.Conn = connectors.NewConnector(target.Target.ProviderKind, connectors.Options{})
		decision.Selected = fmt.Sprintf("%s@%s", c.AccountID, c.ModelID)
		decision.Candidates = append(decision.Candidates, decision.Selected)
		upstreamReq := *req
		upstreamReq.Model = c.ModelID
		attemptID := r.startAttempt(ctx, requestID, c, alias, i)
		start := time.Now()
		resp, err := target.Conn.Forward(ctx, target.Target, &upstreamReq)
		latency := time.Since(start).Milliseconds()
		if err == nil {
			r.finishAttempt(ctx, requestID, attemptID, resp, latency, "success")
			decision.ReasonCodes = append(decision.ReasonCodes, "ok")
			return resp, decision, nil
		}
		r.finishAttemptError(ctx, requestID, attemptID, err, latency, "failed")
		lastErr = err
		decision.ReasonCodes = append(decision.ReasonCodes, "failover")
		if !safeToRetry(err) {
			break
		}
	}
	return nil, decision, lastErr
}

// ForwardStream 流式转发。一旦开始输出，不做整请求重放（US-006）。
func (r *Router) ForwardStream(ctx context.Context, alias string, req *connectors.ChatRequest, requestID string, onChunk func([]byte) error) (*connectors.Usage, *Decision, error) {
	cands, policyID, err := r.resolveCandidates(ctx, alias)
	if err != nil {
		return nil, nil, err
	}
	decision := &Decision{RequestID: requestID, Alias: alias, PolicyID: policyID, CreatedAt: time.Now().Unix()}
	var lastErr error = ErrNoCandidate
	streamed := false
	// 包装回调：记录是否已向客户端输出过内容（一旦输出，禁止重放）
	wrapped := func(b []byte) error {
		streamed = true
		return onChunk(b)
	}
	for i, c := range cands {
		target, err := r.resolve(ctx, c.AccountID)
		if err != nil {
			lastErr = err
			continue
		}
		target.ModelID = c.ModelID
		target.Conn = connectors.NewConnector(target.Target.ProviderKind, connectors.Options{})
		decision.Selected = fmt.Sprintf("%s@%s", c.AccountID, c.ModelID)
		decision.Candidates = append(decision.Candidates, decision.Selected)
		upstreamReq := *req
		upstreamReq.Model = c.ModelID
		attemptID := r.startAttempt(ctx, requestID, c, alias, i)
		start := time.Now()
		usage, err := target.Conn.ForwardStream(ctx, target.Target, &upstreamReq, wrapped)
		latency := time.Since(start).Milliseconds()
		if err == nil {
			r.finishAttemptStream(ctx, requestID, attemptID, usage, latency, "success")
			decision.ReasonCodes = append(decision.ReasonCodes, "ok")
			return usage, decision, nil
		}
		status := "failed"
		if streamed {
			status = "partial" // 已输出内容后断流
		}
		r.finishAttemptStream(ctx, requestID, attemptID, usage, latency, status, err)
		lastErr = err
		decision.ReasonCodes = append(decision.ReasonCodes, "failover")
		if streamed {
			break // 已输出内容，禁止重放（US-006）
		}
		if !safeToRetry(err) {
			break
		}
	}
	return nil, decision, lastErr
}

// startAttempt 记录一次尝试开始；返回 attempt_id（hook 未装配时为 ""）。
func (r *Router) startAttempt(ctx context.Context, requestID string, c domain.Candidate, alias string, idx int) string {
	if r.Attempts == nil {
		return ""
	}
	info := AttemptInfo{
		RequestID: requestID, AccountID: c.AccountID,
		LogicalModel: alias, ActualModel: c.ModelID, Protocol: "chat.completions",
	}
	return r.Attempts.StartAttempt(ctx, info)
}

func (r *Router) finishAttempt(ctx context.Context, requestID, attemptID string, resp *connectors.ChatResponse, latencyMS int64, status string) {
	if r.Attempts == nil || attemptID == "" {
		return
	}
	res := AttemptResult{Status: status, LatencyMS: latencyMS, ReasonCode: "ok"}
	if resp != nil && resp.Usage != nil {
		res.InputTokens = resp.Usage.PromptTokens
		res.OutputTokens = resp.Usage.CompletionTokens
		res.Metering = "reported"
		if resp.Usage.PromptTokensDetails != nil {
			res.CacheReadTokens = resp.Usage.PromptTokensDetails.CachedTokens
		}
	} else {
		res.Metering = "unknown"
	}
	r.Attempts.FinishAttempt(ctx, requestID, attemptID, res)
}

func (r *Router) finishAttemptError(ctx context.Context, requestID, attemptID string, err error, latencyMS int64, status string) {
	if r.Attempts == nil || attemptID == "" {
		return
	}
	res := AttemptResult{Status: status, LatencyMS: latencyMS, Metering: "unknown", ErrorClass: errClass(err), ReasonCode: "upstream_error"}
	if _, ok := err.(*connectors.UpstreamError); ok {
		res.ReasonCode = "upstream_error"
	}
	if e, ok := err.(*connectors.UpstreamError); ok {
		res.StatusCode = e.Status
	}
	r.Attempts.FinishAttempt(ctx, requestID, attemptID, res)
}

func (r *Router) finishAttemptStream(ctx context.Context, requestID, attemptID string, usage *connectors.Usage, latencyMS int64, status string, err ...error) {
	if r.Attempts == nil || attemptID == "" {
		return
	}
	res := AttemptResult{Status: status, LatencyMS: latencyMS, Metering: "unknown"}
	if usage != nil {
		res.InputTokens = usage.InputTokens
		res.OutputTokens = usage.OutputTokens
		res.CacheReadTokens = usage.CacheTokens
		res.Metering = "reported"
	}
	if len(err) > 0 && err[0] != nil {
		res.ErrorClass = errClass(err[0])
		res.ReasonCode = "upstream_error"
	}
	r.Attempts.FinishAttempt(ctx, requestID, attemptID, res)
}

func errClass(err error) string {
	switch {
	case errors.Is(err, connectors.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, connectors.ErrAuth):
		return "auth"
	case errors.Is(err, connectors.ErrTimeout):
		return "timeout"
	case errors.Is(err, connectors.ErrNetwork):
		return "network"
	case errors.Is(err, connectors.ErrTruncated):
		return "truncated"
	case errors.Is(err, connectors.ErrQuotaExhausted):
		return "quota_exhausted"
	default:
		return "upstream_error"
	}
}

// safeToRetry 仅在可安全重试的错误上切换（429、超时、网络、截断、5xx）。
// 使用结构化错误分类（errors.Is），禁止字符串匹配（MR-012）。
func safeToRetry(err error) bool {
	if err == nil {
		return false
	}
	return connectors.Retryable(err)
}
