// Package oauth 实现授权码 + PKCE 登录、Token 刷新与撤销边界（MR-007）。
//
// 设计要点（对齐实施清单 MR-007 与 §5.3 凭据规则）：
//   - state 随机、短期、单次消费，绑定本机授权会话；verifier 只保存在服务端内存。
//   - Token 以 JSON bundle 整体存入 Vault（Keychain），业务表仅持 SecretRef。
//   - 刷新按账户单飞：并发请求只触发一次上游刷新；成功后写入新凭据引用，
//     暂时失败保留旧凭据；确认失效由调用方标记 reauth_required。
//   - 平台不支持官方撤销端点时显式返回 ErrRevokeUnsupported，只做本地断开。
//   - 非持久 Vault（内存实现）直接拒绝发起授权，不以重启即丢的库伪装持久连接。
//
// 代码来源：端点常量与流程参考 work/CLIProxyAPI internal/auth/codex（冻结提交
// 17a65ee5470fbaf0e22fc219381e6a4ae9e07624，MIT），按 Midroute 接口改写，
// 未复制原文件；登记见 docs/source-ledger.md。
package oauth

import (
	"errors"
)

// Provider 描述一个 OAuth 平台的端点与客户端参数。
type Provider struct {
	// Name 平台标识（如 codex）。
	Name string
	// AuthURL 授权端点。
	AuthURL string
	// TokenURL 令牌端点（交换与刷新共用）。
	TokenURL string
	// ClientID 平台分配的公共客户端 ID。
	ClientID string
	// AuthScope 授权请求 scope。
	AuthScope string
	// RefreshScope 刷新请求 scope（Codex 与授权 scope 不同）。
	RefreshScope string
	// RedirectURI 本机回调地址。
	RedirectURI string
}

// Codex 是 OpenAI Codex/ChatGPT 的 OAuth 参数（证据：上游 openai_auth.go:24-30）。
var Codex = Provider{
	Name:         "codex",
	AuthURL:      "https://auth.openai.com/oauth/authorize",
	TokenURL:     "https://auth.openai.com/oauth/token",
	ClientID:     "app_EMoamEEZ73f0CkXaXp7hrann",
	AuthScope:    "openid email profile offline_access",
	RefreshScope: "openid profile email",
	RedirectURI:  "http://localhost:1455/auth/callback",
}

// TokenBundle 是一次授权/刷新得到的完整令牌集。整体序列化后存入 Vault。
type TokenBundle struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
	Email        string `json:"email,omitempty"`
	// ExpiresAt 令牌失效时间（RFC3339 UTC）。
	ExpiresAt string `json:"expires_at"`
}

// OAuth 错误。错误值不含令牌与原始响应正文（§5.3 凭据不进异常原文）。
var (
	// ErrStateInvalid state 不存在、不匹配或已被消费。
	ErrStateInvalid = errors.New("oauth: invalid or consumed state")
	// ErrStateExpired state 已过期，需要重新发起授权。
	ErrStateExpired = errors.New("oauth: state expired, restart authorization")
	// ErrAuthorizationCancelled 用户在平台侧取消授权（回调携带 error）。
	ErrAuthorizationCancelled = errors.New("oauth: authorization cancelled by user")
	// ErrRefreshFailed 刷新失败；旧凭据保留可用状态由调用方判定。
	ErrRefreshFailed = errors.New("oauth: token refresh failed")
	// ErrRefreshRejected 平台明确拒绝刷新（如 refresh_token 已轮换/失效），
	// 不可重试，账户应进入 reauth_required。
	ErrRefreshRejected = errors.New("oauth: refresh token rejected by provider")
	// ErrRevokeUnsupported 平台无已验证的撤销端点；只能本地断开。
	ErrRevokeUnsupported = errors.New("oauth: provider has no verified revoke endpoint")
	// ErrVaultNotPersistent Vault 非持久（内存实现），拒绝保存长期凭据。
	ErrVaultNotPersistent = errors.New("oauth: vault is not persistent; refusing to store long-lived tokens")
	// ErrUnknownProvider 请求的平台未注册。
	ErrUnknownProvider = errors.New("oauth: unknown provider")
)
