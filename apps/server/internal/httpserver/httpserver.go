// Package httpserver 提供 Midroute 的 HTTP 基础：/healthz、/readyz、安全边界与路由挂载。
package httpserver

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"midroute/internal/errs"
	"midroute/internal/session"
)

// Server 持有路由表与中间件。
type Server struct {
	Log       *slog.Logger
	DB        *dbStore
	Guard     *session.Guard
	Mux       *http.ServeMux
	StartedAt time.Time
}

type dbStore struct {
	ready   bool
	version int
}

// NewDBStore 报告数据库就绪状态。
func NewDBStore(version int) *dbStore {
	return &dbStore{ready: true, version: version}
}

// New 创建基础 HTTP 服务（healthz/readyz + 安全边界）。
func New(log *slog.Logger, store *dbStore, guard *session.Guard) *Server {
	s := &Server{Log: log, DB: store, Guard: guard, Mux: http.NewServeMux(), StartedAt: time.Now()}
	s.Mux.HandleFunc("/healthz", s.healthz)
	s.Mux.HandleFunc("/readyz", s.readyz)
	s.Mux.HandleFunc("/api/v1/session", s.handleSession)
	return s
}

// handleSession 管理受保护会话：POST 登录签发 Cookie，DELETE 退出注销。
// 请求本身经过 Guard：POST 需 Bearer 管理员密钥，DELETE 凭有效会话 Cookie。
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		if s.Guard.LocalOnlyFree() {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "本地免登录模式无需会话"})
			return
		}
		token, expires, err := s.Guard.IssueSession()
		if err != nil {
			writeError(w, errs.Wrap(errs.CodeInternal, "签发会话失败", err))
			return
		}
		// Secure 省略：本地明文 HTTP 部署；经 TLS 反代时应由代理保证传输安全。
		http.SetCookie(w, &http.Cookie{
			Name:     session.CookieName,
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Expires:  expires,
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "expires_at": expires.UTC().Format(time.RFC3339)})
	case http.MethodDelete:
		existed := s.Guard.RevokeRequestSession(r)
		http.SetCookie(w, &http.Cookie{
			Name:     session.CookieName,
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   -1,
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": existed})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "reason": "方法不支持"})
	}
}

// Mount 注册附加路由（管理 API、网关 API）。
func (s *Server) Mount(pattern string, h http.Handler) {
	s.Mux.Handle(pattern, h)
}

// MountFunc 注册附加函数路由。
func (s *Server) MountFunc(pattern string, fn http.HandlerFunc) {
	s.Mux.HandleFunc(pattern, fn)
}

// Handler 返回带安全边界中间件的最终 handler。
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			s.Mux.ServeHTTP(w, r)
			return
		}
		if err := s.Guard.Authorize(r); err != nil {
			writeError(w, err)
			return
		}
		s.Mux.ServeHTTP(w, r)
	})
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "midroute", "schema_version": s.DB.version})
}

func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	if !s.DB.ready {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "reason": "database not ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "db_ready": true, "uptime_seconds": int(time.Since(s.StartedAt).Seconds())})
}

// WriteJSON 导出供 api 包复用。
func WriteJSON(w http.ResponseWriter, status int, v any) { writeJSON(w, status, v) }

// WriteError 导出供 api 包复用。
func WriteError(w http.ResponseWriter, err *errs.Error) { writeError(w, err) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err *errs.Error) {
	writeJSON(w, err.HTTPStatus(), map[string]any{"error": map[string]any{
		"code": err.Code, "message": err.Message, "retryable": err.Retryable,
	}})
}
