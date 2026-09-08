package connectors

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OpenAICompatibleConnector 通用 OpenAI-compatible 平台（也用于 OpenAI 官方）。
// 入参/响应/SSE 已与 OpenAI 兼容，仅做鉴权头替换、模型透传与错误映射。
type OpenAICompatibleConnector struct {
	client *http.Client
	kind   ProviderKind
}

// NewOpenAICompatibleConnector 创建 OpenAI-compatible 连接器。
func NewOpenAICompatibleConnector(client *http.Client, kind ProviderKind) *OpenAICompatibleConnector {
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	return &OpenAICompatibleConnector{client: client, kind: kind}
}

func (c *OpenAICompatibleConnector) Kind() ProviderKind { return c.kind }

func (c *OpenAICompatibleConnector) Capabilities() []Capability {
	return []Capability{CapValidateCredential, CapDiscoverModels, CapForward, CapStream, CapProbeHealth}
}

func (c *OpenAICompatibleConnector) authHeader(t Target) map[string]string {
	return map[string]string{"Authorization": "Bearer " + t.APIKey}
}

func baseURL(t Target) string {
	return strings.TrimRight(t.BaseURL, "/")
}

func (c *OpenAICompatibleConnector) ValidateCredential(ctx context.Context, t Target) error {
	resp, err := httpDo(ctx, c.client, http.MethodGet, baseURL(t)+"/v1/models", c.authHeader(t), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("凭据无效（HTTP %d）", resp.StatusCode)
	default:
		return fmt.Errorf("校验失败（HTTP %d）", resp.StatusCode)
	}
}

func (c *OpenAICompatibleConnector) DiscoverModels(ctx context.Context, t Target) ([]ModelInfo, error) {
	resp, err := httpDo(ctx, c.client, http.MethodGet, baseURL(t)+"/v1/models", c.authHeader(t), nil)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := readJSON(&parsed, resp); err != nil {
		return nil, err
	}
	res := make([]ModelInfo, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		res = append(res, ModelInfo{UpstreamID: m.ID})
	}
	return res, nil
}

func (c *OpenAICompatibleConnector) Forward(ctx context.Context, t Target, req *ChatRequest) (*ChatResponse, error) {
	req.Stream = false
	resp, err := httpDo(ctx, c.client, http.MethodPost, baseURL(t)+"/v1/chat/completions", c.authHeader(t), req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := readBytes(resp, 4096)
		return nil, classifyUpstreamError(resp.StatusCode, raw)
	}
	var out ChatResponse
	if err := readJSON(&out, resp); err != nil {
		return nil, err
	}
	if out.Model == "" {
		out.Model = req.Model
	}
	return &out, nil
}

func (c *OpenAICompatibleConnector) ForwardStream(ctx context.Context, t Target, req *ChatRequest, onChunk func([]byte) error) (*Usage, error) {
	req.Stream = true
	resp, err := httpDo(ctx, c.client, http.MethodPost, baseURL(t)+"/v1/chat/completions", c.authHeader(t), req)
	if err != nil {
		return nil, err
	}
	done := false
	usage := &Usage{}
	err = consumeSSE(ctx, resp, func(b []byte) error {
		if bytesIndex(b, []byte("[DONE]")) >= 0 {
			done = true
		}
		return onChunk(b)
	})
	if err != nil {
		return usage, classifyUpstreamError(0, nil)
	}
	if !done {
		return usage, fmt.Errorf("upstream stream truncated")
	}
	return usage, nil
}

// bytesIndex 在 b 中查找子序列 sub 的位置；未找到返回 -1。
func bytesIndex(b, sub []byte) int {
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == string(sub) {
			return i
		}
	}
	return -1
}

func (c *OpenAICompatibleConnector) ProbeHealth(ctx context.Context, t Target) (*HealthProbe, error) {
	start := time.Now()
	err := c.ValidateCredential(ctx, t)
	lat := time.Since(start).Milliseconds()
	if err != nil {
		return &HealthProbe{OK: false, LatencyMS: lat, Error: err.Error()}, nil
	}
	return &HealthProbe{OK: true, LatencyMS: lat}, nil
}

// classifyUpstreamError 将上游错误归类为可重试/稳定语义（429、5xx、其他）。
func classifyUpstreamError(status int, body []byte) error {
	switch {
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("upstream rate_limited")
	case status >= 500 && status <= 599:
		return fmt.Errorf("upstream 5xx")
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("upstream auth")
	default:
		return fmt.Errorf("upstream error http_%d", status)
	}
}
