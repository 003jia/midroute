package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStaticTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>管理台</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("console.log(1)"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStaticServesFilesAndSPAFallback(t *testing.T) {
	h := staticHandler(newStaticTestDir(t))

	// 根路径回退 index.html
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "管理台") {
		t.Fatalf("root: %d %s", w.Code, w.Body.String())
	}
	// 静态资源
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "console.log") {
		t.Fatalf("asset: %d", w.Code)
	}
	// SPA 前端路由回退 index.html
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/accounts/page/deep", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "管理台") {
		t.Fatalf("spa fallback: %d", w.Code)
	}
}

func TestStaticUnknownAPIReturnsJSON(t *testing.T) {
	h := staticHandler(newStaticTestDir(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/unknown", nil))
	if w.Code != 404 {
		t.Fatalf("code=%d", w.Code)
	}
	if ct := w.Header().Get("content-type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("unknown API must return JSON, got %s", ct)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/unknown", nil))
	if ct := w.Header().Get("content-type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("/v1 unknown must return JSON, got %s", ct)
	}
}

func TestStaticBlocksPathTraversal(t *testing.T) {
	h := staticHandler(newStaticTestDir(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/..%2f..%2fetc%2fpasswd", nil))
	// 不应返回 200；且回退为 index.html 或 404，绝不能读到外部文件
	if w.Code == 200 && strings.Contains(w.Body.String(), "root:") {
		t.Fatal("path traversal leaked file content")
	}
}
