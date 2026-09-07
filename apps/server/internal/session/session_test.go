package session

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func loopbackReq() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	return r
}

func remoteReq() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.RemoteAddr = "192.168.1.10:12345"
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
