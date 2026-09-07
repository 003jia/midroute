package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	// 用真实监听验证 RemoteAddr 判定；httptest 的 RemoteAddr 不可控，直接测 handler
	h := newTestServer(t, true)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.RemoteAddr = "192.168.0.5:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("remote without key: got %d", w.Code)
	}
	req.RemoteAddr = "127.0.0.1:1234"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("loopback should reach route (404): got %d", w.Code)
	}
}

func TestProtectedRouteWithValidKey(t *testing.T) {
	h := newTestServer(t, true)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.RemoteAddr = "192.168.0.5:1234"
	req.Header.Set("Authorization", "Bearer test-admin-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("valid key should reach route (404): got %d", w.Code)
	}
}
