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

const openAIBase = "https://api.openai.com/v1"

// OpenAIAdapter 读取 OpenAI 组织级 Usage/Cost API。
// 需要组织级管理权限（sk-admin 系列凭据）；普通 API Key 返回不支持并说明原因。
type OpenAIAdapter struct {
	baseURL  string
	client   *http.Client
	dryRunOK bool // 测试注入时允许跳过网络
}

// OpenAIOptions 可选配置。
type OpenAIOptions struct {
	BaseURL string
	Client  *http.Client
}

// NewOpenAIAdapter 创建 OpenAI 适配器。
func NewOpenAIAdapter(opts OpenAIOptions) *OpenAIAdapter {
	a := &OpenAIAdapter{baseURL: openAIBase, client: http.DefaultClient}
	if opts.BaseURL != "" {
		a.baseURL = strings.TrimRight(opts.BaseURL, "/")
	}
	if opts.Client != nil {
		a.client = opts.Client
	}
	return a
}

func (a *OpenAIAdapter) Provider() Provider { return ProviderOpenAI }

func (a *OpenAIAdapter) Capabilities() []Capability {
	return []Capability{CapVerifyCredential, CapFetchUsage, CapFetchCost}
}

func (a *OpenAIAdapter) isAdminKey(key string) bool {
	return strings.HasPrefix(key, "sk-admin-") || strings.HasPrefix(key, "sk-proj-")
}

func (a *OpenAIAdapter) CapabilityMatrix(ctx context.Context, key string) ([]CapabilityResult, error) {
	now := time.Now().Unix()
	results := []CapabilityResult{
		{Capability: CapVerifyCredential, Supported: a.isAdminKey(key), TestedAt: now,
			Reason: orUnsupported(a.isAdminKey(key), "需要组织级管理凭据（sk-admin-*），普通 API Key 无读取权限")},
		{Capability: CapFetchUsage, Supported: a.isAdminKey(key), TestedAt: now,
			Reason: orUnsupported(a.isAdminKey(key), "Usage API 需要组织级管理权限")},
		{Capability: CapFetchCost, Supported: a.isAdminKey(key), TestedAt: now,
			Reason: orUnsupported(a.isAdminKey(key), "Cost 端点用于账单对账，需要组织级管理权限")},
	}
	// 尝试用真实凭据校验（仅在具备组织权限形态时）
	if a.isAdminKey(key) && !isPlaceholder(key) {
		if err := a.probe(ctx, key); err != nil {
			results[0].Supported = false
			results[0].Reason = "校验失败: " + err.Error()
			results[1].Supported = false
			results[2].Supported = false
		}
	}
	return results, nil
}

func (a *OpenAIAdapter) probe(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/organization/usage/completions?start_time=1&bucket_width=1d", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("HTTP %d: 无组织级 Usage 权限", resp.StatusCode)
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}

func (a *OpenAIAdapter) FetchUsage(ctx context.Context, key string, window Window) (*Usage, error) {
	if isPlaceholder(key) {
		return estimatedUsage(ProviderOpenAI, window, "dry-run：未携带真实管理凭据，用量为估算"), nil
	}
	if !a.isAdminKey(key) {
		return unavailableUsage(ProviderOpenAI, window, "需要组织级管理凭据（sk-admin-*）"), nil
	}
	start := window.Start.Unix()
	end := window.End.Unix()
	if end <= start {
		end = start + 86400
	}
	url := fmt.Sprintf("%s/organization/usage/completions?start_time=%d&end_time=%d&bucket_width=1d", a.baseURL, start, end)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return unavailableUsage(ProviderOpenAI, window, fmt.Sprintf("HTTP %d：无组织级 Usage 权限", resp.StatusCode)), nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai usage: HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var parsed struct {
		Data []struct {
			StartTime int64 `json:"start_time"`
			Results   []struct {
				InputTokens       int64 `json:"input_tokens"`
				OutputTokens      int64 `json:"output_tokens"`
				InputCachedTokens int64 `json:"input_cached_tokens"`
				NumRequests       int64 `json:"num_requests"`
			} `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("openai usage: parse: %w", err)
	}
	u := &Usage{
		Provider:    ProviderOpenAI,
		WindowStart: window.Start,
		WindowEnd:   window.End,
		Source:      SourceOfficial,
		Confidence:  ConfidenceExact,
		CollectedAt: time.Now().Unix(),
	}
	for _, d := range parsed.Data {
		for _, r := range d.Results {
			u.InputTokens += r.InputTokens
			u.OutputTokens += r.OutputTokens
			u.CacheTokens += r.InputCachedTokens
			u.Requests += r.NumRequests
		}
	}
	return u, nil
}

func (a *OpenAIAdapter) FetchCost(ctx context.Context, key string, window Window) (*Cost, error) {
	if isPlaceholder(key) {
		return &Cost{Provider: ProviderOpenAI, Source: SourceEstimated, Confidence: ConfidenceEstimated,
			Note: "dry-run：未携带真实管理凭据", CollectedAt: time.Now().Unix()}, nil
	}
	if !a.isAdminKey(key) {
		return &Cost{Provider: ProviderOpenAI, Source: SourceOfficial, Confidence: ConfidenceUnavailable,
			Note: "需要组织级管理凭据（sk-admin-*）", CollectedAt: time.Now().Unix()}, nil
	}
	start := window.Start.Unix()
	url := fmt.Sprintf("%s/organization/costs?start_time=%d&bucket_width=1d", a.baseURL, start)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai cost: HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var parsed struct {
		Data []struct {
			Amount struct {
				Value    float64 `json:"value"`
				Currency string  `json:"currency"`
			} `json:"amount"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("openai cost: parse: %w", err)
	}
	c := &Cost{Provider: ProviderOpenAI, Currency: "usd", Source: SourceOfficial, Confidence: ConfidenceExact, CollectedAt: time.Now().Unix()}
	for _, d := range parsed.Data {
		c.Amount += d.Amount.Value
		if d.Amount.Currency != "" {
			c.Currency = d.Amount.Currency
		}
	}
	return c, nil
}

func (a *OpenAIAdapter) FetchQuota(_ context.Context, _ string) (*Quota, error) {
	return nil, ErrUnsupported
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func orUnsupported(ok bool, reason string) string {
	if ok {
		return ""
	}
	return reason
}

// isPlaceholder 判断测试/占位凭据，避免真实网络调用。
func isPlaceholder(key string) bool {
	return key == "" || strings.HasPrefix(key, "sk-placeholder") || strings.HasPrefix(key, "dry-run")
}

func unavailableUsage(p Provider, w Window, note string) *Usage {
	return &Usage{Provider: p, WindowStart: w.Start, WindowEnd: w.End,
		Source: SourceOfficial, Confidence: ConfidenceUnavailable, Note: note, CollectedAt: time.Now().Unix()}
}

func estimatedUsage(p Provider, w Window, note string) *Usage {
	return &Usage{Provider: p, WindowStart: w.Start, WindowEnd: w.End,
		Source: SourceEstimated, Confidence: ConfidenceEstimated, Note: note, CollectedAt: time.Now().Unix()}
}
