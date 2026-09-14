// Package session 实现本地安全边界（对齐 PRD US-010 / FR-24 与 MR-002 步骤④）。
//   - 本地模式：只有 loopback 来源可免登录访问；远程来源必须认证。
//   - 认证方式：Authorization: Bearer <管理员密钥>（CLI/程序客户端）或
//     受保护会话 Cookie（浏览器，POST /api/v1/session 登录建立，DELETE 退出）。
//   - Host 边界：免登录路径要求 Host 为 loopback 字面量，阻断 DNS rebinding。
//   - CSRF：写请求携带 Origin/Referer 时必须与请求 Host 同源；非浏览器客户端
//     （两者都不带）不受影响。不读取任何 X-Forwarded-* 转发头，代理部署
//     需自行保证 Host 语义并配置 AdminKey。
//   - 密钥与会话令牌比对使用常量时间比较，避免时序侧信道。
package session

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"midroute/internal/errs"
)

// CookieName 是受保护会话的 Cookie 名。
const CookieName = "midroute_session"

// defaultSessionTTL 是会话的空闲过期时长；每次通过校验滑动续期。
const defaultSessionTTL = 12 * time.Hour

// Guard 判定请求是否被授权。
type Guard struct {
	localOnly bool
	adminKey  string
	sessions  *sessionStore
}

// NewGuard 创建安全守卫。localOnly=true 时仅 loopback 免登录；否则必须认证
// （Bearer 管理员密钥或会话 Cookie）。
func NewGuard(localOnly bool, adminKey string) *Guard {
	return &Guard{localOnly: localOnly, adminKey: adminKey, sessions: newSessionStore(defaultSessionTTL)}
}

// IsLoopback 判断请求是否来自回环地址。
func IsLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Authorize 校验请求授权。返回 nil 表示允许；否则返回稳定错误。
// 规则（按序）：
//  1. 免登录路径（loopback + localOnly）要求 Host 为 loopback 字面量；
//  2. 认证：有效会话 Cookie 或 Bearer 管理员密钥；免登录路径跳过；
//  3. 写请求的 Origin/Referer（如携带）必须与 Host 同源。
func (g *Guard) Authorize(r *http.Request) *errs.Error {
	loopback := IsLoopback(r)
	freePath := loopback && g.localOnly

	if freePath && !hostIsLoopbackLiteral(r.Host) {
		return errs.New(errs.CodeForbidden, "Host 不在允许范围（疑似 DNS rebinding）")
	}

	if !freePath {
		if g.sessionToken(r) != "" {
			// 有效会话（内部已滑动续期）
		} else {
			token := extractBearer(r.Header.Get("Authorization"))
			if g.adminKey == "" {
				return errs.New(errs.CodeForbidden, "未配置管理员密钥，拒绝远程访问")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(g.adminKey)) != 1 {
				return errs.New(errs.CodeUnauthorized, "认证失败：密钥无效或会话不存在")
			}
		}
	}

	if isWriteMethod(r.Method) {
		if err := checkSameOrigin(r); err != nil {
			return err
		}
	}
	return nil
}

// LocalOnlyFree 报告是否处于本地免登录模式（会话端点据此拒绝发无意义会话）。
func (g *Guard) LocalOnlyFree() bool { return g.localOnly }

// IssueSession 签发一个新会话令牌，返回令牌与过期时间。
func (g *Guard) IssueSession() (token string, expiresAt time.Time, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, g.sessions.add(token), nil
}

// RevokeRequestSession 注销请求 Cookie 携带的会话；返回是否存在该会话。
func (g *Guard) RevokeRequestSession(r *http.Request) bool {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return false
	}
	return g.sessions.remove(c.Value)
}

// sessionToken 返回请求携带的有效会话令牌（滑动续期）；无有效会话返回空串。
func (g *Guard) sessionToken(r *http.Request) string {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return ""
	}
	if !g.sessions.valid(c.Value) {
		return ""
	}
	return c.Value
}

// -------- 会话存储 --------

// sessionStore 保存会话令牌到过期时间。令牌只存服务端内存：进程重启即全部
// 失效（安全默认，宁可要求重新登录也不持久化会话）。
type sessionStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]time.Time
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{ttl: ttl, now: time.Now, entries: map[string]time.Time{}}
}

func (s *sessionStore) add(token string) time.Time {
	exp := s.now().Add(s.ttl)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[token] = exp
	return exp
}

func (s *sessionStore) valid(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.entries[token]
	if !ok {
		return false
	}
	if !s.now().Before(exp) {
		delete(s.entries, token)
		return false
	}
	// 滑动续期：活跃用户不因空闲超时被登出
	s.entries[token] = s.now().Add(s.ttl)
	return true
}

func (s *sessionStore) remove(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[token]
	delete(s.entries, token)
	return ok
}

// -------- Host / Origin 判定 --------

// hostIsLoopbackLiteral 判断 Host 头（host[:port]）的主机部分是否为
// loopback 字面量（localhost / 127.0.0.0/8 / ::1）。端口不作限制：服务只
// 监听自己的端口，攻击者无法让别的端口响应；名字解析到回环的非字面量
// Host（DNS rebinding 的形态）在这里被拒绝。
func hostIsLoopbackLiteral(hostPort string) bool {
	hostPort = strings.TrimSpace(hostPort)
	if hostPort == "" {
		return false
	}
	host := hostPort
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isWriteMethod 判定是否状态变更方法（CSRF 校验范围）。
func isWriteMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// checkSameOrigin 校验写请求的来源：浏览器跨站请求必带 Origin（或 Referer），
// 必须与请求自身的 Host 同源；两者都不携带的非浏览器客户端放行。
func checkSameOrigin(r *http.Request) *errs.Error {
	originHost := ""
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host == "" {
			return errs.New(errs.CodeForbidden, "Origin 头无效")
		}
		originHost = strings.ToLower(u.Host)
	} else if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		if err != nil || u.Host == "" {
			return errs.New(errs.CodeForbidden, "Referer 头无效")
		}
		originHost = strings.ToLower(u.Host)
	} else {
		return nil
	}
	if originHost != strings.ToLower(strings.TrimSpace(r.Host)) {
		return errs.New(errs.CodeForbidden, "跨站写请求被拒绝")
	}
	return nil
}

func extractBearer(h string) string {
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return h[len(prefix):]
	}
	return h
}
