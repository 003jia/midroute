package session

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func loopbackReq() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	r.Host = "127.0.0.1:18100"
	return r
}

func remoteReq() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.RemoteAddr = "192.168.1.10:12345"
	r.Host = "192.168.1.10:18100"
	return r
}

func TestLocalLoopbackBypass(t *testing.T) {
	g := NewGuard(true, "")
	if err := g.Authorize(loopbackReq()); err != nil {
		t.Fatalf("loopback must pass in localOnly mode: %v", err)
	}
	if err := g.Authorize(remoteReq()); err == nil {
		t.Fatal("remote must be rejected without key")
	} else if err.Code != "forbidden" {
		t.Fatalf("remote code=%s", err.Code)
	}
}

func TestRemoteRequiresValidKey(t *testing.T) {
	g := NewGuard(true, "secret-admin-key")
	req := remoteReq()
	req.Header.Set("Authorization", "Bearer wrong-key")
	if err := g.Authorize(req); err == nil {
		t.Fatal("wrong key must fail")
	}
	req.Header.Set("Authorization", "Bearer secret-admin-key")
	if err := g.Authorize(req); err != nil {
		t.Fatalf("valid key must pass: %v", err)
	}
}

func TestNonLoopbackLocalModeNeedsKeyToo(t *testing.T) {
	g := NewGuard(false, "admin")
	req := loopbackReq()
	req.Header.Set("Authorization", "Bearer admin")
	if err := g.Authorize(req); err != nil {
		t.Fatalf("local-only=false must still accept valid key: %v", err)
	}
	req.Header.Set("Authorization", "Bearer nope")
	if err := g.Authorize(req); err == nil {
		t.Fatal("invalid key must fail")
	}
}

func TestIsLoopback(t *testing.T) {
	if !IsLoopback(loopbackReq()) {
		t.Fatal("127.0.0.1 must be loopback")
	}
	if IsLoopback(remoteReq()) {
		t.Fatal("192.168.x must not be loopback")
	}
}

// TestFreePathRequiresLoopbackHost DNS rebinding：免登录路径上 Host 必须
// 是 loopback 字面量。
func TestFreePathRequiresLoopbackHost(t *testing.T) {
	g := NewGuard(true, "")
	for _, host := range []string{"example.com", "attacker.net:18100", "127.0.0.1.evil.net"} {
		r := loopbackReq()
		r.Host = host
		if err := g.Authorize(r); err == nil {
			t.Fatalf("Host %q must be rejected", host)
		}
	}
	for _, host := range []string{"127.0.0.1:18100", "localhost:9999", "[::1]:18100", "127.8.8.8:1"} {
		r := loopbackReq()
		r.Host = host
		if err := g.Authorize(r); err != nil {
			t.Fatalf("Host %q must pass: %v", host, err)
		}
	}
	// 非免登录路径不校验 Host 字面量（认证另行把关）
	r := remoteReq()
	r.Host = "panel.example.com:443"
	r.SetBasicAuth("", "")
	r.Header.Set("Authorization", "Bearer ")
	g2 := NewGuard(false, "k")
	if err := g2.Authorize(r); err == nil {
		t.Fatal("expected auth failure, not host failure")
	}
}

// TestWriteOriginSameOrigin CSRF：写请求 Origin/Referer 必须同源。
func TestWriteOriginSameOrigin(t *testing.T) {
	g := NewGuard(true, "")
	newPost := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", strings.NewReader("{}"))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Host = "127.0.0.1:18100"
		return r
	}
	r := newPost()
	r.Header.Set("Origin", "http://evil.com")
	if err := g.Authorize(r); err == nil || err.Code != "forbidden" {
		t.Fatalf("cross-site origin must be forbidden: %v", err)
	}
	r = newPost()
	r.Header.Set("Referer", "https://evil.com/x")
	if err := g.Authorize(r); err == nil || err.Code != "forbidden" {
		t.Fatalf("cross-site referer must be forbidden: %v", err)
	}
	r = newPost()
	r.Header.Set("Origin", "null")
	if err := g.Authorize(r); err == nil {
		t.Fatal("Origin: null must be rejected")
	}
	r = newPost()
	r.Header.Set("Origin", "http://127.0.0.1:18100")
	if err := g.Authorize(r); err != nil {
		t.Fatalf("same-origin write must pass: %v", err)
	}
	r = newPost()
	if err := g.Authorize(r); err != nil {
		t.Fatalf("no-origin write (non-browser) must pass: %v", err)
	}
	// GET 不做 Origin 校验
	r = loopbackReq()
	r.Header.Set("Origin", "http://evil.com")
	if err := g.Authorize(r); err != nil {
		t.Fatalf("GET must skip origin check: %v", err)
	}
}

// TestSessionStoreLifecycleAndExpiry 会话签发、滑动续期、过期与注销。
func TestSessionStoreLifecycleAndExpiry(t *testing.T) {
	g := NewGuard(false, "k")
	base := time.Now
	g.sessions.now = func() time.Time { return base() } // 可注入时钟
	token, _, err := g.IssueSession()
	if err != nil {
		t.Fatal(err)
	}

	req := remoteReq()
	req.AddCookie(&http.Cookie{Name: CookieName, Value: token})
	if err := g.Authorize(req); err != nil {
		t.Fatalf("session cookie must authenticate: %v", err)
	}

	// 滑动续期：临近过期时通过校验会刷新
	advance := func(d time.Duration) { b := g.sessions.now; g.sessions.now = func() time.Time { return b().Add(d) } }
	advance(defaultSessionTTL - time.Minute)
	req = remoteReq()
	req.AddCookie(&http.Cookie{Name: CookieName, Value: token})
	if err := g.Authorize(req); err != nil {
		t.Fatalf("session should be refreshed by activity: %v", err)
	}

	// 超过空闲 TTL 后失效
	advance(defaultSessionTTL + time.Minute)
	req = remoteReq()
	req.AddCookie(&http.Cookie{Name: CookieName, Value: token})
	if err := g.Authorize(req); err == nil {
		t.Fatal("expired session must fail")
	}

	// 注销路径
	token2, _, _ := g.IssueSession()
	req = remoteReq()
	req.AddCookie(&http.Cookie{Name: CookieName, Value: token2})
	if !g.RevokeRequestSession(req) {
		t.Fatal("revoke should report existed")
	}
	req = remoteReq()
	req.AddCookie(&http.Cookie{Name: CookieName, Value: token2})
	if err := g.Authorize(req); err == nil {
		t.Fatal("revoked session must fail")
	}
	if g.RevokeRequestSession(req) {
		t.Fatal("double revoke should report not existed")
	}
}

// TestSessionConcurrency 并发校验与注销不产生数据竞争（配合 -race）。
func TestSessionConcurrency(t *testing.T) {
	g := NewGuard(false, "k")
	token, _, _ := g.IssueSession()
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(revoke bool) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				req := remoteReq()
				req.AddCookie(&http.Cookie{Name: CookieName, Value: token})
				_ = g.Authorize(req)
				if revoke {
					g.RevokeRequestSession(req)
				}
			}
		}(i == 0)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
