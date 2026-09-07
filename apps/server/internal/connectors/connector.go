// Package connectors 实现 PRD §6.1 Connector 合约与各平台接入。
// 统一对外使用 OpenAI-compatible 入参/响应/SSE；Anthropic/Gemini 在连接器内部完成协议转换。
package connectors

import (
	"context"
	"errors"
)

// ProviderKind 平台类型。
type ProviderKind string

const (
	KindOpenAI           ProviderKind = "openai"
	KindAnthropic        ProviderKind = "anthropic"
	KindGemini           ProviderKind = "gemini"
	KindOpenAICompatible ProviderKind = "openai-compatible"
)

// Capability 连接器能力（对齐 PRD FR-3/4/5，不支持的显式返回 unsupported）。
type Capability string

const (
	CapValidateCredential Capability = "validateCredential"
	CapDiscoverModels     Capability = "discoverModels"
	CapForward            Capability = "forward"
	CapStream             Capability = "stream"
	CapProbeHealth        Capability = "probeHealth"
	CapFetchQuota         Capability = "fetchQuota" // M4 接入，暂 unsupported
)

// Target 一次转发的目标描述。
type Target struct {
	ProviderKind ProviderKind
	BaseURL      string
	APIKey       string // 仅内存使用；由凭证库解密，不落库不落日志
	ModelID      string // 上游模型 ID
}

// Connector 契约（PRD §6.1 精简版，M2 覆盖验证/发现/转发/探测）。
type Connector interface {
	// Kind 返回平台类型。
	Kind() ProviderKind
	// Capabilities 声明支持的能力。
	Capabilities() []Capability
	// ValidateCredential 校验凭据是否可用。返回稳定错误。
	ValidateCredential(ctx context.Context, t Target) error
	// DiscoverModels 拉取该凭据可用的模型列表。
	DiscoverModels(ctx context.Context, t Target) ([]ModelInfo, error)
	// Forward 转发一次 OpenAI-compatible chat 请求。
	Forward(ctx context.Context, t Target, req *ChatRequest) (*ChatResponse, error)
	// ForwardStream 转发流式请求，逐块回传 OpenAI-compatible SSE 数据。
	ForwardStream(ctx context.Context, t Target, req *ChatRequest, onChunk func([]byte) error) (*Usage, error)
	// ProbeHealth 主动探测目标健康。
	ProbeHealth(ctx context.Context, t Target) (*HealthProbe, error)
}

// ModelInfo 上游模型信息。
type ModelInfo struct {
	UpstreamID   string
	ContextLimit int64
}

// Usage 用量元数据（不保存正文）。
type Usage struct {
	InputTokens     int64
	OutputTokens    int64
	CacheTokens     int64
	ReasoningTokens int64
}

// HealthProbe 探测结果。
type HealthProbe struct {
	OK        bool
	LatencyMS int64
	Status    int
	Error     string
}

// ErrUnsupported 能力不支持。
var ErrUnsupported = errors.New("connectors: capability unsupported")

// ErrQuotaExhausted 上游返回额度耗尽（429 + 特定头或体）。
var ErrQuotaExhausted = errors.New("connectors: quota exhausted")

// NewChatRequest 便捷构造。
func NewChatRequest(model string, msgs []ChatMessage) *ChatRequest {
	return &ChatRequest{Model: model, Messages: msgs}
}

// ChatMessage OpenAI-compatible 消息。
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall 工具调用。
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ChatRequest OpenAI-compatible chat/completions 请求（最小子集，透传扩展字段）。
type ChatRequest struct {
	Model         string         `json:"model"`
	Messages      []ChatMessage  `json:"messages"`
	Stream        bool           `json:"stream,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	MaxTokens     *int           `json:"max_tokens,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	Stop          []string       `json:"stop,omitempty"`
	Tools         []any          `json:"tools,omitempty"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
}

// StreamOptions 流式选项。
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ChatResponse OpenAI-compatible 响应。
type ChatResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int         `json:"index"`
		Message      ChatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *UsageDTO `json:"usage,omitempty"`
}

// UsageDTO OpenAI-compatible 用量表示。
type UsageDTO struct {
	PromptTokens            int64                    `json:"prompt_tokens"`
	CompletionTokens        int64                    `json:"completion_tokens"`
	TotalTokens             int64                    `json:"total_tokens"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// PromptTokensDetails 输入 token 明细。
type PromptTokensDetails struct {
	CachedTokens    int64 `json:"cached_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// CompletionTokensDetails 完成 token 明细。
type CompletionTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}
