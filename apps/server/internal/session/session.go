// Package session 实现本地安全边界（对齐 PRD US-010 / FR-24）。
//   - 本地模式：只有 loopback 来源可免登录访问；远程来源必须提供管理员密钥。
//   - 远程监听：必须配置 AdminKey（config 已强制），所有请求校验 Bearer 密钥。
//   - 密钥比对使用常量时间比较，避免时序侧信道。
package session

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"

	"midroute/internal/errs"
)

// Guard 判定请求是否被授权。
type Guard struct {
	localOnly bool
	adminKey  string
}

// NewGuard 创建安全守卫。localOnly=true 时仅 loopback 免登录；否则必须携带 adminKey。
func NewGuard(localOnly bool, adminKey string) *Guard {
	return &Guard{localOnly: localOnly, adminKey: adminKey}
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
// 规则：
//   - 远程来源永远需要有效管理员密钥。
//   - 本地来源：localOnly 模式下免登录；否则同样需要密钥。
func (g *Guard) Authorize(r *http.Request) *errs.Error {
	loopback := IsLoopback(r)
	if loopback && g.localOnly {
		return nil
	}
	// 需要密钥：从 Authorization: Bearer <token> 提取
	token := extractBearer(r.Header.Get("Authorization"))
	if g.adminKey == "" {
		return errs.New(errs.CodeForbidden, "未配置管理员密钥，拒绝远程访问")
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(g.adminKey)) != 1 {
		return errs.New(errs.CodeUnauthorized, "管理员密钥无效")
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
