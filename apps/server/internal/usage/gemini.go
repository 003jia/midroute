package billing

import (
	"context"
	"time"
)

// GeminiAdapter 读取 Gemini 用量/额度/账单。
// Gemini 限额以项目为单位（RPM/TPM/RPD），用量、额度和账单自动读取
// 需要相应 Google Cloud Monitoring/Quota/Billing IAM 权限。
// V0.1 无 GCP 凭据时明确返回 unsupported，不使用 0 伪装成功。
type GeminiAdapter struct{}

// NewGeminiAdapter 创建 Gemini 适配器。
func NewGeminiAdapter() *GeminiAdapter { return &GeminiAdapter{} }

func (a *GeminiAdapter) Provider() Provider { return ProviderGemini }

func (a *GeminiAdapter) Capabilities() []Capability {
	return []Capability{CapVerifyCredential, CapFetchUsage, CapFetchQuota}
}

func (a *GeminiAdapter) CapabilityMatrix(_ context.Context, _ string) ([]CapabilityResult, error) {
	now := time.Now().Unix()
	reason := "需要 Google Cloud Monitoring/Quota/Billing IAM 权限；V0.1 未配置 GCP 服务账号，不做无凭据扫描"
	return []CapabilityResult{
		{Capability: CapVerifyCredential, Supported: false, Reason: reason, TestedAt: now},
		{Capability: CapFetchUsage, Supported: false, Reason: reason, TestedAt: now},
		{Capability: CapFetchQuota, Supported: false, Reason: reason, TestedAt: now},
	}, nil
}

func (a *GeminiAdapter) FetchUsage(_ context.Context, _ string, w Window) (*Usage, error) {
	return unavailableUsage(ProviderGemini, w, "需要 Google Cloud Monitoring 权限"), nil
}

func (a *GeminiAdapter) FetchCost(_ context.Context, _ string, _ Window) (*Cost, error) {
	return &Cost{Provider: ProviderGemini, Source: SourceOfficial, Confidence: ConfidenceUnavailable,
		Note: "需要 Google Cloud Billing 权限", CollectedAt: time.Now().Unix()}, nil
}

func (a *GeminiAdapter) FetchQuota(_ context.Context, _ string) (*Quota, error) {
	return nil, ErrUnsupported
}
