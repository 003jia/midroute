// Package api 提供管理 API 与 OpenAI-compatible 网关 API。
// 对齐 PRD US-001/003/006/007：单服务、Provider/Account 管理、统一 /v1 转发。
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"midroute/internal/access"
	"midroute/internal/accounts"
	"midroute/internal/connectors"
	"midroute/internal/connectors/oauth"
	"midroute/internal/credentials"
	"midroute/internal/domain"
	"midroute/internal/errs"
	"midroute/internal/httpserver"
	"midroute/internal/quota"
	"midroute/internal/repository"
	"midroute/internal/router"
	"midroute/internal/scheduler"
)

// App 聚合依赖。
type App struct {
	Store  *repository.Store
	Vault  credentials.Vault
	Router *router.Router
	Log    *slog.Logger
	// Access 推理令牌鉴权（MR-016）；nil 时 /v1/* 网关全部拒绝。
	Access *access.Service
	// OAuth 按平台名注册的 OAuth 服务（MR-007/M3 接线）。
	OAuth map[string]*oauth.Service
	// Caps 账户能力检查器（MR-006）。
	Caps *accounts.CapabilityChecker
	// QuotaSvc 额度快照服务（MR-009）。
	QuotaSvc *quota.Service
	// Sched 后台刷新调度（MR-010）。
	Sched *scheduler.Scheduler
	// httpClient 推理转发使用的共享客户端（带超时/连接限制的由 main 注入）。
	httpClient *http.Client
	flows      *oauthFlows
	now        func() string
}

// NewApp 创建 App。vault 由调用方注入（Keychain 或内存）。
func NewApp(store *repository.Store, v credentials.Vault, r *router.Router, log *slog.Logger) *App {
	app := &App{
		Store: store, Vault: v, Router: r, Log: log,
		Access:     access.New(store),
		OAuth:      map[string]*oauth.Service{},
		httpClient: &http.Client{},
		flows:      newOAuthFlows(),
		now:        func() string { return time.Now().UTC().Format(time.RFC3339) },
	}
	app.Caps = accounts.New(store, func(ctx context.Context, acc domain.Account) (*connectors.Target, error) {
		return app.resolveTarget(ctx, acc, "")
	})
	app.QuotaSvc = quota.New(store)
	app.Sched = scheduler.New(store, log)
	return app
}

// SetHTTPClient 注入共享 HTTP 客户端（测试与生产各自配置）。
func (a *App) SetHTTPClient(c *http.Client) { a.httpClient = c }

// RegisterOAuth 注册一个平台的 OAuth 服务（main 装配或测试注入假端点）。
func (a *App) RegisterOAuth(provider string, svc *oauth.Service) { a.OAuth[provider] = svc }

// Mount 在基础服务上挂载管理 API 与网关 API。
func (a *App) Mount(srv *httpserver.Server) {
	// 管理 API
	srv.MountFunc("/api/v1/providers", a.handleProviders)
	srv.MountFunc("/api/v1/providers/", a.providerAction)
	srv.MountFunc("/api/v1/accounts", a.handleAccounts)
	srv.MountFunc("/api/v1/accounts/", a.accountAction)
	srv.MountFunc("/api/v1/models", a.handleModels)
	srv.MountFunc("/api/v1/tokens", a.handleTokens)
	srv.MountFunc("/api/v1/tokens/", a.tokenAction)
	srv.MountFunc("/api/v1/projects", a.handleProjects)
	srv.MountFunc("/api/v1/projects/", a.projectAction)
	srv.MountFunc("/api/v1/routing-policies", a.handleRoutingPolicies)
	srv.MountFunc("/api/v1/routing-policies/", a.handleRoutingPolicyByAlias)
	srv.MountFunc("/api/v1/quota-pools", a.handleQuotaPools)
	srv.MountFunc("/api/v1/quota-pools/", a.handleQuotaPoolSnapshots)
	srv.MountFunc("/api/v1/jobs", a.handleJobs)
	srv.MountFunc("/api/v1/jobs/", a.handleJob)
	srv.MountFunc("/api/v1/requests", a.handleRequests)
	srv.MountFunc("/api/v1/requests/", a.handleRequestDetail)
	srv.MountFunc("/api/v1/oauth/", a.oauthAction)
	// 网关（推理）API：项目令牌鉴权（httpserver 对 /v1/* 不施加管理 Guard）
	srv.MountFunc("/v1/models", a.requireToken(a.handleGatewayModels))
	srv.MountFunc("/v1/chat/completions", a.requireToken(a.handleChatCompletions))
	srv.MountFunc("/v1/responses", a.requireToken(a.handleResponses))
}

// runDiscover 执行账户模型发现并落库（调度任务用；错误由调用方处理）。
func (a *App) RunDiscover(ctx context.Context, acc domain.Account) error {
	target, err := a.resolveTarget(ctx, acc, "")
	if err != nil {
		return err
	}
	conn := connectors.NewConnector(target.ProviderKind, connectors.Options{})
	infos, err := conn.DiscoverModels(ctx, *target)
	if err != nil {
		return err
	}
	models := make([]domain.Model, 0, len(infos))
	rows := make([]repository.ModelInfoRow, 0, len(infos))
	for _, mi := range infos {
		models = append(models, domain.Model{ID: provModelID(acc.ProviderID, mi.UpstreamID), ProviderID: acc.ProviderID, UpstreamID: mi.UpstreamID, ContextLimit: mi.ContextLimit})
		rows = append(rows, repository.ModelInfoRow{ModelID: provModelID(acc.ProviderID, mi.UpstreamID), Listed: true, Usable: true})
	}
	if err := a.Store.UpsertModels(ctx, models); err != nil {
		return err
	}
	return a.Store.SaveAccountModels(ctx, acc.ID, rows)
}

// ResolveForRouter 实现 router.Resolver：账户 → 可执行目标。
func (a *App) ResolveForRouter(ctx context.Context, accountID string) (*router.ResolvedTarget, error) {
	acc, err := a.Store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	t, err := a.resolveTarget(ctx, acc, "")
	if err != nil {
		return nil, err
	}
	return &router.ResolvedTarget{
		AccountID: acc.ID,
		ModelID:   t.ModelID,
		Target:    *t,
	}, nil
}

// defaultBaseURL 平台默认地址。
func defaultBaseURL(kind string) string {
	switch kind {
	case "anthropic":
		return "https://api.anthropic.com"
	case "gemini":
		return "https://generativelanguage.googleapis.com"
	case "codex":
		// Responses 端点证据见 docs/provider-capabilities.md §2.2
		return "https://chatgpt.com/backend-api/codex"
	default:
		return "https://api.openai.com"
	}
}

// -------- providers --------

func (a *App) handleProviders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := a.Store.ListProviders(r.Context())
		if err != nil {
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询 Provider 失败", err))
			return
		}
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"data": list})
	case http.MethodPost:
		var in struct {
			ID      string `json:"id"`
			Kind    string `json:"kind"`
			Name    string `json:"name"`
			BaseURL string `json:"base_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "请求体无效"))
			return
		}
		switch in.Kind {
		case "openai", "anthropic", "gemini", "openai-compatible", "codex":
		default:
			httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "不支持的 kind: "+in.Kind))
			return
		}
		if in.ID == "" {
			in.ID = "prov_" + shortID()
		}
		if in.Name == "" {
			in.Name = in.Kind
		}
		if in.BaseURL == "" {
			in.BaseURL = defaultBaseURL(in.Kind)
		}
		if err := a.Store.CreateProvider(r.Context(), domain.Provider{
			ID: in.ID, Kind: in.Kind, Name: in.Name, BaseURL: in.BaseURL,
		}); err != nil {
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "创建 Provider 失败", err))
			return
		}
		httpserver.WriteJSON(w, http.StatusCreated, map[string]any{"id": in.ID})
	default:
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
	}
}

// -------- accounts --------

func (a *App) handleAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := a.Store.ListAccounts(r.Context())
		if err != nil {
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询账户失败", err))
			return
		}
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"data": list})
	case http.MethodPost:
		a.createAccount(w, r)
	default:
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
	}
}

func (a *App) createAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProviderID string `json:"provider_id"`
		Name       string `json:"name"`
		APIKey     string `json:"api_key"` // 仅请求体内出现；存储走 SecretRef
		Mode       string `json:"mode"`    // monitor_only | relay_and_monitor，默认 relay_and_monitor
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "请求体无效"))
		return
	}
	if in.ProviderID == "" || in.APIKey == "" {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "缺少 provider_id 或 api_key"))
		return
	}
	if in.Mode == "" {
		in.Mode = string(domain.ModeRelayAndMonitor)
	}
	if in.Mode != string(domain.ModeMonitorOnly) && in.Mode != string(domain.ModeRelayAndMonitor) {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "mode 仅支持 monitor_only / relay_and_monitor"))
		return
	}
	prov, err := a.Store.GetProvider(r.Context(), in.ProviderID)
	if err != nil {
		if err == sql.ErrNoRows {
			httpserver.WriteError(w, errs.New(errs.CodeNotFound, "Provider 不存在"))
			return
		}
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询 Provider 失败", err))
		return
	}
	// 密钥写入 Vault，仅保存 SecretRef
	service := "account-" + in.ProviderID
	accountID := "acc_" + shortID()
	ref, err := a.Vault.Store(service, accountID, []byte(in.APIKey))
	if err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "凭据入库失败", err))
		return
	}
	name := in.Name
	if name == "" {
		name = prov.Kind + "-" + accountID[:8]
	}
	acc := domain.Account{
		ID: accountID, ProviderID: prov.ID, Name: name, Status: "active",
		Mode: in.Mode, AuthType: string(domain.AuthAPIKey),
		BillingMode: string(domain.BillingUnknown), AuthState: string(domain.AuthStateUnknown),
		VaultProvider: ref.VaultProvider, SecretService: ref.Service, SecretAccount: ref.Account,
		SecretFingerprint: ref.Fingerprint,
	}
	if err := a.Store.CreateAccount(r.Context(), acc); err != nil {
		_ = a.Vault.Delete(ref)
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "创建账户失败", err))
		return
	}
	httpserver.WriteJSON(w, http.StatusCreated, map[string]any{"id": accountID, "secret_fingerprint": ref.Fingerprint})
}

// accountAction 处理 /api/v1/accounts/{id}/{action} 与 /api/v1/accounts/{id}（PATCH/DELETE，MR-004）。
func (a *App) accountAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 4:
		// [api v1 accounts <id>]
		switch r.Method {
		case http.MethodGet:
			acc, err := a.Store.GetAccount(r.Context(), parts[3])
			if err != nil {
				if err == sql.ErrNoRows {
					httpserver.WriteError(w, errs.New(errs.CodeNotFound, "账户不存在"))
					return
				}
				httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询账户失败", err))
				return
			}
			httpserver.WriteJSON(w, http.StatusOK, acc)
		case http.MethodPatch, http.MethodPut:
			a.updateAccount(w, r, parts[3])
		case http.MethodDelete:
			a.deleteAccount(w, r, parts[3])
		default:
			httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
		}
	case len(parts) == 5:
		// [api v1 accounts <id> <action>]
		a.accountSubAction(w, r, parts[3], parts[4])
	default:
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "路径无效"))
	}
}

// updateAccount 更新账户（编辑名称/启停；MR-004）。
func (a *App) updateAccount(w http.ResponseWriter, r *http.Request, accountID string) {
	var in struct {
		Name   *string `json:"name,omitempty"`
		Status *string `json:"status,omitempty"` // active | disabled | error
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "请求体无效"))
		return
	}
	acc, err := a.Store.GetAccount(r.Context(), accountID)
	if err != nil {
		if err == sql.ErrNoRows {
			httpserver.WriteError(w, errs.New(errs.CodeNotFound, "账户不存在"))
			return
		}
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询账户失败", err))
		return
	}
	if in.Status != nil {
		switch *in.Status {
		case "active", "disabled", "error":
		default:
			httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "非法状态: "+*in.Status))
			return
		}
		if err := a.Store.SetAccountStatus(r.Context(), accountID, *in.Status); err != nil {
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "更新状态失败", err))
			return
		}
		_ = a.Store.RecordAuditEvent(r.Context(), "admin", "account.status", accountID, *in.Status)
	}
	if in.Name != nil && *in.Name != "" && *in.Name != acc.Name {
		if err := a.Store.SetAccountName(r.Context(), accountID, *in.Name); err != nil {
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "更新名称失败", err))
			return
		}
		_ = a.Store.RecordAuditEvent(r.Context(), "admin", "account.rename", accountID, "")
	}
	updated, _ := a.Store.GetAccount(r.Context(), accountID)
	httpserver.WriteJSON(w, http.StatusOK, updated)
}

// deleteAccount 删除账户：被路由策略或历史用量引用时返回冲突（MR-004）。
func (a *App) deleteAccount(w http.ResponseWriter, r *http.Request, accountID string) {
	if _, err := a.Store.GetAccount(r.Context(), accountID); err != nil {
		if err == sql.ErrNoRows {
			httpserver.WriteError(w, errs.New(errs.CodeNotFound, "账户不存在"))
			return
		}
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询账户失败", err))
		return
	}
	if n, _ := a.Store.CountRoutingCandidatesByAccount(r.Context(), accountID); n > 0 {
		httpserver.WriteError(w, errs.New(errs.CodeConflict, "账户仍被路由策略引用，请先移除候选"))
		return
	}
	if n, _ := a.Store.CountUsageEventsByAccount(r.Context(), accountID); n > 0 {
		httpserver.WriteError(w, errs.New(errs.CodeConflict, "账户存在历史用量记录，仅可禁用不可删除"))
		return
	}
	if err := a.Store.DeleteAccount(r.Context(), accountID); err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "删除账户失败", err))
		return
	}
	_ = a.Store.RecordAuditEvent(r.Context(), "admin", "account.delete", accountID, "")
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountSubAction 处理账户子动作。
func (a *App) accountSubAction(w http.ResponseWriter, r *http.Request, accountID, action string) {
	acc, err := a.Store.GetAccount(r.Context(), accountID)
	if err != nil {
		if err == sql.ErrNoRows {
			httpserver.WriteError(w, errs.New(errs.CodeNotFound, "账户不存在"))
			return
		}
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询账户失败", err))
		return
	}
	if action == "revoke" {
		// 本地断开不需要解析转发目标；OAuth 账户专用
		if acc.AuthType != string(domain.AuthOAuth) {
			httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "revoke 仅适用于 OAuth 账户"))
			return
		}
		a.revokeAccount(w, r, accountID)
		return
	}
	// capabilities / credential 不需要在线目标（credential 内部用新凭据自行解析）
	switch action {
	case "capabilities":
		caps, err := a.Caps.Check(r.Context(), acc)
		if err != nil {
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "能力检查失败", err))
			return
		}
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"data": caps})
		return
	case "credential":
		if r.Method != http.MethodPost {
			httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "请用 POST"))
			return
		}
		a.rotateCredential(w, r, accountID, acc)
		return
	case "quota":
		if r.Method != http.MethodGet {
			httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "请用 GET"))
			return
		}
		a.accountQuota(w, r, accountID)
		return
	case "quota-manual":
		if r.Method != http.MethodPut && r.Method != http.MethodPost {
			httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "请用 PUT/POST"))
			return
		}
		a.accountManualQuota(w, r, accountID)
		return
	case "refresh":
		if r.Method != http.MethodPost {
			httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "请用 POST"))
			return
		}
		a.refreshAccount(w, r, accountID)
		return
	case "verify", "discover":
	default:
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "不支持的动作: "+action))
		return
	}
	target, err := a.resolveTarget(r.Context(), acc, "")
	if err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "解析目标失败", err))
		return
	}
	conn := connectors.NewConnector(target.ProviderKind, connectors.Options{})
	switch action {
	case "verify":
		if err := conn.ValidateCredential(r.Context(), *target); err != nil {
			_ = a.Store.SetAccountStatus(r.Context(), accountID, "error")
			httpserver.WriteError(w, errs.Wrap(errs.CodeUpstreamAuth, "凭据校验失败", err))
			return
		}
		at := a.now()
		_ = a.Store.SetAccountStatus(r.Context(), accountID, "active")
		_ = a.Store.SetAccountVerifiedAt(r.Context(), accountID, at)
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "verified_at": at})
	case "discover":
		infos, err := conn.DiscoverModels(r.Context(), *target)
		if err != nil {
			httpserver.WriteError(w, errs.Wrap(errs.CodeUpstreamError, "模型发现失败", err))
			return
		}
		models := make([]domain.Model, 0, len(infos))
		rows := make([]repository.ModelInfoRow, 0, len(infos))
		for _, mi := range infos {
			models = append(models, domain.Model{ID: provModelID(acc.ProviderID, mi.UpstreamID), ProviderID: acc.ProviderID, UpstreamID: mi.UpstreamID, ContextLimit: mi.ContextLimit})
			rows = append(rows, repository.ModelInfoRow{ModelID: provModelID(acc.ProviderID, mi.UpstreamID), Listed: true, Usable: true})
		}
		if err := a.Store.UpsertModels(r.Context(), models); err != nil {
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "保存模型失败", err))
			return
		}
		if err := a.Store.SaveAccountModels(r.Context(), accountID, rows); err != nil {
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "保存账户模型失败", err))
			return
		}
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(infos)})
	}
}

// rotateCredential 凭据轮换（MR-004）：先存新 SecretRef，验证通过后再原子切换；
// 验证失败保留旧凭据，绝不覆盖。
func (a *App) rotateCredential(w http.ResponseWriter, r *http.Request, accountID string, acc domain.Account) {
	var in struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.APIKey) == "" {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "缺少 api_key"))
		return
	}
	prov, err := a.Store.GetProvider(r.Context(), acc.ProviderID)
	if err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询 Provider 失败", err))
		return
	}
	// 1) 新密钥先入 Vault，取得新 SecretRef
	service := "account-" + acc.ProviderID
	newRef, err := a.Vault.Store(service, accountID+"-rot", []byte(strings.TrimSpace(in.APIKey)))
	if err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "凭据入库失败", err))
		return
	}
	// 2) 用新凭据解析目标并验证
	target, err := a.resolveTargetFromRef(r.Context(), prov, newRef)
	if err != nil {
		_ = a.Vault.Delete(newRef)
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "解析新凭据失败", err))
		return
	}
	conn := connectors.NewConnector(target.ProviderKind, connectors.Options{})
	if err := conn.ValidateCredential(r.Context(), *target); err != nil {
		_ = a.Vault.Delete(newRef) // 验证失败：删除新引用，保留旧凭据
		_ = a.Store.RecordAuditEvent(r.Context(), "admin", "account.credential.rotate.failed", accountID, "")
		httpserver.WriteError(w, errs.Wrap(errs.CodeUpstreamAuth, "新凭据验证失败，已保留原凭据", err))
		return
	}
	// 3) 验证成功：原子切换 SecretRef
	if err := a.Store.SetAccountCredentialRef(r.Context(), accountID, newRef.VaultProvider, newRef.Service, newRef.Account, newRef.Fingerprint); err != nil {
		_ = a.Vault.Delete(newRef)
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "切换凭据失败", err))
		return
	}
	at := a.now()
	_ = a.Store.SetAccountStatus(r.Context(), accountID, "active")
	_ = a.Store.SetAccountVerifiedAt(r.Context(), accountID, at)
	_ = a.Store.RecordAuditEvent(r.Context(), "admin", "account.credential.rotate", accountID, "")
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "secret_fingerprint": newRef.Fingerprint, "verified_at": at})
}

// resolveTargetFromRef 使用指定 SecretRef 解析目标（凭据轮换用）。
func (a *App) resolveTargetFromRef(ctx context.Context, prov domain.Provider, ref credentials.SecretRef) (*connectors.Target, error) {
	sec, err := a.Vault.Get(ref)
	if err != nil {
		return nil, err
	}
	defer sec.Zero()
	base := prov.BaseURL
	if base == "" {
		base = defaultBaseURL(prov.Kind)
	}
	return &connectors.Target{
		ProviderKind: connectors.ProviderKind(prov.Kind),
		BaseURL:      base,
		APIKey:       string(sec.Value),
	}, nil
}

// -------- 刷新任务（MR-010）--------

// refreshAccount 触发账户的指定能力刷新（去重，异步 202 + job_id）。
func (a *App) refreshAccount(w http.ResponseWriter, r *http.Request, accountID string) {
	if _, err := a.Store.GetAccount(r.Context(), accountID); err != nil {
		if err == sql.ErrNoRows {
			httpserver.WriteError(w, errs.New(errs.CodeNotFound, "账户不存在"))
			return
		}
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询账户失败", err))
		return
	}
	var in struct {
		Capability string `json:"capability"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Capability == "" {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "缺少 capability"))
		return
	}
	if a.Sched == nil {
		httpserver.WriteError(w, errs.New(errs.CodeInternal, "调度器未装配"))
		return
	}
	job, err := a.Sched.Enqueue(r.Context(), accountID, in.Capability)
	if err != nil {
		if errors.Is(err, scheduler.ErrBusy) {
			if job != nil {
				httpserver.WriteJSON(w, http.StatusOK, map[string]any{"job_id": job.ID, "busy": true})
				return
			}
			httpserver.WriteJSON(w, http.StatusAccepted, map[string]any{"busy": true})
			return
		}
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "创建刷新任务失败", err))
		return
	}
	httpserver.WriteJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID, "state": job.State})
}

// handleJob 查询任务状态。
func (a *App) handleJob(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// [api v1 jobs <id>]
	if len(parts) != 4 {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "路径无效"))
		return
	}
	job, err := a.Store.GetRefreshJob(r.Context(), parts[3])
	if err != nil {
		if err == sql.ErrNoRows {
			httpserver.WriteError(w, errs.New(errs.CodeNotFound, "任务不存在"))
			return
		}
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询任务失败", err))
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, job)
}

// handleJobs 列出任务（可选 ?account_id=）。
func (a *App) handleJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
		return
	}
	accountID := r.URL.Query().Get("account_id")
	jobs, err := a.Store.ListRefreshJobs(r.Context(), accountID)
	if err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询任务失败", err))
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"data": jobs})
}

// -------- providers 生命周期（MR-004）--------

func (a *App) providerAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// [api v1 providers <id>]
	if len(parts) != 4 {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "路径无效"))
		return
	}
	id := parts[3]
	switch r.Method {
	case http.MethodGet:
		p, err := a.Store.GetProvider(r.Context(), id)
		if err != nil {
			if err == sql.ErrNoRows {
				httpserver.WriteError(w, errs.New(errs.CodeNotFound, "Provider 不存在"))
				return
			}
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询 Provider 失败", err))
			return
		}
		httpserver.WriteJSON(w, http.StatusOK, p)
	case http.MethodPatch, http.MethodPut:
		var in struct {
			Name    *string `json:"name,omitempty"`
			BaseURL *string `json:"base_url,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "请求体无效"))
			return
		}
		if err := a.Store.UpdateProvider(r.Context(), id, in.Name, in.BaseURL); err != nil {
			if err == sql.ErrNoRows {
				httpserver.WriteError(w, errs.New(errs.CodeNotFound, "Provider 不存在"))
				return
			}
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "更新 Provider 失败", err))
			return
		}
		_ = a.Store.RecordAuditEvent(r.Context(), "admin", "provider.update", id, "")
		p, _ := a.Store.GetProvider(r.Context(), id)
		httpserver.WriteJSON(w, http.StatusOK, p)
	case http.MethodDelete:
		if n, _ := a.Store.CountAccountsByProvider(r.Context(), id); n > 0 {
			httpserver.WriteError(w, errs.New(errs.CodeConflict, "Provider 仍有账户引用，请先删除或迁移账户"))
			return
		}
		if err := a.Store.DeleteProvider(r.Context(), id); err != nil {
			if err == sql.ErrNoRows {
				httpserver.WriteError(w, errs.New(errs.CodeNotFound, "Provider 不存在"))
				return
			}
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "删除 Provider 失败", err))
			return
		}
		_ = a.Store.RecordAuditEvent(r.Context(), "admin", "provider.delete", id, "")
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
	}
}

// -------- 请求详情（MR-015）--------

// handleRequests 列出近期请求尝试。
func (a *App) handleRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
		return
	}
	attempts, err := a.Store.ListRecentAttempts(r.Context(), 100)
	if err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询请求失败", err))
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"data": attempts})
}

// handleRequestDetail 按 request_id 查全部尝试。
func (a *App) handleRequestDetail(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 4 {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "路径无效"))
		return
	}
	attempts, err := a.Store.ListAttemptsByRequest(r.Context(), parts[3])
	if err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询请求详情失败", err))
		return
	}
	if len(attempts) == 0 {
		httpserver.WriteError(w, errs.New(errs.CodeNotFound, "请求不存在"))
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"request_id": parts[3], "data": attempts})
}

// -------- 额度池与快照（MR-009）--------

func (a *App) handleQuotaPools(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := a.Store.ListQuotaPools(r.Context())
		if err != nil {
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询额度池失败", err))
			return
		}
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"data": list})
	case http.MethodPost:
		var in domain.QuotaPool
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ID == "" || in.ProviderID == "" {
			httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "缺少 id 或 provider_id"))
			return
		}
		if len(in.MemberIDs) == 0 {
			httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "缺少 member_ids"))
			return
		}
		if err := a.Store.SaveQuotaPool(r.Context(), in); err != nil {
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "保存额度池失败", err))
			return
		}
		_ = a.Store.RecordAuditEvent(r.Context(), "admin", "quota_pool.create", in.ID, fmt.Sprintf("members=%d", len(in.MemberIDs)))
		httpserver.WriteJSON(w, http.StatusCreated, map[string]any{"id": in.ID})
	default:
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
	}
}

func (a *App) handleQuotaPoolSnapshots(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// [api v1 quota-pools <id> snapshots]
	if len(parts) != 5 || parts[4] != "snapshots" {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "路径无效"))
		return
	}
	if r.Method != http.MethodGet {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
		return
	}
	snaps, err := a.Store.ListPoolSnapshots(r.Context(), parts[3])
	if err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询快照失败", err))
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"data": snaps})
}

// accountQuota 读取账户额度快照。
func (a *App) accountQuota(w http.ResponseWriter, r *http.Request, accountID string) {
	snaps, err := a.Store.ListQuotaSnapshots(r.Context(), accountID)
	if err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询额度失败", err))
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"data": snaps})
}

// accountManualQuota 手写额度值（PUT 语义：补充而非覆盖官方）。
func (a *App) accountManualQuota(w http.ResponseWriter, r *http.Request, accountID string) {
	var in domain.QuotaSnapshot
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.WindowType == "" {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "缺少 window_type"))
		return
	}
	// 数值缺省必须显式：手工填 0 是 0，未知应传 null
	if in.Used == nil && in.Remaining == nil && in.Limit == nil {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "请至少提供 limit/used/remaining 之一"))
		return
	}
	if a.QuotaSvc == nil {
		httpserver.WriteError(w, errs.New(errs.CodeInternal, "额度服务未装配"))
		return
	}
	snap, err := a.QuotaSvc.ManualSupplement(r.Context(), accountID, "admin", in)
	if err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "写入手工额度失败", err))
		return
	}
	_ = a.Store.RecordAuditEvent(r.Context(), "admin", "quota.manual", accountID, string(snap.WindowType))
	httpserver.WriteJSON(w, http.StatusCreated, snap)
}

// -------- tokens（MR-016 / PRD M2 Token 数据模型）--------

func (a *App) handleTokens(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := a.Store.ListTokens(r.Context())
		if err != nil {
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询令牌失败", err))
			return
		}
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"data": list})
	case http.MethodPost:
		a.createToken(w, r)
	default:
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
	}
}

func (a *App) createToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name           string   `json:"name"`
		ProjectID      string   `json:"project_id"`
		ModelWhitelist []string `json:"model_whitelist"`
		ExpiresAt      string   `json:"expires_at"` // RFC3339，空为长期
		MaxConcurrency int      `json:"max_concurrency"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Name) == "" {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "缺少 name"))
		return
	}
	if in.ProjectID != "" {
		if _, err := a.Store.GetProject(r.Context(), in.ProjectID); err != nil {
			if err == sql.ErrNoRows {
				httpserver.WriteError(w, errs.New(errs.CodeNotFound, "项目不存在"))
				return
			}
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询项目失败", err))
			return
		}
	}
	if in.ExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339, in.ExpiresAt); err != nil {
			httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "expires_at 必须为 RFC3339 时间"))
			return
		}
	}
	if in.MaxConcurrency < 0 {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "max_concurrency 不能为负"))
		return
	}
	whitelist := "[]"
	if len(in.ModelWhitelist) > 0 {
		b, err := json.Marshal(in.ModelWhitelist)
		if err != nil {
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "序列化白名单失败", err))
			return
		}
		whitelist = string(b)
	}
	// 生成明文令牌（仅此一次返回）；库中只存 SHA-256 与前缀
	raw := "mrt_" + randHex(24)
	sum := sha256.Sum256([]byte(raw))
	t := domain.Token{
		ID:             "tok_" + randHex(4),
		Name:           strings.TrimSpace(in.Name),
		KeyHash:        fmt.Sprintf("%x", sum),
		KeyPrefix:      raw[:10],
		Enabled:        true,
		CreatedAt:      a.now(),
		ProjectID:      in.ProjectID,
		ModelWhitelist: whitelist,
		ExpiresAt:      in.ExpiresAt,
		MaxConcurrency: in.MaxConcurrency,
	}
	if err := a.Store.CreateToken(r.Context(), t); err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "创建令牌失败", err))
		return
	}
	_ = a.Store.RecordAuditEvent(r.Context(), "admin", "token.create", t.ID, "")
	httpserver.WriteJSON(w, http.StatusCreated, map[string]any{
		"id": t.ID, "name": t.Name, "key_prefix": t.KeyPrefix, "token": raw,
		"project_id": t.ProjectID, "model_whitelist": in.ModelWhitelist,
		"expires_at": t.ExpiresAt, "max_concurrency": t.MaxConcurrency,
		"note": "令牌明文仅此一次显示，请妥善保存",
	})
}

func (a *App) tokenAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 4 {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "路径无效"))
		return
	}
	id := parts[3]
	switch r.Method {
	case http.MethodPatch, http.MethodPut:
		var in struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Enabled == nil {
			httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "缺少 enabled"))
			return
		}
		if err := a.Store.SetTokenEnabled(r.Context(), id, *in.Enabled); err != nil {
			if err == sql.ErrNoRows {
				httpserver.WriteError(w, errs.New(errs.CodeNotFound, "令牌不存在"))
				return
			}
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "更新令牌失败", err))
			return
		}
		_ = a.Store.RecordAuditEvent(r.Context(), "admin", "token.status", id, fmt.Sprintf("%v", *in.Enabled))
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodDelete:
		if err := a.Store.DeleteToken(r.Context(), id); err != nil {
			if err == sql.ErrNoRows {
				httpserver.WriteError(w, errs.New(errs.CodeNotFound, "令牌不存在"))
				return
			}
			httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "删除令牌失败", err))
			return
		}
		_ = a.Store.RecordAuditEvent(r.Context(), "admin", "token.delete", id, "")
		httpserver.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
	}
}

// -------- models --------

func (a *App) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
		return
	}
	list, err := a.Store.ListModels(r.Context())
	if err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询模型失败", err))
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"data": list})
}

// -------- routing policies --------

func (a *App) handleRoutingPolicies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
		return
	}
	list, err := a.Store.ListRoutingPolicies(r.Context())
	if err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询路由策略失败", err))
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"data": list})
}

func (a *App) handleRoutingPolicyByAlias(w http.ResponseWriter, r *http.Request) {
	alias := strings.TrimPrefix(r.URL.Path, "/api/v1/routing-policies/")
	if alias == "" {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "缺少别名"))
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
		return
	}
	var in struct {
		ID         string             `json:"id"`
		Name       string             `json:"name"`
		Candidates []domain.Candidate `json:"candidates"`
		Enabled    bool               `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "请求体无效"))
		return
	}
	if len(in.Candidates) == 0 {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "缺少 candidates"))
		return
	}
	id := in.ID
	if id == "" {
		id = "pol_" + shortID()
	}
	p := domain.RoutingPolicy{ID: id, Name: in.Name, Alias: alias, Candidates: in.Candidates, Enabled: in.Enabled}
	if err := a.Store.SaveRoutingPolicy(r.Context(), p); err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "保存路由策略失败", err))
		return
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"id": id})
}

// -------- 网关 --------

func (a *App) handleGatewayModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
		return
	}
	list, err := a.Store.ListModels(r.Context())
	if err != nil {
		httpserver.WriteError(w, errs.Wrap(errs.CodeInternal, "查询模型失败", err))
		return
	}
	type m struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	ids := make([]m, 0, len(list))
	for _, mm := range list {
		ids = append(ids, m{ID: mm.UpstreamID, Object: "model", Created: time.Now().Unix(), OwnedBy: mm.ProviderID})
	}
	httpserver.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": ids})
}

func (a *App) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "方法不支持"))
		return
	}
	var req connectors.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "请求体无效"))
		return
	}
	if req.Model == "" || len(req.Messages) == 0 {
		httpserver.WriteError(w, errs.New(errs.CodeInvalidRequest, "缺少 model 或 messages"))
		return
	}
	if !a.checkModelAllowed(w, r, req.Model) {
		return
	}
	requestID := "req_" + shortID()
	if req.Stream {
		a.streamChat(w, r, &req, requestID)
		return
	}
	start := time.Now()
	resp, decision, err := a.Router.Forward(r.Context(), req.Model, &req, requestID)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		a.logError(decision, err)
		a.recordUsage(requestID, req.Model, decision, nil, latency, errs.From(err).Code)
		httpserver.WriteError(w, a.mapRelayError(err))
		return
	}
	a.recordUsage(requestID, req.Model, decision, resp.Usage, latency, "")
	if resp.ID == "" {
		resp.ID = "chatcmpl-" + requestID
	}
	resp.Model = req.Model
	httpserver.WriteJSON(w, http.StatusOK, resp)
}

// recordUsage 记录请求用量元数据（不保存正文；FR-12/FR-13）。
// 使用独立 context：客户端取消不应丢失用量记录；写库失败必须记日志（FR-47）。
func (a *App) recordUsage(requestID, model string, decision *router.Decision, usage *connectors.UsageDTO, latencyMS int64, errCode errs.Code) {
	ev := repository.UsageEvent{
		ID:         "ue_" + shortID(),
		RequestID:  requestID,
		ModelID:    model,
		StatusCode: 200,
		LatencyMS:  latencyMS,
		ErrorClass: string(errCode),
	}
	if errCode != "" {
		ev.StatusCode = errs.New(errCode, "").HTTPStatus()
	}
	if usage != nil {
		ev.InputTokens = usage.PromptTokens
		ev.OutputTokens = usage.CompletionTokens
		if usage.PromptTokensDetails != nil {
			ev.CacheTokens = usage.PromptTokensDetails.CachedTokens
		}
	}
	if decision != nil {
		if i := strings.Index(decision.Selected, "@"); i > 0 {
			ev.AccountID = decision.Selected[:i]
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := a.Store.RecordUsageEvent(ctx, ev); err != nil {
		if a.Log != nil {
			a.Log.Error("usage event write failed", "request_id", requestID, "err", err)
		}
	}
}

func (a *App) streamChat(w http.ResponseWriter, r *http.Request, req *connectors.ChatRequest, requestID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpserver.WriteError(w, errs.New(errs.CodeInternal, "不支持流式"))
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	start := time.Now()
	started := false
	usage, decision, err := a.Router.ForwardStream(r.Context(), req.Model, req, requestID, func(b []byte) error {
		started = true
		_, err := w.Write(b)
		if err == nil {
			flusher.Flush()
		}
		return err
	})
	latency := time.Since(start).Milliseconds()
	if err != nil && !started {
		// 尚未输出任何内容，回写稳定错误事件（不透传上游原文，防泄露）
		a.logError(decision, err)
		stable := a.mapRelayError(err)
		a.recordUsage(requestID, req.Model, decision, nil, latency, stable.Code)
		payload, _ := json.Marshal(map[string]any{"error": map[string]any{
			"code": stable.Code, "message": stable.Message, "request_id": requestID,
		}})
		w.Write([]byte("data: " + string(payload) + "\n\n"))
		flusher.Flush()
		return
	}
	if err != nil {
		a.logError(decision, err)
	}
	var usageDTO *connectors.UsageDTO
	if usage != nil {
		usageDTO = &connectors.UsageDTO{
			PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens, TotalTokens: usage.InputTokens + usage.OutputTokens,
		}
	}
	errCode := errs.Code("")
	if err != nil {
		errCode = a.mapRelayError(err).Code
	}
	a.recordUsage(requestID, req.Model, decision, usageDTO, latency, errCode)
	w.Write([]byte("data: [DONE]\n\n"))
	flusher.Flush()
}

// mapRelayError 将中继错误映射为稳定错误。
func (a *App) mapRelayError(err error) *errs.Error {
	// 结构化错误分类（MR-012）：errors.Is/As，禁止字符串匹配
	if errors.Is(err, router.ErrNoCandidate) {
		return errs.New(errs.CodeModelUnavailable, "没有可用渠道")
	}
	switch {
	case errors.Is(err, connectors.ErrAuth):
		return errs.New(errs.CodeUpstreamAuth, "上游鉴权失败")
	case errors.Is(err, connectors.ErrRateLimited):
		return errs.Retryable(errs.New(errs.CodeRateLimited, "上游限流"))
	case errors.Is(err, connectors.ErrTimeout):
		return errs.Retryable(errs.New(errs.CodeUpstreamTimeout, "上游超时"))
	case errors.Is(err, connectors.ErrNetwork):
		return errs.Retryable(errs.New(errs.CodeUpstreamError, "网络错误"))
	case errors.Is(err, connectors.ErrTruncated):
		return errs.Retryable(errs.New(errs.CodeUpstreamError, "流式响应被截断"))
	case errors.Is(err, connectors.ErrModelUnavailable):
		return errs.New(errs.CodeModelUnavailable, "上游模型不可用")
	case errors.Is(err, connectors.ErrUpstream):
		return errs.Retryable(errs.New(errs.CodeUpstreamError, "上游异常"))
	case errors.Is(err, connectors.ErrQuotaExhausted):
		return errs.New(errs.CodeQuotaExhausted, "上游额度耗尽")
	default:
		return errs.Wrap(errs.CodeUpstreamError, "转发失败", err)
	}
}

func (a *App) logError(decision *router.Decision, err error) {
	if a.Log != nil && err != nil {
		reqID := ""
		alias := ""
		if decision != nil {
			reqID, alias = decision.RequestID, decision.Alias
		}
		a.Log.Error("relay failed", "request_id", reqID, "alias", alias, "err", err)
	}
}

// resolveTarget 解析账户目标（含凭据解密）。
// OAuth 账户（auth_type=oauth）：加载令牌束，按需单飞刷新；平台拒绝刷新时
// 标记 reauth_required 并返回上游鉴权错误（MR-007 步骤④ 接线）。
func (a *App) resolveTarget(ctx context.Context, acc domain.Account, modelID string) (*connectors.Target, error) {
	prov, err := a.Store.GetProvider(ctx, acc.ProviderID)
	if err != nil {
		return nil, err
	}
	if acc.Status != "active" {
		return nil, fmt.Errorf("账户不可用: %s", acc.Status)
	}
	if acc.AuthType == string(domain.AuthOAuth) {
		return a.resolveOAuthTarget(ctx, acc, prov, modelID)
	}
	sec, err := a.Vault.Get(credentials.SecretRef{
		Service: acc.SecretService, Account: acc.SecretAccount, VaultProvider: acc.VaultProvider,
	})
	if err != nil {
		return nil, err
	}
	defer sec.Zero()
	base := prov.BaseURL
	if base == "" {
		base = defaultBaseURL(prov.Kind)
	}
	return &connectors.Target{
		ProviderKind: connectors.ProviderKind(prov.Kind),
		BaseURL:      base,
		APIKey:       string(sec.Value),
		ModelID:      modelID,
	}, nil
}

// resolveOAuthTarget OAuth 账户目标解析：读 bundle → 过期则刷新（单飞）→
// 刷新被拒标记 reauth_required。返回的 Target.APIKey 为 access token。
func (a *App) resolveOAuthTarget(ctx context.Context, acc domain.Account, prov domain.Provider, modelID string) (*connectors.Target, error) {
	svc, ok := a.OAuth[prov.Kind]
	if !ok {
		return nil, fmt.Errorf("平台 %s 未注册 OAuth 服务", prov.Kind)
	}
	ref := credentials.SecretRef{
		Service: acc.SecretService, Account: acc.SecretAccount, VaultProvider: acc.VaultProvider,
	}
	newRef, bundle, err := svc.Refresh(ctx, acc.ID, ref.Service, ref.Account, ref)
	if err != nil {
		if errors.Is(err, oauth.ErrRefreshRejected) {
			_ = a.Store.SetAccountAuthState(context.WithoutCancel(ctx), acc.ID, string(domain.AuthStateReauthRequired))
			return nil, fmt.Errorf("oauth 凭据已失效，需要重新授权: %w", err)
		}
		return nil, fmt.Errorf("oauth 凭据刷新失败: %w", err)
	}
	// 刷新成功且引用发生变化：原子切换账户 SecretRef
	if newRef.Fingerprint != acc.SecretFingerprint {
		updated := acc
		updated.VaultProvider, updated.SecretService, updated.SecretAccount, updated.SecretFingerprint =
			newRef.VaultProvider, newRef.Service, newRef.Account, newRef.Fingerprint
		if uerr := a.Store.UpdateAccountSecretRef(context.WithoutCancel(ctx), acc.ID, updated); uerr != nil && a.Log != nil {
			a.Log.Error("update secret ref failed", "account", acc.ID, "err", uerr)
		}
	}
	base := prov.BaseURL
	if base == "" {
		base = defaultBaseURL(prov.Kind)
	}
	return &connectors.Target{
		ProviderKind: connectors.ProviderKind(prov.Kind),
		BaseURL:      base,
		APIKey:       bundle.AccessToken,
		ModelID:      modelID,
	}, nil
}

func provModelID(providerID, upstreamID string) string {
	return providerID + "|" + upstreamID
}

// shortID 生成随机短 ID。
func shortID() string {
	return randHex(6)
}

// randHex 生成 n 字节随机数的十六进制串。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}
