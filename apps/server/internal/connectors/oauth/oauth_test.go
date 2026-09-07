package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"midroute/internal/credentials"
)

// persistentVault 是测试用持久内存库（模拟 Keychain 语义）。
type persistentVault struct {
	*credentials.InMemoryVault
}

func (persistentVault) Persistent() bool { return true }

// tokenServer 是可编程的假令牌端点：按 PKCE 语义校验 verifier（SHA256 匹配
// StartAuth 授权 URL 中的 challenge），记录请求次数，可注入失败状态码。
type tokenServer struct {
	*httptest.Server
	mu         sync.Mutex
	requests   int
	failStatus int    // 非 0 时返回该状态码
	challenge  string // 最近一次授权 URL 中登记的 code_challenge
}

func newTokenServer(t *testing.T) *tokenServer {
	t.Helper()
	ts := &tokenServer{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		ts.requests++
		fail := ts.failStatus
		ts.mu.Unlock()
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		form := map[string]string{}
		for k := range r.Form {
			form[k] = r.Form.Get(k)
		}
		if fail != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fail)
			fmt.Fprint(w, `{"error":"invalid_grant","error_description":"token expired or revoked"}`)
			return
		}
		switch form["grant_type"] {
		case "authorization_code":
			// 真实 PKCE 语义：SHA256(verifier) 必须匹配授权时登记的 challenge
			if form["code"] != "auth-code-1" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"invalid_grant","error_description":"bad code"}`)
				return
			}
			sum := sha256.Sum256([]byte(form["code_verifier"]))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != ts.currentChallenge() {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"invalid_grant","error_description":"PKCE mismatch"}`)
				return
			}
			writeToken(w)
		case "refresh_token":
			// 模拟轮换失效：仅特定值被拒
			if form["refresh_token"] == "" || form["refresh_token"] == "stale" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"invalid_grant","error_description":"refresh token rejected"}`)
				return
			}
			writeToken(w)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (ts *tokenServer) currentChallenge() string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.challenge
}

func (ts *tokenServer) setChallenge(v string) {
	ts.mu.Lock()
	ts.challenge = v
	ts.mu.Unlock()
}

func writeToken(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  "at-" + fmt.Sprint(time.Now().UnixNano()),
		"refresh_token": "rt-" + fmt.Sprint(time.Now().UnixNano()),
		"id_token":      "idt",
		"expires_in":    3600,
	})
}

func newTestService(t *testing.T, ts *tokenServer) *Service {
	t.Helper()
	p := Codex
	p.TokenURL = ts.URL
	return NewService(persistentVault{credentials.NewInMemoryVault()}, p, ts.Client())
}

func extractState(authURL string) string {
	idx := strings.Index(authURL, "state=")
	state := authURL[idx+len("state="):]
	if amp := strings.Index(state, "&"); amp >= 0 {
		state = state[:amp]
	}
	return state
}

// startAndComplete 发起授权（把授权 URL 的 challenge 登记到假端点），
// 再以约定 code 完成交换，模拟浏览器完成整个回跳。
func startAndComplete(t *testing.T, ts *tokenServer, s *Service, service, account, callbackErr string) (credentials.SecretRef, TokenBundle, error) {
	t.Helper()
	authURL, err := s.StartAuth(service, account)
	if err != nil {
		return credentials.SecretRef{}, TokenBundle{}, err
	}
	if i := strings.Index(authURL, "code_challenge="); i >= 0 {
		ch := authURL[i+len("code_challenge="):]
		if amp := strings.Index(ch, "&"); amp >= 0 {
			ch = ch[:amp]
		}
		ts.setChallenge(ch)
	}
	return s.CompleteAuth(context.Background(), extractState(authURL), "auth-code-1", callbackErr)
}

func TestCompleteAuthSuccess(t *testing.T) {
	ts := newTokenServer(t)
	s := newTestService(t, ts)

	authURL, err := s.StartAuth("account-codex", "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(authURL, "access_token") || strings.Contains(authURL, "refresh_token") {
		t.Fatal("auth URL must not contain tokens")
	}
	ts.setChallenge(extractParam(t, authURL, "code_challenge"))

	ref, bundle, err := s.CompleteAuth(context.Background(), extractState(authURL), "auth-code-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Service == "" || ref.Fingerprint == "" {
		t.Fatal("empty secret ref")
	}
	if bundle.AccessToken == "" || bundle.RefreshToken == "" || bundle.ExpiresAt == "" {
		t.Fatalf("incomplete bundle: %+v", bundle)
	}
	// 同一 state 二次消费必须失败
	if _, _, err := s.CompleteAuth(context.Background(), extractState(authURL), "auth-code-1", ""); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("replayed state: %v", err)
	}
}

func extractParam(t *testing.T, raw, key string) string {
	t.Helper()
	i := strings.Index(raw, key+"=")
	if i < 0 {
		t.Fatalf("param %q missing in %q", key, raw)
	}
	v := raw[i+len(key)+1:]
	if amp := strings.Index(v, "&"); amp >= 0 {
		v = v[:amp]
	}
	return v
}

func TestCompleteAuthStateErrors(t *testing.T) {
	ts := newTokenServer(t)
	s := newTestService(t, ts)

	// 未知 state
	if _, _, err := s.CompleteAuth(context.Background(), "bogus", "code", ""); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("bogus state: %v", err)
	}
	// 用户取消：回调携带 error
	if _, _, err := startAndComplete(t, ts, s, "svc", "acc", "access_denied"); !errors.Is(err, ErrAuthorizationCancelled) {
		t.Fatalf("cancelled: %v", err)
	}
	// state 过期：注入时钟推进 11 分钟（超过 stateTTL）
	authURL, err := s.StartAuth("svc", "acc")
	if err != nil {
		t.Fatal(err)
	}
	ts.setChallenge(extractParam(t, authURL, "code_challenge"))
	base := s.now
	s.now = func() time.Time { return base().Add(11 * time.Minute) }
	defer func() { s.now = base }()
	if _, _, err := s.CompleteAuth(context.Background(), extractState(authURL), "auth-code-1", ""); !errors.Is(err, ErrStateExpired) {
		t.Fatalf("expired state: %v", err)
	}
}

func TestPKCEMismatchRejected(t *testing.T) {
	ts := newTokenServer(t)
	s := newTestService(t, ts)

	authURL, err := s.StartAuth("svc", "acc")
	if err != nil {
		t.Fatal(err)
	}
	ts.setChallenge(extractParam(t, authURL, "code_challenge"))
	// 正确 state、错误 verifier：平台应 400 拒绝，CompleteAuth 显式报错不写库
	if _, _, err := s.CompleteAuth(context.Background(), extractState(authURL), "auth-code-1", ""); err != nil {
		// 先验证正确 verifier 能通过（对照组）
		t.Fatalf("correct verifier should pass: %v", err)
	}

	// 错误 verifier 直接打端点：假端点按 PKCE 校验必须拒绝
	bad := NewService(persistentVault{credentials.NewInMemoryVault()}, Provider{
		Name: "codex", AuthURL: "http://127.0.0.1:1/authorize", TokenURL: ts.URL,
		ClientID: "app_EMoamEEZ73f0CkXaXp7hrann", AuthScope: "openid", RefreshScope: "openid",
		RedirectURI: "http://localhost:1455/auth/callback",
	}, ts.Client())
	if _, err := bad.exchange(context.Background(), "auth-code-1", "wrong-verifier"); err == nil {
		t.Fatal("PKCE mismatch must be rejected by provider")
	}
}

func TestRefreshRotation(t *testing.T) {
	ts := newTokenServer(t)
	s := newTestService(t, ts)

	ref, _, err := startAndComplete(t, ts, s, "account-codex", "acc_1", "")
	if err != nil {
		t.Fatal(err)
	}
	// 到期时间在 1 小时后，未到 lead（5 分钟）→ 不触发刷新，引用不变
	newRef, bundle, err := s.Refresh(context.Background(), "account-codex|acc_1", "account-codex", "acc_1", ref)
	if err != nil {
		t.Fatal(err)
	}
	if newRef.Fingerprint != ref.Fingerprint {
		t.Fatal("unexpired token must not rotate")
	}
	if bundle.AccessToken == "" {
		t.Fatal("bundle lost")
	}

	// 注入时钟越过到期时间 → 刷新并轮换
	base := s.now
	s.now = func() time.Time { return base().Add(2 * time.Hour) }
	newRef2, bundle2, err := s.Refresh(context.Background(), "account-codex|acc_1", "account-codex", "acc_1", newRef)
	if err != nil {
		t.Fatal(err)
	}
	if newRef2.Fingerprint == newRef.Fingerprint {
		t.Fatal("refresh must rotate credential fingerprint")
	}
	if bundle2.RefreshToken == "" || bundle2.ExpiresAt == "" {
		t.Fatalf("bad refreshed bundle: %+v", bundle2)
	}
}

func TestRefreshFailureKeepsOldCredential(t *testing.T) {
	ts := newTokenServer(t)
	s := newTestService(t, ts)

	ref, _, err := startAndComplete(t, ts, s, "svc", "acc", "")
	if err != nil {
		t.Fatal(err)
	}
	ts.mu.Lock()
	ts.failStatus = http.StatusBadRequest
	ts.mu.Unlock()

	base := s.now
	s.now = func() time.Time { return base().Add(2 * time.Hour) }
	_, _, err = s.Refresh(context.Background(), "svc|acc", "svc", "acc", ref)
	if err == nil {
		t.Fatal("expected refresh failure")
	}
	if !errors.Is(err, ErrRefreshRejected) {
		t.Fatalf("want ErrRefreshRejected, got %v", err)
	}
	// 旧凭据仍然可读（未被覆盖）
	sec, err := s.vault.Get(ref)
	if err != nil {
		t.Fatalf("old credential lost after failed refresh: %v", err)
	}
	sec.Zero()

	// 平台恢复后可重试成功
	ts.mu.Lock()
	ts.failStatus = 0
	ts.mu.Unlock()
	newRef, _, err := s.Refresh(context.Background(), "svc|acc", "svc", "acc", ref)
	if err != nil {
		t.Fatalf("retry after transient failure: %v", err)
	}
	if newRef.Fingerprint == ref.Fingerprint {
		t.Fatal("successful retry must rotate")
	}
}

func TestRefreshSingleFlight(t *testing.T) {
	var upstreamHits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&upstreamHits, 1)
		time.Sleep(80 * time.Millisecond) // 放大并发窗口
		writeToken(w)
	}))
	defer srv.Close()

	p := Codex
	p.TokenURL = srv.URL
	s := NewService(persistentVault{credentials.NewInMemoryVault()}, p, srv.Client())

	// 预置一个已过期的 bundle（直接写入 Vault）
	ref, err := s.vault.Store("svc", "acc", mustJSON(TokenBundle{
		AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: "2000-01-01T00:00:00Z",
	}))
	if err != nil {
		t.Fatal(err)
	}

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = s.Refresh(context.Background(), "svc|acc", "svc", "acc", ref)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt64(&upstreamHits); got != 1 {
		t.Fatalf("upstream refresh calls = %d, want 1", got)
	}
}

func TestVaultNotPersistentRejected(t *testing.T) {
	ts := newTokenServer(t)
	p := Codex
	p.TokenURL = ts.URL
	s := NewService(credentials.NewInMemoryVault(), p, ts.Client())
	if _, err := s.StartAuth("svc", "acc"); !errors.Is(err, ErrVaultNotPersistent) {
		t.Fatalf("memory vault must be rejected, got %v", err)
	}
}

func TestRevokeUnsupported(t *testing.T) {
	ts := newTokenServer(t)
	s := newTestService(t, ts)
	if err := s.Revoke(); !errors.Is(err, ErrRevokeUnsupported) {
		t.Fatalf("codex revoke must be explicit unsupported, got %v", err)
	}
}

func TestAwaitCallback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	type cbResult struct {
		cb  CallbackResult
		err error
	}
	done := make(chan cbResult, 1)
	go func() {
		cb, err := AwaitCallback(context.Background(), ln, 5*time.Second)
		done <- cbResult{cb, err}
	}()
	// 模拟平台重定向
	resp, err := http.Get(fmt.Sprintf("http://%s/auth/callback?code=abc&state=xyz", ln.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.cb.Code != "abc" || r.cb.State != "xyz" {
			t.Fatalf("callback: %+v", r.cb)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("AwaitCallback timed out in test")
	}
	// 收到一个回调后监听器应关闭：再连应失败
	if _, err := http.Get(fmt.Sprintf("http://%s/auth/callback", ln.Addr())); err == nil {
		t.Fatal("listener should be closed after first callback")
	}

	// 回调超时：50ms 无回调应返回超时错误
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AwaitCallback(context.Background(), ln2, 50*time.Millisecond); err == nil {
		t.Fatal("expected callback timeout error")
	}

	// ExtractCallback：手动粘贴回调 URL 的场景
	res, err := ExtractCallback("http://localhost:1455/auth/callback?code=abc&state=xyz")
	if err != nil || res.Code != "abc" || res.State != "xyz" {
		t.Fatalf("ExtractCallback: %+v %v", res, err)
	}
	res, err = ExtractCallback("http://localhost:1455/auth/callback?error=access_denied")
	if err != nil || res.Error != "access_denied" {
		t.Fatalf("ExtractCallback error case: %+v %v", res, err)
	}
}
