// responses_oauth.go 承载 /v1/responses 转发（MR-013）、OAuth 管理 API（M3 接线）
// 与项目 CRUD（MR-016）。按文件拆分避免 api.go 过长。
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"midroute/internal/access"
	"midroute/internal/connectors/oauth"
	"midroute/internal/credentials"
	"midroute/internal/domain"
	"midroute/internal/errs"
	"midroute/internal/httpserver"
	"midroute/internal/protocols/responses"
	"midroute/internal/repository"
	"midroute/internal/router"
)

// ============================================================
// /v1/* 项目令牌鉴权（MR-016）
// ============================================================

// tokenCtxKey 请求级令牌信息键。
type tokenCtxKey struct{}

// requireToken 网关鉴权中间件：校验项目访问令牌（与管理会话/管理员密钥分离）。
// 管理员密钥不能自动成为推理凭证——管理面凭 Guard，推理面只认项目令牌。
func (a *App) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.Access == nil {
			httpserver.WriteError(w, errs.New(errs.CodeForbidden, "推理访问未启用"))
			return
		}
		tok, err := a.Access.Authenticate(r.Context(), r)
		if err != nil {
			if errors.Is(err, access.ErrUnauthorized) {
				w.Header().Set("WWW-Authenticate", "Bearer")
				httpserver.WriteError(w, errs.New(errs.CodeUnauthorized, "访问令牌缺失、无效、禁用或已过期"))
				return
			}
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "令牌校验失败", err))
			return
		}
		release, err := a.Access.Acquire(tok.ID, tok.MaxConcurrency)
		if err != nil {
			httpserver.WriteError(w, errs.Retryable(errs.New(errs.CodeRateLimited, "令牌并发达到上限")))
			return
		}
		defer release()
		next(w, r.WithContext(context.WithValue(r.Context(), tokenCtxKey{}, tok)))
	}
}

// tokenFromRequest 取出中间件注入的令牌信息。
func tokenFromRequest(r *http.Request) domain.Token {
	tok, _ := r.Context().Value(tokenCtxKey{}).(domain.Token)
	return tok
}

// checkModelAllowed 白名单校验（请求上游前拒绝，FR-41）。
func (a *App) checkModelAllowed(w http.ResponseWriter, r *http.Request, model string) bool {
	tok := tokenFromRequest(r)
	if !access.ModelAllowed(tok, model) {
		httpserver.WriteError(w, errs.New(errs.CodeForbidden, "令牌无权访问模型: "+model))
		return false
	}
	return true
}

// ============================================================
// /v1/responses（MR-013）
// ============================================================

// responsesBindingTTL 会话句柄绑定有效期。上游句柄实际时效未知，取保守的
// 工程默认；过期后拒绝续接而不是换账户（MR-013 验收第 2 条）。
const responsesBindingTTL = 24 * time.Hour

// maxResponsesBody 请求体上限（4MB，覆盖大上下文）。
const maxResponsesBody = 4 << 20

// respCand 一次 Responses 转发候选；sticky 表示来自会话句柄绑定，不得切换。
type respCand struct {
	accountID string
	modelID   string
	sticky    bool
}

func (a *App) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxResponsesBody+1))
	if err != nil {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "读取请求体失败"))
		return
	}
	if len(body) > maxResponsesBody {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "请求体过大"))
		return
	}
	// 解析最小字段集，其余字段透传保真
	var probe struct {
		Model              string `json:"model"`
		Stream             bool   `json:"stream"`
		PreviousResponseID string `json:"previous_response_id"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "请求体必须是 JSON 对象"))
		return
	}
	if probe.Model == "" {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "缺少 model"))
		return
	}
	if !a.checkModelAllowed(w, r, probe.Model) {
		return
	}
	requestID := "req_" + shortID()

	var cands []respCand
	if probe.PreviousResponseID != "" {
		b, err := a.Store.GetResponseBinding(r.Context(), probe.PreviousResponseID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				httpserver.WriteError(w, errs.New(errs.CodeNotFound, "会话句柄不存在或已过期，请在新请求中重开会话"))
				return
			}
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询会话绑定失败", err))
			return
		}
		acc, err := a.Store.GetAccount(r.Context(), b.AccountID)
		if err != nil || acc.Status != "active" || acc.Mode == string(domain.ModeMonitorOnly) {
			httpserver.WriteError(w, errs.New(errs.CodeUpstreamAuth, "会话句柄绑定的账户当前不可用，请重开会话"))
			return
		}
		cands = []respCand{{accountID: b.AccountID, modelID: probe.Model, sticky: true}}
	} else {
		list, _, err := a.Router.ResolveCandidates(r.Context(), probe.Model)
		if err != nil {
			a.logError(&router.Decision{RequestID: requestID, Alias: probe.Model}, err)
			httpserver.WriteError(w, a.mapRelayError(err))
			return
		}
		for _, c := range list {
			cands = append(cands, respCand{accountID: c.AccountID, modelID: c.ModelID})
		}
	}

	start := time.Now()
	var lastErr error
	for _, c := range cands {
		acc, err := a.Store.GetAccount(r.Context(), c.accountID)
		if err != nil {
			lastErr = err
			continue
		}
		target, err := a.resolveTarget(r.Context(), acc, "")
		if err != nil {
			lastErr = err
			if strings.Contains(err.Error(), "需要重新授权") {
				break // reauth 类错误换账户无意义
			}
			continue
		}
		upBody, err := rewriteResponsesModel(body, c.modelID)
		if err != nil {
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "改写模型字段失败", err))
			return
		}
		_, ferr := a.forwardResponses(w, r, responses.Target{BaseURL: target.BaseURL, Token: target.APIKey}, upBody, probe.Stream, requestID, c, start)
		if ferr == nil {
			return
		}
		lastErr = ferr
		if c.sticky || !responsesRetryable(ferr) {
			break
		}
	}
	a.logError(&router.Decision{RequestID: requestID, Alias: probe.Model}, lastErr)
	if lastErr == nil {
		lastErr = errors.New("no usable candidate")
	}
	a.recordResponsesUsage(requestID, probe.Model, "", nil, time.Since(start).Milliseconds(), a.mapRelayError(lastErr).Code)
	httpserver.WriteError(w, a.mapRelayError(lastErr))
}

// forwardResponses 执行单次转发并写回客户端；成功时落用量与句柄绑定。
// 已开始输出后失败不再切换候选（由 started 标志与返回错误共同保证）。
func (a *App) forwardResponses(w http.ResponseWriter, r *http.Request, t responses.Target, body []byte, stream bool, requestID string, c respCand, start time.Time) (*responses.Meta, error) {
	started := false
	flusher, _ := w.(http.Flusher)
	meta, err := responses.Forward(r.Context(), a.httpClient, t, body, stream, func(b []byte) error {
		if !started {
			started = true
			if !stream {
				w.Header().Set("Content-Type", "application/json")
			}
			w.WriteHeader(http.StatusOK)
		}
		if _, werr := w.Write(b); werr != nil {
			return werr
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	latency := time.Since(start).Milliseconds()
	if err != nil {
		if !started {
			return meta, err // 未输出：交给调用方错误路径
		}
		// 已输出后失败：不再写错误体（客户端已收到部分流），记录后返回
		a.recordResponsesUsage(requestID, c.modelID, c.accountID, meta, latency, errs.CodeUpstreamError)
		return meta, err
	}
	a.recordResponsesUsage(requestID, c.modelID, c.accountID, meta, latency, "")
	if meta != nil && meta.ID != "" {
		b := domain.ResponseBinding{
			ResponseID: meta.ID,
			AccountID:  c.accountID,
			ModelID:    c.modelID,
			ExpiresAt:  time.Now().UTC().Add(responsesBindingTTL).Format(time.RFC3339),
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
		defer cancel()
		if serr := a.Store.SaveResponseBinding(ctx, b); serr != nil && a.Log != nil {
			a.Log.Error("save response binding failed", "request_id", requestID, "err", serr)
		}
	}
	return meta, nil
}

// recordResponsesUsage /v1/responses 用量落库（独立收尾 context；不保存正文）。
func (a *App) recordResponsesUsage(requestID, model, accountID string, meta *responses.Meta, latencyMS int64, errCode errs.Code) {
	ev := repository.UsageEvent{
		ID:         "ue_" + shortID(),
		RequestID:  requestID,
		AccountID:  accountID,
		ModelID:    model,
		StatusCode: 200,
		LatencyMS:  latencyMS,
		ErrorClass: string(errCode),
	}
	if errCode != "" {
		ev.StatusCode = errs.New(errCode, "").HTTPStatus()
	}
	if meta != nil && meta.HasUsage {
		ev.InputTokens = meta.Usage.InputTokens
		ev.OutputTokens = meta.Usage.OutputTokens
		ev.CacheTokens = meta.Usage.CachedTokens
		ev.ReasoningTokens = meta.Usage.ReasoningTokens
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.Store.RecordUsageEvent(ctx, ev); err != nil && a.Log != nil {
		a.Log.Error("usage event write failed", "request_id", requestID, "err", err)
	}
}

// rewriteResponsesModel 仅改写 JSON 的 model 字段，其余字段保真。
func rewriteResponsesModel(body []byte, model string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	mb, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	m["model"] = mb
	return json.Marshal(m)
}

// responsesRetryable 安全切换判定：429/5xx/网络错误可换候选；
// 其余 4xx（鉴权/参数）不切换。
func responsesRetryable(err error) bool {
	if err == nil {
		return false
	}
	var se *responses.StatusError
	if errors.As(err, &se) {
		return se.RateLimited() || se.Status >= 500
	}
	msg := err.Error()
	for _, kw := range []string{"connection", "timeout", "EOF", "refused", "broken pipe"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// ============================================================
// OAuth 管理 API（M3 接线）
// ============================================================

// oauthFlow 一次进行中的授权会话（服务端内存态；重启即失效，需重新发起）。
type oauthFlow struct {
	mu         sync.Mutex
	provider   string
	state      string
	name       string
	providerID string
	mode       string
	status     string // pending | completed | failed
	accountID  string
	err        string
}

// oauthFlows 管理进行中的授权流。
type oauthFlows struct {
	mu    sync.Mutex
	flows map[string]*oauthFlow
}

func newOAuthFlows() *oauthFlows { return &oauthFlows{flows: map[string]*oauthFlow{}} }

func (f *oauthFlows) add(fl *oauthFlow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flows[fl.state] = fl
}

func (f *oauthFlows) get(state string) (*oauthFlow, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fl, ok := f.flows[state]
	return fl, ok
}

// oauthAction 路由 /api/v1/oauth/{provider}/{start|complete} 与
// /api/v1/oauth/{provider}/flows/{state}。
func (a *App) oauthAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 5 {
		switch parts[4] {
		case "start":
			a.oauthStart(w, r, parts[3])
			return
		case "complete":
			a.oauthComplete(w, r, parts[3])
			return
		}
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "不支持的动作: "+parts[4]))
		return
	}
	if len(parts) == 6 && parts[4] == "flows" {
		a.oauthFlowStatus(w, r, parts[3], parts[5])
		return
	}
	httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "路径无效"))
}

// oauthStart 发起授权：返回浏览器打开的授权 URL；同时尝试在本机 1455 端口
// 监听回调（Codex RedirectURI 约定）。端口被占时仍返回 URL，由用户手动把
// 回调地址粘贴到 complete。
func (a *App) oauthStart(w http.ResponseWriter, r *http.Request, provider string) {
	if r.Method != http.MethodPost {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
		return
	}
	svc, ok := a.OAuth[provider]
	if !ok {
		httpserver.WriteError(w, errs.New(errs.CodeNotFound, "平台未注册 OAuth: "+provider))
		return
	}
	var in struct {
		Name       string `json:"name"`
		ProviderID string `json:"provider_id"`
		Mode       string `json:"mode"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in)
	if in.Mode == "" {
		in.Mode = string(domain.ModeRelayAndMonitor)
	}
	if in.Mode != string(domain.ModeMonitorOnly) && in.Mode != string(domain.ModeRelayAndMonitor) {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "mode 仅支持 monitor_only / relay_and_monitor"))
		return
	}
	providerID := in.ProviderID
	if providerID == "" {
		var err error
		providerID, _, err = a.ensureProvider(r.Context(), provider)
		if err != nil {
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "准备 Provider 失败", err))
			return
		}
	} else {
		p, err := a.Store.GetProvider(r.Context(), providerID)
		if err != nil {
			httpserver.WriteError(w, errs.New(errs.CodeNotFound, "Provider 不存在"))
			return
		}
		if p.Kind != provider {
			httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "Provider kind 与授权平台不符"))
			return
		}
	}
	flowID := "flow_" + shortID()
	authURL, err := svc.StartAuth("oauth-"+provider, flowID)
	if err != nil {
		if errors.Is(err, oauth.ErrVaultNotPersistent) {
			httpserver.WriteError(w, errs.New(errs.CodeForbidden, "凭据库非持久（内存模式），不能保存长期授权；请配置 Keychain"))
			return
		}
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "发起授权失败", err))
		return
	}
	fl := &oauthFlow{
		provider: provider, state: extractStateParam(authURL), name: in.Name,
		providerID: providerID, mode: in.Mode, status: "pending",
	}
	a.flows.add(fl)
	_ = a.Store.RecordAuditEvent(r.Context(), "admin", "oauth.start", provider, "")

	listenerStarted := false
	if ln, lerr := net.Listen("tcp", "127.0.0.1:1455"); lerr == nil {
		listenerStarted = true
		go func() {
			cb, cerr := oauth.AwaitCallback(context.Background(), ln, 5*time.Minute)
			if cerr != nil {
				return // 超时/监听异常：等待手动 complete
			}
			a.completeFlowAccount(fl, cb.Code, cb.State, cb.Error)
		}()
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{
		"auth_url": authURL, "flow_id": flowID, "callback_listener": listenerStarted,
		"hint": "在浏览器完成授权；若未自动回调，可将回调地址粘贴到 complete 接口",
	})
}

// oauthComplete 手动完成授权（用户粘贴回调 URL）。
func (a *App) oauthComplete(w http.ResponseWriter, r *http.Request, provider string) {
	if r.Method != http.MethodPost {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
		return
	}
	svc, ok := a.OAuth[provider]
	if !ok {
		httpserver.WriteError(w, errs.New(errs.CodeNotFound, "平台未注册 OAuth: "+provider))
		return
	}
	var in struct {
		CallbackURL string `json:"callback_url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || strings.TrimSpace(in.CallbackURL) == "" {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "缺少 callback_url"))
		return
	}
	cb, err := oauth.ExtractCallback(in.CallbackURL)
	if err != nil {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "回调地址无法解析"))
		return
	}
	fl, ok := a.flows.get(cb.State)
	if !ok || fl.provider != provider {
		httpserver.WriteError(w, errs.New(errs.CodeNotFound, "授权流不存在或已过期，请重新发起"))
		return
	}
	acc, cerr := a.completeFlowAccount(fl, cb.Code, cb.State, cb.Error)
	if cerr != nil {
		_ = cerr // 状态已写入流
		fl.mu.Lock()
		st, ferr := fl.status, fl.err
		fl.mu.Unlock()
		httpserver.WriteError(w, errs.New(errs.CodeUpstreamAuth, "授权未完成: "+stableOAuthErrStr(st, ferr)))
		return
	}
	httpserver.WriteJSON(w, http.StatusCreated, map[string]any{
		"id": acc.ID, "provider_id": acc.ProviderID, "auth_type": acc.AuthType,
		"secret_fingerprint": acc.SecretFingerprint,
	})
	_ = svc
}

// completeFlowAccount 统一的完成路径：交换令牌 → 写 Vault → 创建 OAuth 账户，
// 更新流状态。后台回调与手动 complete 共用；并发到达时先到先得。
func (a *App) completeFlowAccount(fl *oauthFlow, code, state, cbErr string) (domain.Account, error) {
	svc, ok := a.OAuth[fl.provider]
	if !ok {
		return domain.Account{}, errors.New("oauth: provider not registered")
	}
	ref, _, err := svc.CompleteAuth(context.Background(), state, code, cbErr)
	fl.mu.Lock()
	if fl.status != "pending" {
		defer fl.mu.Unlock()
		return domain.Account{}, nil // 另一路径已完成
	}
	if err != nil {
		fl.status, fl.err = "failed", stableOAuthErr(err)
		fl.mu.Unlock()
		return domain.Account{}, err
	}
	fl.mu.Unlock()

	prov, err := a.Store.GetProvider(context.Background(), fl.providerID)
	if err != nil {
		fl.mu.Lock()
		fl.status, fl.err = "failed", "provider 不存在"
		fl.mu.Unlock()
		return domain.Account{}, err
	}
	accountID := "acc_" + shortID()
	name := fl.name
	if name == "" {
		name = fl.provider + "-" + accountID[:8]
	}
	acc := domain.Account{
		ID: accountID, ProviderID: prov.ID, Name: name, Status: "active",
		Mode: fl.mode, AuthType: string(domain.AuthOAuth),
		BillingMode: string(domain.BillingUnknown), AuthState: string(domain.AuthStateOK),
		VaultProvider: ref.VaultProvider, SecretService: ref.Service, SecretAccount: ref.Account,
		SecretFingerprint: ref.Fingerprint,
	}
	if err := a.Store.CreateAccount(context.Background(), acc); err != nil {
		_ = a.Vault.Delete(ref)
		fl.mu.Lock()
		fl.status, fl.err = "failed", "账户创建失败"
		fl.mu.Unlock()
		return domain.Account{}, err
	}
	fl.mu.Lock()
	fl.status, fl.accountID = "completed", accountID
	fl.mu.Unlock()
	_ = a.Store.RecordAuditEvent(context.Background(), "admin", "oauth.complete", accountID, fl.provider)
	return acc, nil
}

// oauthFlowStatus 查询授权流状态。
func (a *App) oauthFlowStatus(w http.ResponseWriter, _ *http.Request, provider, state string) {
	fl, ok := a.flows.get(state)
	if !ok || fl.provider != provider {
		httpserver.WriteError(w, errs.New(errs.CodeNotFound, "授权流不存在"))
		return
	}
	fl.mu.Lock()
	defer fl.mu.Unlock()
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{
		"status": fl.status, "account_id": fl.accountID, "error": fl.err,
	})
}

// ensureProvider 确保 kind=kind 的平台存在，返回 (id, 是否新建, err)。
func (a *App) ensureProvider(ctx context.Context, kind string) (string, bool, error) {
	provs, err := a.Store.ListProviders(ctx)
	if err != nil {
		return "", false, err
	}
	for _, p := range provs {
		if p.Kind == kind {
			return p.ID, false, nil
		}
	}
	id := "prov_" + shortID()
	if err := a.Store.CreateProvider(ctx, domain.Provider{
		ID: id, Kind: kind, Name: kind, BaseURL: defaultBaseURL(kind),
	}); err != nil {
		return "", false, err
	}
	return id, true, nil
}

// extractStateParam 从授权 URL 提取 state（不做完整解析，避免依赖 URL 形态）。
func extractStateParam(authURL string) string {
	if i := strings.Index(authURL, "state="); i >= 0 {
		s := authURL[i+len("state="):]
		if amp := strings.IndexAny(s, "&#"); amp >= 0 {
			s = s[:amp]
		}
		return s
	}
	return ""
}

// ============================================================
// 账户撤销（本地断开）
// ============================================================

// revokeAccount 仅本地断开：删除 Vault 凭据并把账户标记为需重新授权。
// Codex 无已验证的官方撤销端点（provider-capabilities §2.1），显式不做远端撤销。
func (a *App) revokeAccount(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
		return
	}
	acc, err := a.Store.GetAccount(r.Context(), accountID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpserver.WriteError(w, errs.New(errs.CodeNotFound, "账户不存在"))
			return
		}
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询账户失败", err))
		return
	}
	if derr := a.Vault.Delete(credentials.SecretRef{
		VaultProvider: acc.VaultProvider, Service: acc.SecretService, Account: acc.SecretAccount,
	}); derr != nil && !errors.Is(derr, credentials.ErrNotFound) {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "凭据清理失败，可重试", derr))
		return
	}
	_ = a.Store.SetAccountAuthState(r.Context(), accountID, string(domain.AuthStateReauthRequired))
	_ = a.Store.RecordAuditEvent(r.Context(), "admin", "account.revoke", accountID, "local-disconnect")
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{
		"ok": true, "remote_revoked": false,
		"note": "平台无已验证的撤销端点，已执行本地断开；重新使用需再次授权",
	})
}

// stableOAuthErr 生成不含令牌/原始响应的稳定错误描述。
func stableOAuthErr(err error) string {
	switch {
	case errors.Is(err, oauth.ErrStateInvalid):
		return "state 无效或已消费"
	case errors.Is(err, oauth.ErrStateExpired):
		return "授权会话已过期"
	case errors.Is(err, oauth.ErrAuthorizationCancelled):
		return "用户取消了授权"
	case errors.Is(err, oauth.ErrRefreshRejected):
		return "平台拒绝了刷新令牌"
	default:
		return "授权失败"
	}
}

func stableOAuthErrStr(status, detail string) string {
	if detail != "" {
		return detail
	}
	return status
}

// ============================================================
// projects CRUD（MR-016）
// ============================================================

func (a *App) handleProjects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := a.Store.ListProjects(r.Context())
		if err != nil {
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询项目失败", err))
			return
		}
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"data": list})
	case http.MethodPost:
		var in struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || strings.TrimSpace(in.Name) == "" {
			httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "缺少 name"))
			return
		}
		if in.ID == "" {
			in.ID = "proj_" + shortID()
		}
		if err := a.Store.CreateProject(r.Context(), domain.Project{ID: in.ID, Name: strings.TrimSpace(in.Name)}); err != nil {
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "创建项目失败", err))
			return
		}
		_ = a.Store.RecordAuditEvent(r.Context(), "admin", "project.create", in.ID, "")
		httpserver.WriteJSON(w, http.StatusCreated, map[string]any{"id": in.ID})
	default:
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
	}
}

// projectAction /api/v1/projects/{id} 删除（有令牌引用时返回冲突）。
func (a *App) projectAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 4 || r.Method != http.MethodDelete {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
		return
	}
	id := parts[3]
	toks, err := a.Store.ListTokens(r.Context())
	if err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询令牌失败", err))
		return
	}
	for _, t := range toks {
		if t.ProjectID == id {
			httpserver.WriteError(w, errs.New(errs.CodeConflict, "项目仍有令牌引用，请先删除或迁移令牌"))
			return
		}
	}
	if err := a.Store.DeleteProject(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpserver.WriteError(w, errs.New(errs.CodeNotFound, "项目不存在"))
			return
		}
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "删除项目失败", err))
		return
	}
	_ = a.Store.RecordAuditEvent(r.Context(), "admin", "project.delete", id, "")
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}
