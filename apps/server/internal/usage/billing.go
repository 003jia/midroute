// Package billing 实现 Provider 官方组织 Usage/Cost/Quota 读取与对账（M2-G2）。
//
// 对齐计划 §6.4 Usage & Quota Collector 与 §9 首批适配器：
//   - 每个能力显式声明 supported / unsupported，不支持返回明确原因，不用 0 伪装成功。
//   - 每个数值带来源（official / observed / estimated / manual）与可信度
//     （exact / reported / estimated / unavailable）。
//   - 官方用量与本地观测分列展示，不盲目求和（Reconcile 只对账）。
//   - Gemini 需要 Google Cloud Monitoring/Quota/Billing 权限，无凭据时返回 unsupported。
package billing

import (
	"context"
	"errors"
	"time"
)

// Provider 平台标识。
type Provider string

const (
	ProviderOpenAI    Provider = "openai"
	ProviderAnthropic Provider = "anthropic"
	ProviderGemini    Provider = "gemini"
)

// Capability 适配器能力。
type Capability string

const (
	CapVerifyCredential Capability = "verifyCredential"
	CapFetchUsage       Capability = "fetchUsage"
	CapFetchCost        Capability = "fetchCost"
	CapFetchQuota       Capability = "fetchQuota"
	CapFetchPlanWindows Capability = "fetchPlanWindows"
	CapFetchStatus      Capability = "fetchOfficialStatus"
)

// Source 数据来源。
type Source string

const (
	SourceOfficial  Source = "official"  // Provider 官方 API
	SourceObserved  Source = "observed"  // 经本地网关观测
	SourceEstimated Source = "estimated" // tokenizer+定价表估算
	SourceManual    Source = "manual"    // 用户手工录入
)

// Confidence 可信度。
type Confidence string

const (
	ConfidenceExact       Confidence = "exact"
	ConfidenceReported    Confidence = "reported"
	ConfidenceEstimated   Confidence = "estimated"
	ConfidenceUnavailable Confidence = "unavailable"
)

// Window 时间窗口。
type Window struct {
	Start time.Time
	End   time.Time
}

// CapabilityResult 单个能力评估结果。
type CapabilityResult struct {
	Capability Capability `json:"capability"`
	Supported  bool       `json:"supported"`
	Reason     string     `json:"reason,omitempty"`
	TestedAt   int64      `json:"tested_at"`
}

// Usage 用量。
type Usage struct {
	Provider     Provider   `json:"provider"`
	WindowStart  time.Time  `json:"window_start"`
	WindowEnd    time.Time  `json:"window_end"`
	InputTokens  int64      `json:"input_tokens"`
	OutputTokens int64      `json:"output_tokens"`
	CacheTokens  int64      `json:"cache_tokens"`
	Requests     int64      `json:"requests"`
	Source       Source     `json:"source"`
	Confidence   Confidence `json:"confidence"`
	Note         string     `json:"note,omitempty"`
	CollectedAt  int64      `json:"collected_at"`
}

// Cost 费用。
type Cost struct {
	Provider    Provider   `json:"provider"`
	Amount      float64    `json:"amount"`
	Currency    string     `json:"currency"`
	Source      Source     `json:"source"`
	Confidence  Confidence `json:"confidence"`
	Note        string     `json:"note,omitempty"`
	CollectedAt int64      `json:"collected_at"`
}

// Quota 配额/额度。
type Quota struct {
	Provider    Provider   `json:"provider"`
	QuotaType   string     `json:"quota_type"` // rpm | tpm | rpd | spend | plan_window
	Limit       float64    `json:"limit"`
	Used        float64    `json:"used"`
	Remaining   float64    `json:"remaining"`
	ResetAt     time.Time  `json:"reset_at,omitempty"`
	Source      Source     `json:"source"`
	Confidence  Confidence `json:"confidence"`
	Note        string     `json:"note,omitempty"`
	CollectedAt int64      `json:"collected_at"`
}

// DiffEntry 对账差异项。
type DiffEntry struct {
	Metric         string     `json:"metric"`
	Official       float64    `json:"official"`
	Observed       float64    `json:"observed"`
	Diff           float64    `json:"diff"`
	OfficialConf   Confidence `json:"official_confidence"`
	ObservedConf   Confidence `json:"observed_confidence"`
	Interpretation string     `json:"interpretation"`
}

// Reconciliation 对账报告：官方与本地观测分列展示差异，不求和。
type Reconciliation struct {
	Provider  Provider    `json:"provider"`
	Window    Window      `json:"window"`
	Entries   []DiffEntry `json:"entries"`
	Generated int64       `json:"generated_at"`
}

// Adapter 是官方 Usage/Cost/Quota 读取适配器。
type Adapter interface {
	Provider() Provider
	Capabilities() []Capability
	// CapabilityMatrix 评估当前凭据支持的官方能力。
	CapabilityMatrix(ctx context.Context, key string) ([]CapabilityResult, error)
	// FetchUsage 读取官方用量。key 为空或为测试占位符时返回 estimated/unavailable 而不发起请求。
	FetchUsage(ctx context.Context, key string, window Window) (*Usage, error)
	// FetchCost 读取官方费用。
	FetchCost(ctx context.Context, key string, window Window) (*Cost, error)
	// FetchQuota 读取官方额度。
	FetchQuota(ctx context.Context, key string) (*Quota, error)
}

// ErrUnsupported 表示当前 Provider/凭据不支持该能力。
var ErrUnsupported = errors.New("billing: capability unsupported")

// NewAdapter 根据 Provider 创建官方读取适配器。
func NewAdapter(p Provider) Adapter {
	switch p {
	case ProviderOpenAI:
		return &OpenAIAdapter{}
	case ProviderAnthropic:
		return &AnthropicAdapter{}
	case ProviderGemini:
		return &GeminiAdapter{}
	default:
		return &unsupportedAdapter{provider: p}
	}
}

type unsupportedAdapter struct{ provider Provider }

func (a *unsupportedAdapter) Provider() Provider         { return a.provider }
func (a *unsupportedAdapter) Capabilities() []Capability { return []Capability{CapVerifyCredential} }
func (a *unsupportedAdapter) CapabilityMatrix(_ context.Context, _ string) ([]CapabilityResult, error) {
	return []CapabilityResult{{Capability: CapVerifyCredential, Supported: false, Reason: "unknown provider"}}, nil
}
func (a *unsupportedAdapter) FetchUsage(_ context.Context, _ string, _ Window) (*Usage, error) {
	return nil, ErrUnsupported
}
func (a *unsupportedAdapter) FetchCost(_ context.Context, _ string, _ Window) (*Cost, error) {
	return nil, ErrUnsupported
}
func (a *unsupportedAdapter) FetchQuota(_ context.Context, _ string) (*Quota, error) {
	return nil, ErrUnsupported
}
