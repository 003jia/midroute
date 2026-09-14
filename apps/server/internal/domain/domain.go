// Package domain 定义 Midroute 自有领域模型（对齐 PRD §6.2 数据表）。
// 敏感信息规则：accounts 只保存 SecretRef 引用与指纹，绝不存明文凭据。
package domain

// Provider 平台类型与能力。
type Provider struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"` // openai | anthropic | gemini | openai-compatible
	Name         string `json:"name"`
	BaseURL      string `json:"base_url"`
	Capabilities string `json:"capabilities,omitempty"`
}

// AccountMode 账户模式：仅监测账户不参与请求转发（合约 v1 §accounts.mode）。
type AccountMode string

const (
	// ModeMonitorOnly 仅监测：只读取套餐/额度/健康，永不成为路由候选。
	ModeMonitorOnly AccountMode = "monitor_only"
	// ModeRelayAndMonitor 转发并监测：默认模式。
	ModeRelayAndMonitor AccountMode = "relay_and_monitor"
)

// AuthType 认证类型（合约 v1 §accounts.auth_type）。
type AuthType string

const (
	AuthAPIKey AuthType = "api_key"
	AuthOAuth  AuthType = "oauth"
	AuthManual AuthType = "manual"
)

// BillingMode 计费方式（合约 v1 §accounts.billing_mode）。
type BillingMode string

const (
	BillingSubscription BillingMode = "subscription"
	BillingMetered      BillingMode = "metered"
	BillingUnknown      BillingMode = "unknown"
)

// AuthState 认证可用状态（合约 v1 §accounts.auth_state）。
// 与 Status（active/disabled/error，账户启停）分列，不混用。
type AuthState string

const (
	AuthStateOK             AuthState = "ok"
	AuthStateExpired        AuthState = "expired"
	AuthStateReauthRequired AuthState = "reauth_required"
	AuthStateUnknown        AuthState = "unknown"
)

// CapabilityStatus 能力状态（合约 v1 §capability）。
// 平台声明支持 ≠ 当前凭据权限足够，二者分列保存。
// 注意：P0-1 扩展为五态 + error/unknown，不再只有三态。
type CapabilityStatus string

const (
	CapabilitySupported           CapabilityStatus = "supported"
	CapabilityUnsupported         CapabilityStatus = "unsupported"
	CapabilityPermissionRequired  CapabilityStatus = "permission_required"
	CapabilityReauthRequired      CapabilityStatus = "reauth_required"
	CapabilityError               CapabilityStatus = "error"
	CapabilityUnknown             CapabilityStatus = "unknown"
)

// CapabilityName 能力名称（账户能力矩阵）。
type CapabilityName string

const (
	CapVerifyCredential CapabilityName = "verifyCredential"
	CapDiscoverModels   CapabilityName = "discoverModels"
	CapForward          CapabilityName = "forward"
	CapOAuth            CapabilityName = "oauth"
	CapSubscription     CapabilityName = "subscription"
	CapQuota            CapabilityName = "quota"
	CapRefresh          CapabilityName = "refresh"
	CapProbeHealth      CapabilityName = "probeHealth"
)

// AccountCapability 账户单项能力结果（不保存原始认证响应）。
type AccountCapability struct {
	AccountID        string          `json:"account_id"`
	Capability       CapabilityName  `json:"capability"`
	Status           CapabilityStatus `json:"status"`
	Reason           string          `json:"reason,omitempty"`
	ConnectorVersion string          `json:"connector_version"`
	CheckedAt        string          `json:"checked_at"`
}

// Account 平台账户。凭据仅以 SecretRef 引用形式存在。
type Account struct {
	ID                string `json:"id"`
	ProviderID        string `json:"provider_id"`
	Name              string `json:"name"`
	Status            string `json:"status"`       // active | disabled | error
	Mode              string `json:"mode"`         // monitor_only | relay_and_monitor
	AuthType          string `json:"auth_type"`    // api_key | oauth | manual
	BillingMode       string `json:"billing_mode"` // subscription | metered | unknown
	AuthState         string `json:"auth_state"`   // ok | expired | reauth_required | unknown
	VaultProvider     string `json:"vault_provider"`
	SecretService     string `json:"secret_service"`
	SecretAccount     string `json:"secret_account"`
	SecretFingerprint string `json:"secret_fingerprint"`
	LastVerifiedAt    string `json:"last_verified_at"`
}

// Model 逻辑/上游模型。
type Model struct {
	ID             string `json:"id"`
	ProviderID     string `json:"provider_id"`
	UpstreamID     string `json:"upstream_id"`
	CanonicalAlias string `json:"canonical_alias,omitempty"`
	ContextLimit   int64  `json:"context_limit"`
	Capabilities   string `json:"capabilities,omitempty"`
}

// AccountModel 账户-模型可用性。
type AccountModel struct {
	AccountID string `json:"account_id"`
	ModelID   string `json:"model_id"`
	Listed    bool   `json:"listed"`
	Probed    bool   `json:"probed"`
	Usable    bool   `json:"usable"`
}

// RoutingPolicy 逻辑模型 → 候选路由策略。
type RoutingPolicy struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Alias      string      `json:"alias"` // 逻辑模型名，如 coding-fast
	Candidates []Candidate `json:"candidates"`
	Enabled    bool        `json:"enabled"`
}

// Candidate 一个候选目标（账户 + 上游模型）。
type Candidate struct {
	AccountID string `json:"account_id"`
	ModelID   string `json:"model_id"` // 上游模型 ID（透传给上游）
	Priority  int    `json:"priority"` // 数字越小优先级越高
	Weight    int    `json:"weight"`
}

// Token 推理访问令牌（PRD M2 数据模型；MR-016）。
// 只保存哈希与前缀；明文仅在创建响应中出现一次，绝不落库/落日志。
type Token struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	KeyHash    string `json:"-"` // 不序列化
	KeyPrefix  string `json:"key_prefix"`
	Enabled    bool   `json:"enabled"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at"`
	// ProjectID 所属项目；空串为未分组的默认作用域（migration v4 兼容）。
	ProjectID string `json:"project_id"`
	// ModelWhitelist 模型白名单（JSON 数组字符串）；空数组表示不限制。
	ModelWhitelist string `json:"model_whitelist"`
	// ExpiresAt 到期时间（RFC3339）；空串表示长期有效。
	ExpiresAt string `json:"expires_at"`
	// MaxConcurrency 最大并发推理请求数；0 表示不限制。
	MaxConcurrency int `json:"max_concurrency"`
}

// Project 推理项目（MR-016）：访问令牌的分组与预算载体。
type Project struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ResponseBinding Responses 协议会话句柄绑定（MR-013）：
// previous_response_id 与产生它的账户严格绑定，跨账户不可续接。
type ResponseBinding struct {
	ResponseID string `json:"response_id"`
	AccountID  string `json:"account_id"`
	ModelID    string `json:"model_id"`
	CreatedAt  string `json:"created_at"`
	ExpiresAt  string `json:"expires_at"`
}
