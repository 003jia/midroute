// Package responses 实现 OpenAI Responses 协议的上游转发（MR-013 / FR-34）。
//
// 设计边界（对齐实施清单 MR-013）：
//   - 请求/响应以原始 JSON 透传为主，保真保留未知字段（工具调用、reasoning
//     等不在本层拆装）；仅 model 字段由调用方按路由结果改写；
//   - 流式为 SSE 逐事件原样转发，客户端取消向上游传播；
//   - 从 response.created / response.completed 事件提取 response_id 与 usage，
//     供会话句柄绑定与用量落库使用；解析失败不影响转发本身；
//   - 会话句柄（previous_response_id）的账户绑定由 api 层负责，本层不路由。
//
// 端点证据：https://chatgpt.com/backend-api/codex/responses（CLIProxyAPI
// 冻结提交 17a65ee，internal/runtime/executor/codex_executor_execute.go:32,76；
// 见 docs/provider-capabilities.md §2.2）。
package responses

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Target 一次 Responses 转发的上游目标。
type Target struct {
	// BaseURL 形如 https://chatgpt.com/backend-api/codex（不含 /responses）。
	BaseURL string
	// Token OAuth 访问令牌；仅内存使用，不落库不落日志。
	Token string
}

// Usage Responses 协议用量（Token 原义分列，未知为 0 且以 hasUsage 区分）。
type Usage struct {
	InputTokens     int64 `json:"input_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// Meta 从响应中提取的元数据。
type Meta struct {
	// ID response_id；用于会话句柄绑定。流式中可能为空（未收到 created/completed）。
	ID string
	// Model 上游回显的模型名（未回显为空）。
	Model string
	// Usage 用量；hasUsage=false 表示响应未携带（未知，不得当作 0 记账）。
	Usage    *Usage
	HasUsage bool
}

// StatusError 上游返回非 2xx。Body 为截断的错误体（可能含上游错误详情，
// 只用于映射错误码，不得原样进入日志）。
type StatusError struct {
	Status int
	Code   string // 解析出的 error.code / error.type（可空）
	Body   string // 截断 512 字节
}

func (e *StatusError) Error() string {
	return "responses: upstream status " + strconv.Itoa(e.Status)
}

// RateLimited 便捷判定 429。
func (e *StatusError) RateLimited() bool { return e.Status == http.StatusTooManyRequests }

// maxBody 限制读取的错误体与流单行长度，防内存放大。
const maxBody = 4 << 20

// Forward 转发一次 Responses 请求。
// body 必须是合法 JSON 对象（调用方已完成 model 改写）。
// stream=true 时 onEvent 收到逐行原样字节（含 event:/data: 行），返回前保证
// 已回调完全部输出；非流式时响应 JSON 整体作为单个 onEvent 参数回调一次。
// ctx 取消（客户端断开）会传播到上游并中止读取。
func Forward(ctx context.Context, client *http.Client, t Target, body []byte, stream bool, onEvent func([]byte) error) (*Meta, error) {
	if client == nil {
		client = http.DefaultClient
	}
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(t.BaseURL, "/")+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("responses: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+t.Token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("responses: upstream request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, parseStatusError(resp)
	}

	meta := &Meta{}
	if !stream {
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		if err != nil {
			return meta, fmt.Errorf("responses: read body: %w", err)
		}
		if err := onEvent(data); err != nil {
			return meta, err
		}
		extractMeta(data, meta)
		return meta, nil
	}
	return meta, streamSSE(ctx, resp.Body, onEvent, meta)
}

// streamSSE 逐行读取 SSE 并原样回调；同时嗅探 data 行提取元数据。
func streamSSE(ctx context.Context, body io.Reader, onEvent func([]byte) error, meta *Meta) error {
	br := bufio.NewReaderSize(body, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if werr := onEvent(line); werr != nil {
				// 客户端写失败（断开）：返回错误让上层停止并释放上游
				return werr
			}
			if trimmed := bytes.TrimRight(line, "\r\n"); bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(trimmed[len("data:"):])
				if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
					extractMeta(payload, meta)
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("responses: read stream: %w", err)
		}
	}
}

// extractMeta 从单个 JSON 载荷提取 id/model/usage。
// 两种形态：流式事件（字段在 response 容器内）与非流式整体响应（字段在顶层）。
// delta 等无元数据事件解析为零值，无副作用。
func extractMeta(data []byte, meta *Meta) {
	var ev struct {
		Type     string          `json:"type"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return
	}
	src := ev.Response
	if len(src) == 0 {
		// 非流式整体响应：顶层即 response 对象
		src = data
	}
	var robj struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens        int64 `json:"input_tokens"`
			InputTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokens        int64 `json:"output_tokens"`
			OutputTokensDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(src, &robj); err != nil {
		return
	}
	if robj.ID != "" {
		meta.ID = robj.ID
	}
	if robj.Model != "" {
		meta.Model = robj.Model
	}
	if robj.Usage != nil {
		u := &Usage{
			InputTokens:  robj.Usage.InputTokens,
			OutputTokens: robj.Usage.OutputTokens,
		}
		if robj.Usage.InputTokensDetails != nil {
			u.CachedTokens = robj.Usage.InputTokensDetails.CachedTokens
		}
		if robj.Usage.OutputTokensDetails != nil {
			u.ReasoningTokens = robj.Usage.OutputTokensDetails.ReasoningTokens
		}
		meta.Usage = u
		meta.HasUsage = true
	}
}

// parseStatusError 构造 StatusError（截断错误体，提取 error.code/type）。
func parseStatusError(resp *http.Response) *StatusError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	se := &StatusError{Status: resp.StatusCode, Body: string(body)}
	var parsed struct {
		Error struct {
			Code any    `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		switch v := parsed.Error.Code.(type) {
		case string:
			se.Code = v
		case float64:
			se.Code = strconv.Itoa(int(v))
		}
		if se.Code == "" {
			se.Code = parsed.Error.Type
		}
	}
	return se
}
