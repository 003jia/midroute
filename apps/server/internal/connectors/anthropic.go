package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// AnthropicConnector 实现 OpenAI-compatible 入参 → Anthropic Messages 协议转换。
type AnthropicConnector struct {
	client *http.Client
}

// NewAnthropicConnector 创建 Anthropic 连接器。
func NewAnthropicConnector(client *http.Client) *AnthropicConnector {
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	return &AnthropicConnector{client: client}
}

func (c *AnthropicConnector) Kind() ProviderKind { return KindAnthropic }

func (c *AnthropicConnector) Capabilities() []Capability {
	return []Capability{CapValidateCredential, CapDiscoverModels, CapForward, CapStream, CapProbeHealth}
}

// Anthropic 静态模型目录（M2 版本，标注版本；FR-4 允许静态目录）。
var anthropicCatalog = []string{
	"claude-3-5-haiku-20241022",
	"claude-3-5-sonnet-20241022",
	"claude-3-5-sonnet-20240620",
	"claude-sonnet-4-20250514",
	"claude-opus-4-20250514",
	"claude-3-opus-20240229",
	"claude-3-haiku-20240307",
}

func (c *AnthropicConnector) headers(t Target) map[string]string {
	return map[string]string{
		"x-api-key":         t.APIKey,
		"anthropic-version": "2023-06-01",
	}
}

func (c *AnthropicConnector) ValidateCredential(ctx context.Context, t Target) error {
	body := map[string]any{
		"model":      anthropicCatalog[0],
		"max_tokens": 1,
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
	}
	resp, err := httpDo(ctx, c.client, http.MethodPost, baseURL(t)+"/v1/messages", c.headers(t), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("凭据无效（HTTP %d）", resp.StatusCode)
	}
	// 已通过鉴权，但请求被拒（模型/参数），视为可用的弱验证
	return nil
}

func (c *AnthropicConnector) DiscoverModels(_ context.Context, _ Target) ([]ModelInfo, error) {
	out := make([]ModelInfo, 0, len(anthropicCatalog))
	for _, id := range anthropicCatalog {
		out = append(out, ModelInfo{UpstreamID: id})
	}
	return out, nil
}

// anthropicRequest Anthropic Messages 请求。
type anthropicRequest struct {
	Model         string         `json:"model"`
	MaxTokens     int            `json:"max_tokens"`
	System        string         `json:"system,omitempty"`
	Messages      []anthropicMsg `json:"messages"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	StopSequences []string       `json:"stop_sequences,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	Tools         []any          `json:"tools,omitempty"`
}

type anthropicMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string 或 blocks
}

type anthropicResponse struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

func convertToAnthropic(req *ChatRequest) anthropicRequest {
	out := anthropicRequest{
		Model:     req.Model,
		MaxTokens: 4096,
		Messages:  []anthropicMsg{},
	}
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		out.MaxTokens = *req.MaxTokens
	}
	out.Temperature = req.Temperature
	out.TopP = req.TopP
	out.StopSequences = req.Stop
	out.Tools = req.Tools
	var system []string
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			system = append(system, m.Content)
		case "tool":
			out.Messages = append(out.Messages, anthropicMsg{Role: "user", Content: []map[string]any{
				{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content},
			}})
		default:
			out.Messages = append(out.Messages, anthropicMsg{Role: m.Role, Content: m.Content})
		}
	}
	out.System = strings.Join(system, "\n")
	return out
}

func convertFromAnthropic(resp *anthropicResponse, model string) *ChatResponse {
	out := &ChatResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
	}
	var text strings.Builder
	for _, b := range resp.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	finish := mapAnthropicStopReason(resp.StopReason)
	out.Choices = []struct {
		Index        int         `json:"index"`
		Message      ChatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	}{{
		Index:        0,
		Message:      ChatMessage{Role: "assistant", Content: text.String()},
		FinishReason: finish,
	}}
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		cached := resp.Usage.CacheReadInputTokens + resp.Usage.CacheCreationInputTokens
		out.Usage = &UsageDTO{
			PromptTokens:        resp.Usage.InputTokens,
			CompletionTokens:    resp.Usage.OutputTokens,
			TotalTokens:         resp.Usage.InputTokens + resp.Usage.OutputTokens,
			PromptTokensDetails: &PromptTokensDetails{CachedTokens: cached},
		}
	}
	return out
}

func mapAnthropicStopReason(r string) string {
	switch r {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

func (c *AnthropicConnector) Forward(ctx context.Context, t Target, req *ChatRequest) (*ChatResponse, error) {
	aresp, err := c.forwardAnthropic(ctx, t, convertToAnthropic(req))
	if err != nil {
		return nil, err
	}
	return convertFromAnthropic(aresp, req.Model), nil
}

func (c *AnthropicConnector) forwardAnthropic(ctx context.Context, t Target, body anthropicRequest) (*anthropicResponse, error) {
	body.Stream = false
	resp, err := httpDo(ctx, c.client, http.MethodPost, baseURL(t)+"/v1/messages", c.headers(t), body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := readBytes(resp, 4096)
		return nil, classifyUpstreamError(resp.StatusCode, raw)
	}
	var out anthropicResponse
	if err := readJSON(&out, resp); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *AnthropicConnector) ForwardStream(ctx context.Context, t Target, req *ChatRequest, onChunk func([]byte) error) (*Usage, error) {
	body := convertToAnthropic(req)
	body.Stream = true
	resp, err := httpDo(ctx, c.client, http.MethodPost, baseURL(t)+"/v1/messages", c.headers(t), body)
	if err != nil {
		return nil, err
	}
	tr := newAnthropicStreamTranslator(req.Model, onChunk)
	usage, err := tr.translate(ctx, resp)
	if err != nil {
		return usage, err
	}
	return usage, nil
}

func (c *AnthropicConnector) ProbeHealth(ctx context.Context, t Target) (*HealthProbe, error) {
	start := time.Now()
	err := c.ValidateCredential(ctx, t)
	lat := time.Since(start).Milliseconds()
	if err != nil {
		return &HealthProbe{OK: false, LatencyMS: lat, Error: err.Error()}, nil
	}
	return &HealthProbe{OK: true, LatencyMS: lat}, nil
}

// anthropicStreamTranslator 将 Anthropic SSE 事件转换为 OpenAI chunk SSE。
type anthropicStreamTranslator struct {
	model    string
	onChunk  func([]byte) error
	id       string
	created  int64
	usage    *Usage
	finished bool
}

func newAnthropicStreamTranslator(model string, onChunk func([]byte) error) *anthropicStreamTranslator {
	return &anthropicStreamTranslator{model: model, onChunk: onChunk, created: time.Now().Unix()}
}

type anthropicStreamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	Message *struct {
		ID string `json:"id"`
	} `json:"message"`
}

func (t *anthropicStreamTranslator) emitChunk(delta map[string]any, finish string) error {
	chunk := map[string]any{
		"id":      t.id,
		"object":  "chat.completion.chunk",
		"created": t.created,
		"model":   t.model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	return t.onChunk(append([]byte("data: "), append(b, '\n', '\n')...))
}

func (t *anthropicStreamTranslator) translate(ctx context.Context, resp *http.Response) (*Usage, error) {
	err := consumeSSE(ctx, resp, func(raw []byte) error {
		block := string(raw)
		payload, ok := extractData(block)
		if !ok {
			return nil
		}
		if payload == "[DONE]" {
			return nil
		}
		var ev anthropicStreamEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return nil // 忽略不可解析事件
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				t.id = ev.Message.ID
			}
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				if err := t.emitChunk(map[string]any{"content": ev.Delta.Text}, ""); err != nil {
					return err
				}
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				finish := mapAnthropicStopReason(ev.Delta.StopReason)
				if err := t.emitChunk(map[string]any{"content": ""}, finish); err != nil {
					return err
				}
				t.finished = true
			}
			if ev.Usage != nil && t.usage == nil {
				t.usage = &Usage{InputTokens: ev.Usage.InputTokens, OutputTokens: ev.Usage.OutputTokens}
			}
		case "message_stop":
			if !t.finished {
				return t.emitChunk(map[string]any{"content": ""}, "stop")
			}
		}
		return nil
	})
	if t.usage == nil {
		t.usage = &Usage{}
	}
	return t.usage, err
}
