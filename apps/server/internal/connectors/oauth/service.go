package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"midroute/internal/credentials"
)

// refreshLead 是提前刷新余量：到期不足该时长即触发刷新。
const refreshLead = 5 * time.Minute

// refreshTimeout 是单次刷新请求的上限（上游同款 30s 预算）。
const refreshTimeout = 30 * time.Second

// Service 管理一个 OAuth 平台的授权会话与令牌刷新。
// 所有令牌只经由 Vault 存取；Service 不打含令牌的日志。
type Service struct {
	vault      credentials.Vault
	provider   Provider
	httpClient *http.Client
	now        func() time.Time
	states     *stateStore

	// inflight 保证同一账户并发的刷新只发起一次上游请求（单飞）。
	inflightMu sync.Mutex
	inflight   map[string]*inflightCall
}

type inflightCall struct {
	done chan struct{}
	ref  credentials.SecretRef
	b    TokenBundle
	err  error
}

// NewService 创建 Service。provider 由调用方注入（生产用 Codex，测试可指向假端点）。
func NewService(v credentials.Vault, p Provider, httpClient *http.Client) *Service {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	now := func() time.Time { return time.Now().UTC() }
	return &Service{
		vault:      v,
		provider:   p,
		httpClient: httpClient,
		now:        now,
		states:     newStateStore(),
		inflight:   map[string]*inflightCall{},
	}
}

// StartAuth 发起一次本机授权：生成 state 与 PKCE，返回授权 URL。
// verifier 与目标 Vault 坐标保存在内存会话中，等待 CompleteAuth 消费。
// 返回的授权 URL 只含 state 与 challenge，不含任何令牌。
func (s *Service) StartAuth(service, account string) (string, error) {
	if !s.vault.Persistent() {
		return "", ErrVaultNotPersistent
	}
	if service == "" || account == "" {
		return "", errors.New("oauth: service and account are required")
	}
	codes, err := newPKCE()
	if err != nil {
		return "", err
	}
	state, err := s.states.put(s.now(), codes.verifier, service, account)
	if err != nil {
		return "", err
	}
	q := url.Values{
		"client_id":                  {s.provider.ClientID},
		"response_type":              {"code"},
		"redirect_uri":               {s.provider.RedirectURI},
		"scope":                      {s.provider.AuthScope},
		"state":                      {state},
		"code_challenge":             {codes.challenge},
		"code_challenge_method":      {"S256"},
		"prompt":                     {"login"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}
	return s.provider.AuthURL + "?" + q.Encode(), nil
}

// CompleteAuth 用回调参数完成授权：校验并消费 state，用 code + verifier 换取
// 令牌，整体写入 Vault，返回凭据引用与令牌元数据（不含令牌本体）。
// callbackError 非空表示用户在平台侧取消或授权失败。
func (s *Service) CompleteAuth(ctx context.Context, state, code, callbackError string) (credentials.SecretRef, TokenBundle, error) {
	if callbackError != "" {
		return credentials.SecretRef{}, TokenBundle{}, ErrAuthorizationCancelled
	}
	if state == "" || code == "" {
		return credentials.SecretRef{}, TokenBundle{}, ErrStateInvalid
	}
	entry, err := s.states.consume(s.now(), state)
	if err != nil {
		return credentials.SecretRef{}, TokenBundle{}, err
	}
	b, err := s.exchange(ctx, code, entry.verifier)
	if err != nil {
		return credentials.SecretRef{}, TokenBundle{}, err
	}
	ref, err := s.vault.Store(entry.service, entry.account, mustJSON(b))
	if err != nil {
		return credentials.SecretRef{}, TokenBundle{}, fmt.Errorf("oauth: store tokens: %w", err)
	}
	return ref, b, nil
}

// Refresh 读取并按需刷新令牌，返回当前有效令牌与（可能更新的）凭据引用。
// 同一 key（service|account）的并发调用只触发一次上游刷新：
// 第一个到达者执行真实刷新，其余等待并共享结果；失败时保留旧凭据并返回错误。
// 平台明确拒绝（401/400）返回 ErrRefreshRejected，调用方应标记 reauth_required；
// 其他失败返回 ErrRefreshFailed，可稍后重试。
func (s *Service) Refresh(ctx context.Context, key, service, account string, ref credentials.SecretRef) (credentials.SecretRef, TokenBundle, error) {
	s.inflightMu.Lock()
	call, ok := s.inflight[key]
	if !ok {
		call = &inflightCall{done: make(chan struct{})}
		s.inflight[key] = call
		s.inflightMu.Unlock()

		// 创建者路径：执行真实刷新
		newRef, bundle, err := s.refreshOnce(ctx, service, account, ref)

		s.inflightMu.Lock()
		if cur, exists := s.inflight[key]; exists && cur == call {
			delete(s.inflight, key)
		}
		s.inflightMu.Unlock()

		// 先写结果再 close，等待者经 channel 可见性安全读取
		call.ref, call.b, call.err = newRef, bundle, err
		close(call.done)
		return newRef, bundle, err
	}
	s.inflightMu.Unlock()

	<-call.done
	return call.ref, call.b, call.err
}

// refreshOnce 执行一次真实刷新。失败时不动 Vault（旧凭据保留）。
func (s *Service) refreshOnce(ctx context.Context, service, account string, ref credentials.SecretRef) (credentials.SecretRef, TokenBundle, error) {
	old, err := s.vault.Get(ref)
	if err != nil {
		return ref, TokenBundle{}, fmt.Errorf("oauth: load tokens: %w", err)
	}
	defer old.Zero()
	var oldBundle TokenBundle
	if err := json.Unmarshal(old.Value, &oldBundle); err != nil {
		return ref, TokenBundle{}, fmt.Errorf("oauth: parse stored bundle: %w", err)
	}
	if !needsRefresh(oldBundle.ExpiresAt, s.now(), refreshLead) {
		return ref, oldBundle, nil
	}

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	bundle, err := s.postRefresh(rctx, oldBundle.RefreshToken)
	if err != nil {
		return ref, oldBundle, err
	}
	newRef, err := s.vault.Store(service, account, mustJSON(bundle))
	if err != nil {
		// 旧 refresh_token 可能已被轮换作废；存储失败必须显式暴露，不得静默。
		return ref, oldBundle, fmt.Errorf("oauth: store refreshed tokens: %w", err)
	}
	_ = s.vault.Delete(ref)
	return newRef, bundle, nil
}

// postRefresh 调用平台令牌端点执行 refresh_token 授权。
func (s *Service) postRefresh(ctx context.Context, refreshToken string) (TokenBundle, error) {
	if refreshToken == "" {
		return TokenBundle{}, ErrRefreshRejected
	}
	form := url.Values{
		"client_id":     {s.provider.ClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {s.provider.RefreshScope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.provider.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenBundle{}, fmt.Errorf("oauth: build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return TokenBundle{}, fmt.Errorf("%w: %v", ErrRefreshFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// 只提取平台错误码字段，不携带原始正文（防止令牌类内容进入异常链）。
		return TokenBundle{}, refreshError(resp.StatusCode, resp.Body)
	}
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tokenResp); err != nil {
		return TokenBundle{}, fmt.Errorf("%w: parse response: %v", ErrRefreshFailed, err)
	}
	if tokenResp.AccessToken == "" || tokenResp.RefreshToken == "" {
		return TokenBundle{}, fmt.Errorf("%w: response missing token fields", ErrRefreshFailed)
	}
	return TokenBundle{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		IDToken:      tokenResp.IDToken,
		ExpiresAt:    s.now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}, nil
}

// exchange 用授权码 + PKCE verifier 换取令牌（仅 CompleteAuth 使用）。
func (s *Service) exchange(ctx context.Context, code, verifier string) (TokenBundle, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {s.provider.ClientID},
		"code":          {code},
		"redirect_uri":  {s.provider.RedirectURI},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.provider.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenBundle{}, fmt.Errorf("oauth: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return TokenBundle{}, fmt.Errorf("oauth: token exchange failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return TokenBundle{}, fmt.Errorf("%w: status %d", ErrRefreshFailed, resp.StatusCode)
	}
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tokenResp); err != nil {
		return TokenBundle{}, fmt.Errorf("oauth: parse token response: %w", err)
	}
	if tokenResp.AccessToken == "" || tokenResp.RefreshToken == "" {
		return TokenBundle{}, errors.New("oauth: token response missing fields")
	}
	return TokenBundle{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		IDToken:      tokenResp.IDToken,
		ExpiresAt:    s.now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}, nil
}

// Revoke 撤销平台授权。当前无已验证的官方撤销端点（MR-006 结论），
// 显式返回 ErrRevokeUnsupported；本地断开（删 Vault 条目 + 账户下线）由调用方执行。
func (s *Service) Revoke() error {
	return ErrRevokeUnsupported
}

// needsRefresh 判断令牌是否需要刷新：缺失失效时间按需要刷新处理（保守）。
func needsRefresh(expiresAt string, now time.Time, lead time.Duration) bool {
	if expiresAt == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return true
	}
	return now.Add(lead).After(t)
}

// refreshError 从错误响应提取 error/error_description 字段，避免原始正文进入异常链。
func refreshError(status int, body io.Reader) error {
	var parsed struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	rejected := status == http.StatusBadRequest || status == http.StatusUnauthorized
	if err := json.NewDecoder(io.LimitReader(body, 4<<10)).Decode(&parsed); err == nil && parsed.Error != "" {
		wrapped := fmt.Errorf("%w: %s: %s", ErrRefreshFailed, parsed.Error, parsed.Description)
		if rejected {
			return errors.Join(ErrRefreshRejected, wrapped)
		}
		return wrapped
	}
	if rejected {
		return fmt.Errorf("%w: status %d", ErrRefreshRejected, status)
	}
	return fmt.Errorf("%w: status %d", ErrRefreshFailed, status)
}

func mustJSON(b TokenBundle) []byte {
	data, err := json.Marshal(b)
	if err != nil {
		// TokenBundle 全为 string 字段，不存在无法序列化的类型；防御性兜底。
		panic(fmt.Sprintf("oauth: marshal token bundle: %v", err))
	}
	return data
}
