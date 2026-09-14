package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GeminiConnector 实现 OpenAI-compatible 入参 → Gemini generateContent 协议转换。
// 限额以项目为单位（RPM/TPM/RPD）；用量/额度读取在 M4 接入（GCP IAM）。
type GeminiConnector struct {
	client *http.Client
}

// NewGeminiConnector 创建 Gemini 连接器。
func NewGeminiConnector(client *http.Client) *GeminiConnector {
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	return &GeminiConnector{client: client}
}

func (c *GeminiConnector) Kind() ProviderKind { return KindGemini }

func (c *GeminiConnector) Capabilities() []Capability {
	return []Capability{CapValidateCredential, CapDiscoverModels, CapForward, CapStream, CapProbeHealth}
}

func (c *GeminiConnector) headers(t Target) map[string]string {
	return map[string]string{"x-goog-api-key": t.APIKey}
}

// geminiModelsEndpoint 模型列表端点。
func (c *GeminiConnector) modelsURL(t Target) string {
	return baseURL(t) + "/v1beta/models"
}

func (c *GeminiConnector) generateURL(t Target, model string) string {
	return baseURL(t) + "/v1beta/models/" + url.PathEscape(model) + ":generateContent"
}

func (c *GeminiConnector) streamURL(t Target, model string) string {
	return baseURL(t) + "/v1beta/models/" + url.PathEscape(model) + ":streamGenerateContent?alt=sse"
}

func (c *GeminiConnector) ValidateCredential(ctx context.Context, t Target) error {
	resp, err := httpDo(ctx, c.client, http.MethodGet, c.modelsURL(t), c.headers(t), nil)
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

func (c *GeminiConnector) DiscoverModels(ctx context.Context, t Target) ([]ModelInfo, error) {
	resp, err := httpDo(ctx, c.client, http.MethodGet, c.modelsURL(t), c.headers(t), nil)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Models []struct {
			Name         string   `json:"name"`
			SupportedGen []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := readJSON(&parsed, resp); err != nil {
		return nil, err
	}
	res := []ModelInfo{}
	for _, m := range parsed.Models {
		// 仅保留支持 generateContent 的模型
		supported := false
		for _, g := range m.SupportedGen {
			if g == "generateContent" {
				supported = true
				break
			}
		}
		if !supported {
			continue
		}
		id := strings.TrimPrefix(m.Name, "models/")
		res = append(res, ModelInfo{UpstreamID: id})
	}
	return res, nil
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiRequest struct {
	Contents          []geminiContent `json:"contents"`
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	GenerationConfig  geminiConfig    `json:"generationConfig"`
}

type geminiConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount        int64 `json:"promptTokenCount"`
		CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
		CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
}

func convertToGemini(req *ChatRequest) geminiRequest {
	out := geminiRequest{Contents: []geminiContent{}}
	out.GenerationConfig.Temperature = req.Temperature
	out.GenerationConfig.TopP = req.TopP
	out.GenerationConfig.StopSequences = req.Stop
	if req.MaxTokens != nil {
		out.GenerationConfig.MaxOutputTokens = *req.MaxTokens
	}
	var system strings.Builder
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if system.Len() > 0 {
				system.WriteString("\n")
			}
			system.WriteString(m.Content)
		case "assistant":
			out.Contents = append(out.Contents, geminiContent{Role: "model", Parts: []geminiPart{{Text: m.Content}}})
		default:
			out.Contents = append(out.Contents, geminiContent{Role: "user", Parts: []geminiPart{{Text: m.Content}}})
		}
	}
	if system.Len() > 0 {
		out.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: system.String()}}}
	}
	return out
}

func convertFromGemini(resp *geminiResponse, model string) *ChatResponse {
	out := &ChatResponse{
		ID:      "chatcmpl-gemini",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
	}
	var text strings.Builder
	if len(resp.Candidates) > 0 {
		for _, p := range resp.Candidates[0].Content.Parts {
			text.WriteString(p.Text)
		}
	}
	out.Choices = []struct {
		Index        int         `json:"index"`
		Message      ChatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	}{{
		Index:        0,
		Message:      ChatMessage{Role: "assistant", Content: text.String()},
		FinishReason: mapGeminiFinish(resp.Candidates),
	}}
	if resp.UsageMetadata.PromptTokenCount > 0 || resp.UsageMetadata.CandidatesTokenCount > 0 {
		out.Usage = &UsageDTO{
			PromptTokens:     resp.UsageMetadata.PromptTokenCount,
			CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      resp.UsageMetadata.PromptTokenCount + resp.UsageMetadata.CandidatesTokenCount,
		}
	}
	return out
}

func mapGeminiFinish(cands []struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason"`
}) string {
	if len(cands) == 0 {
		return "stop"
	}
	switch cands[0].FinishReason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "OTHER":
		return "content_filter"
	default:
		return "stop"
	}
}

func (c *GeminiConnector) Forward(ctx context.Context, t Target, req *ChatRequest) (*ChatResponse, error) {
	gresp, err := c.forwardGemini(ctx, t, req.Model, convertToGemini(req), false)
	if err != nil {
		return nil, err
	}
	return convertFromGemini(gresp, req.Model), nil
}

func (c *GeminiConnector) forwardGemini(ctx context.Context, t Target, model string, body geminiRequest, stream bool) (*geminiResponse, error) {
	url := c.generateURL(t, model)
	if stream {
		url = c.streamURL(t, model)
	}
	resp, err := httpDo(ctx, c.client, http.MethodPost, url, c.headers(t), body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = readBytes(resp, 4096)
		return nil, Classify(resp.StatusCode, nil)
	}
	var out geminiResponse
	if err := readJSON(&out, resp); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *GeminiConnector) ForwardStream(ctx context.Context, t Target, req *ChatRequest, onChunk func([]byte) error) (*Usage, error) {
	body := convertToGemini(req)
	resp, err := httpDo(ctx, c.client, http.MethodPost, c.streamURL(t, req.Model), c.headers(t), body)
	if err != nil {
		return nil, err
	}
	tr := newGeminiStreamTranslator(req.Model, onChunk)
	usage, err := tr.translate(ctx, resp)
	if err != nil {
		return usage, err
	}
	return usage, nil
}

func (c *GeminiConnector) ProbeHealth(ctx context.Context, t Target) (*HealthProbe, error) {
	start := time.Now()
	err := c.ValidateCredential(ctx, t)
	lat := time.Since(start).Milliseconds()
	if err != nil {
		return &HealthProbe{OK: false, LatencyMS: lat, Error: err.Error()}, nil
	}
	return &HealthProbe{OK: true, LatencyMS: lat}, nil
}

// geminiStreamTranslator 将 Gemini SSE 转为 OpenAI chunk SSE。
type geminiStreamTranslator struct {
	model   string
	onChunk func([]byte) error
	created int64
	usage   *Usage
}

func newGeminiStreamTranslator(model string, onChunk func([]byte) error) *geminiStreamTranslator {
	return &geminiStreamTranslator{model: model, onChunk: onChunk, created: time.Now().Unix()}
}

func (t *geminiStreamTranslator) translate(ctx context.Context, resp *http.Response) (*Usage, error) {
	done := false
	err := consumeSSE(ctx, resp, func(raw []byte) error {
		block := string(raw)
		payload, ok := extractData(block)
		if !ok {
			return nil
		}
		if payload == "[DONE]" {
			return nil
		}
		var ev geminiResponse
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return nil
		}
		if len(ev.Candidates) > 0 && ev.Candidates[0].FinishReason != "" {
			done = true
		}
		text := ""
		if len(ev.Candidates) > 0 {
			for _, p := range ev.Candidates[0].Content.Parts {
				text += p.Text
			}
		}
		if text != "" {
			if err := t.emit(map[string]any{"content": text}); err != nil {
				return err
			}
		}
		if ev.UsageMetadata.PromptTokenCount > 0 || ev.UsageMetadata.CandidatesTokenCount > 0 {
			t.usage = &Usage{
				InputTokens:  ev.UsageMetadata.PromptTokenCount,
				OutputTokens: ev.UsageMetadata.CandidatesTokenCount,
				CacheTokens:  ev.UsageMetadata.CachedContentTokenCount,
			}
		}
		return nil
	})
	if t.usage == nil {
		t.usage = &Usage{}
	}
	if !done {
		return t.usage, ErrTruncated
	}
	return t.usage, err
}

func (t *geminiStreamTranslator) emit(delta map[string]any) error {
	chunk := map[string]any{
		"id":      "chatcmpl-gemini",
		"object":  "chat.completion.chunk",
		"created": t.created,
		"model":   t.model,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": ""}},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	return t.onChunk(append([]byte("data: "), append(b, '\n', '\n')...))
}
