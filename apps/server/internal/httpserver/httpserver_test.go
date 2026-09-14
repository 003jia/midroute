package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"log/slog"

	"midroute/internal/session"
)

func newTestServer(t *testing.T, localOnly bool) http.Handler {
	t.Helper()
	log := slog.New(slog.NewTextHandler(discard{}, nil))
	guard := session.NewGuard(localOnly, "test-admin-key")
	return New(log, NewDBStore(1), guard).Handler()
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func TestHealthz(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, true))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true || body["service"] != "midroute" {
		t.Fatalf("body=%v", body)
	}
}

func TestReadyz(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, true))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestProtectedRouteRequiresAuthWhenRemote(t *testing.T) {
	// 用真实监听验证 RemoteAddr 判定；httptest 的 RemoteAddr 不可控，直接测 handler。
	// 注意 /v1/* 已改为推理面（项目令牌鉴权，MR-016），Guard 只覆盖管理面路径。
	h := newTestServer(t, true)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	req.RemoteAddr = "192.168.0.5:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("remote without key: got %d", w.Code)
	}
	req.RemoteAddr = "127.0.0.1:1234"
	req.Host = "127.0.0.1"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("loopback should reach route (404): got %d", w.Code)
	}
}

func TestProtectedRouteWithValidKey(t *testing.T) {
	h := newTestServer(t, true)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	req.RemoteAddr = "192.168.0.5:1234"
	req.Header.Set("Authorization", "Bearer test-admin-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("valid key should reach route (404): got %d", w.Code)
	}
}

// TestInferencePathBypassesGuard /v1/* 不走管理 Guard（推理面由项目令牌鉴权）。
func TestInferencePathBypassesGuard(t *testing.T) {
	h := newTestServer(t, true)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.RemoteAddr = "192.168.0.5:1234" // 远程来源也不需要管理密钥
	req.Host = "any.host"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
		t.Fatalf("/v1 must not be blocked by admin guard, got %d", w.Code)
	}
}

// TestForgedHostRejectedOnFreePath DNS rebinding 防护：本地免登录路径上
// 非法字面量 Host（example.com 解析到回环）必须被拒绝。
func TestForgedHostRejectedOnFreePath(t *testing.T) {
	h := newTestServer(t, true)
	for _, host := range []string{"example.com", "evil.attacker.net:18100", ""} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("Host %q should be rejected with 403, got %d", host, w.Code)
		}
	}
	// loopback 字面量 Host 放行
	for _, host := range []string{"127.0.0.1:18100", "localhost:18100", "[::1]:18100"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Fatalf("Host %q should be allowed", host)
		}
	}
}

// TestCrossSiteWriteRejected CSRF 防护：跨站 Origin/Referer 的写请求被拒，
// 同源与无 Origin 的程序客户端放行。
func TestCrossSiteWriteRejected(t *testing.T) {
	h := newTestServer(t, true)
	post := func(origin, referer string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", strings.NewReader("{}"))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Host = "127.0.0.1:18100"
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if got := post("http://evil.example.com", ""); got != http.StatusForbidden {
		t.Fatalf("cross-site Origin write: got %d want 403", got)
	}
	if got := post("", "http://evil.example.com/page"); got != http.StatusForbidden {
		t.Fatalf("cross-site Referer write: got %d want 403", got)
	}
	if got := post("http://127.0.0.1:18100", ""); got == http.StatusForbidden {
		t.Fatalf("same-origin write must pass, got %d", got)
	}
	if got := post("", ""); got == http.StatusForbidden {
		t.Fatalf("non-browser write (no Origin/Referer) must pass, got %d", got)
	}
}

// TestSessionLoginLogout 受保护会话全流程：Bearer 登录 → Cookie 访问 →
// 退出 → Cookie 失效。
func TestSessionLoginLogout(t *testing.T) {
	h := newTestServer(t, false) // 远程模式
	ts := httptest.NewServer(h)
	defer ts.Close()
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	// 无凭据访问被拒
	resp, err := client.Get(ts.URL + "/api/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no credentials: got %d", resp.StatusCode)
	}

	// Bearer 登录签发会话
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/session", nil)
	req.Header.Set("Authorization", "Bearer test-admin-key")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: got %d", resp.StatusCode)
	}
	var cookies []*http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == session.CookieName {
			cookies = append(cookies, c)
		}
	}
	if len(cookies) != 1 {
		t.Fatalf("session cookie not set")
	}
	if !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie flags wrong: %+v", cookies[0])
	}

	// 仅凭 Cookie 访问受保护路由（到达 mux 返回 404 即视为通过 Guard）
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/models", nil)
	req.AddCookie(cookies[0])
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cookie access: got %d want 404 (passed guard)", resp.StatusCode)
	}

	// 退出
	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/session", nil)
	req.AddCookie(cookies[0])
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || out["revoked"] != true {
		t.Fatalf("logout: %d %v", resp.StatusCode, out)
	}

	// 旧 Cookie 已失效
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/models", nil)
	req.AddCookie(cookies[0])
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked cookie must fail: got %d", resp.StatusCode)
	}
}

// TestSessionEndpointRejectsLocalFreeMode 本地免登录模式下不发会话。
func TestSessionEndpointRejectsLocalFreeMode(t *testing.T) {
	h := newTestServer(t, true)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/session", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Host = "127.0.0.1"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("local free mode session issue should 400, got %d", w.Code)
	}
}
