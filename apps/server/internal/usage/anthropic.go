package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const anthropicBase = "https://api.anthropic.com"

// AnthropicAdapter 读取 Anthropic 组织级 Usage/Cost API。
// 需要 Admin API Key（sk-ant-admin*）或管理范围 OAuth 凭据。
type AnthropicAdapter struct {
	baseURL string
	client  *http.Client
}

// AnthropicOptions 可选配置。
type AnthropicOptions struct {
	BaseURL string
	Client  *http.Client
}

// NewAnthropicAdapter 创建 Anthropic 适配器。
func NewAnthropicAdapter(opts AnthropicOptions) *AnthropicAdapter {
	a := &AnthropicAdapter{baseURL: anthropicBase, client: http.DefaultClient}
	if opts.BaseURL != "" {
		a.baseURL = strings.TrimRight(opts.BaseURL, "/")
	}
	if opts.Client != nil {
		a.client = opts.Client
	}
	return a
}

func (a *AnthropicAdapter) Provider() Provider { return ProviderAnthropic }

func (a *AnthropicAdapter) Capabilities() []Capability {
	return []Capability{CapVerifyCredential, CapFetchUsage, CapFetchCost}
}

func (a *AnthropicAdapter) isAdminKey(key string) bool {
	return strings.HasPrefix(key, "sk-ant-admin")
}

func (a *AnthropicAdapter) CapabilityMatrix(ctx context.Context, key string) ([]CapabilityResult, error) {
	now := time.Now().Unix()
	results := []CapabilityResult{
		{Capability: CapVerifyCredential, Supported: a.isAdminKey(key), TestedAt: now,
			Reason: orUnsupported(a.isAdminKey(key), "需要 Admin API Key（sk-ant-admin*）或管理范围 OAuth 凭据")},
		{Capability: CapFetchUsage, Supported: a.isAdminKey(key), TestedAt: now,
			Reason: orUnsupported(a.isAdminKey(key), "组织级 Usage API 需要 Admin 权限")},
		{Capability: CapFetchCost, Supported: a.isAdminKey(key), TestedAt: now,
			Reason: orUnsupported(a.isAdminKey(key), "组织级 Cost API 需要 Admin 权限")},
	}
	if a.isAdminKey(key) && !isPlaceholder(key) {
		if err := a.probe(ctx, key); err != nil {
			for i := range results {
				results[i].Supported = false
				if results[i].Reason == "" {
					results[i].Reason = "校验失败: " + err.Error()
				}
			}
		}
	}
	return results, nil
}

func (a *AnthropicAdapter) probe(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/v1/organizations/usage_report/messages", nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusMethodNotAllowed {
		// 端点存在（可能是 POST 方法），Admin 凭据被接受
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("HTTP %d: 无 Admin 权限", resp.StatusCode)
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}

func (a *AnthropicAdapter) FetchUsage(ctx context.Context, key string, window Window) (*Usage, error) {
	if isPlaceholder(key) {
		return estimatedUsage(ProviderAnthropic, window, "dry-run：未携带真实 Admin 凭据，用量为估算"), nil
	}
	if !a.isAdminKey(key) {
		return unavailableUsage(ProviderAnthropic, window, "需要 Admin API Key（sk-ant-admin*）"), nil
	}
	payload := fmt.Sprintf(`{"type":"usage_report","period":{"granularity":"day","start":"%s","end":"%s"}}`,
		window.Start.Format("2006-01-02"), window.End.Format("2006-01-02"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/organizations/usage_report/messages",
		strings.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return unavailableUsage(ProviderAnthropic, window, fmt.Sprintf("HTTP %d：无 Admin 权限", resp.StatusCode)), nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic usage: HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var parsed struct {
		Total struct {
			InputTokens      int64 `json:"input_tokens"`
			OutputTokens     int64 `json:"output_tokens"`
			CacheReadTokens  int64 `json:"cache_read_tokens"`
			CacheWriteTokens int64 `json:"cache_write_tokens"`
		} `json:"total"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("anthropic usage: parse: %w", err)
	}
	u := &Usage{
		Provider:    ProviderAnthropic,
		WindowStart: window.Start,
		WindowEnd:   window.End,
		Source:      SourceOfficial,
		Confidence:  ConfidenceExact,
		CollectedAt: time.Now().Unix(),
	}
	u.InputTokens = parsed.Total.InputTokens
	u.OutputTokens = parsed.Total.OutputTokens
	u.CacheTokens = parsed.Total.CacheReadTokens + parsed.Total.CacheWriteTokens
	return u, nil
}

func (a *AnthropicAdapter) FetchCost(_ context.Context, _ string, _ Window) (*Cost, error) {
	// Anthropic Cost 与 Usage 同一端点，量级为费用估算；这里明确标记估算。
	return &Cost{Provider: ProviderAnthropic, Source: SourceOfficial, Confidence: ConfidenceEstimated,
		Note: "Anthropic Cost 与 Usage 同源，金额需结合定价表估算", CollectedAt: time.Now().Unix()}, nil
}

func (a *AnthropicAdapter) FetchQuota(_ context.Context, _ string) (*Quota, error) {
	return nil, ErrUnsupported
}
