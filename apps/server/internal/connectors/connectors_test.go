package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serve 启动 mock 上游，返回 server 与请求记录。
func serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// capture 收集收到的请求体。
func capture(t *testing.T, out *[]byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*out = append(*out, b...)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, "{}")
	}
}

func chatReq(model string) *ChatRequest {
	return &ChatRequest{
		Model: model,
		Messages: []ChatMessage{
			{Role: "system", Content: "be concise"},
			{Role: "user", Content: "hello"},
		},
	}
}

func TestOpenAICompatibleForward(t *testing.T) {
	var authHeader string
	var gotBody []byte
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path=%s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = append(gotBody, b...)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	})
	c := NewOpenAICompatibleConnector(srv.Client(), KindOpenAI)
	target := Target{ProviderKind: KindOpenAI, BaseURL: srv.URL, APIKey: "sk-test-abcdefghijklmnopqrstuvwxyz123456"}
	resp, err := c.Forward(context.Background(), target, chatReq("gpt-4o"))
	if err != nil {
		t.Fatal(err)
	}
	if authHeader != "Bearer sk-test-abcdefghijklmnopqrstuvwxyz123456" {
		t.Fatalf("auth=%q", authHeader)
	}
	if resp.Choices[0].Message.Content != "hi" || resp.Model != "gpt-4o" {
		t.Fatalf("resp=%+v", resp)
	}
	_ = gotBody
}

func TestOpenAICompatibleDiscover(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path=%s", r.URL.Path)
		}
		io.WriteString(w, `{"object":"list","data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"}]}`)
	})
	c := NewOpenAICompatibleConnector(srv.Client(), KindOpenAI)
	infos, err := c.DiscoverModels(context.Background(), Target{BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].UpstreamID != "gpt-4o" {
		t.Fatalf("infos=%+v", infos)
	}
}

func TestOpenAICompatibleStreamPassthrough(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	c := NewOpenAICompatibleConnector(srv.Client(), KindOpenAI)
	var chunks [][]byte
	_, err := c.ForwardStream(context.Background(), Target{BaseURL: srv.URL, APIKey: "k"}, chatReq("gpt-4o"), func(b []byte) error {
		chunks = append(chunks, append([]byte(nil), b...))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || !strings.Contains(string(chunks[0]), "\"content\":\"a\"") || !strings.Contains(string(chunks[1]), "[DONE]") {
		t.Fatalf("chunks=%q", chunks)
	}
}

// 截断的流（无 [DONE] 结束标记）必须报错，不得静默当成功。
func TestOpenAICompatibleStreamTruncated(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"半\"}}]}\n\n")
		// 连接中断，无 [DONE]
	})
	c := NewOpenAICompatibleConnector(srv.Client(), KindOpenAI)
	_, err := c.ForwardStream(context.Background(), Target{BaseURL: srv.URL, APIKey: "k"}, chatReq("gpt-4o"), func(b []byte) error {
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("want truncated error, got %v", err)
	}
}

func TestAnthropicForwardTranslation(t *testing.T) {
	var got map[string]any
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" {
			t.Error("missing x-api-key")
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","model":"claude-x","content":[{"type":"text","text":"回答"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)
	})
	c := NewAnthropicConnector(srv.Client())
	resp, err := c.Forward(context.Background(), Target{BaseURL: srv.URL, APIKey: "sk-ant-admin-test"}, chatReq("claude-sonnet-4"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].Message.Content != "回答" {
		t.Fatalf("content=%q", resp.Choices[0].Message.Content)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 5 {
		t.Fatalf("usage=%+v", resp.Usage)
	}
	// 验证翻译：system 归入 system 字段
	if got["system"] != "be concise" {
		t.Fatalf("system=%v", got["system"])
	}
}

func TestAnthropicStreamTranslation(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"你\"}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"好\"}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	c := NewAnthropicConnector(srv.Client())
	var chunks []string
	usage, err := c.ForwardStream(context.Background(), Target{BaseURL: srv.URL, APIKey: "k"}, chatReq("claude-sonnet-4"), func(b []byte) error {
		chunks = append(chunks, string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(chunks, "")
	if !strings.Contains(joined, "\"content\":\"你\"") || !strings.Contains(joined, "\"content\":\"好\"") {
		t.Fatalf("missing text deltas: %s", joined)
	}
	if !strings.Contains(joined, "\"finish_reason\":\"stop\"") {
		t.Fatalf("missing finish: %s", joined)
	}
	if usage.InputTokens != 3 || usage.OutputTokens != 2 {
		t.Fatalf("usage=%+v", usage)
	}
}

func TestGeminiForwardTranslation(t *testing.T) {
	var got map[string]any
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, ":generateContent") {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.Header.Get("x-goog-api-key") == "" {
			t.Error("missing x-goog-api-key")
		}
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"gemini回答"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":4,"cachedContentTokenCount":2}}`)
	})
	c := NewGeminiConnector(srv.Client())
	resp, err := c.Forward(context.Background(), Target{BaseURL: srv.URL, APIKey: "AIza-test"}, chatReq("gemini-2.5-flash"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].Message.Content != "gemini回答" {
		t.Fatalf("content=%q", resp.Choices[0].Message.Content)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 8 {
		t.Fatalf("usage=%+v", resp.Usage)
	}
	// system → systemInstruction
	si, ok := got["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("missing systemInstruction: %v", got)
	}
	_ = si
}

func TestGeminiStreamTranslation(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"片\"}]}}]}\n\n")
		io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"段\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":3}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	c := NewGeminiConnector(srv.Client())
	var chunks []string
	usage, err := c.ForwardStream(context.Background(), Target{BaseURL: srv.URL, APIKey: "k"}, chatReq("gemini-2.5-flash"), func(b []byte) error {
		chunks = append(chunks, string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(chunks, "")
	if !strings.Contains(joined, "\"content\":\"片\"") || !strings.Contains(joined, "\"content\":\"段\"") {
		t.Fatalf("missing deltas: %s", joined)
	}
	if usage.InputTokens != 5 || usage.OutputTokens != 3 {
		t.Fatalf("usage=%+v", usage)
	}
}

// 结构化错误分类（MR-012/ISS-09）：errors.Is 判定，不靠字符串。
func TestStructuredErrorClassification(t *testing.T) {
	if !errors.Is(Classify(429, nil), ErrRateLimited) {
		t.Fatal("429 must be ErrRateLimited")
	}
	if !errors.Is(Classify(401, nil), ErrAuth) {
		t.Fatal("401 must be ErrAuth")
	}
	if !errors.Is(Classify(503, nil), ErrUpstream) {
		t.Fatal("503 must be ErrUpstream")
	}
	if !errors.Is(Classify(404, nil), ErrModelUnavailable) {
		t.Fatal("404 must be ErrModelUnavailable")
	}
	if !errors.Is(Classify(0, context.DeadlineExceeded), ErrTimeout) {
		t.Fatal("deadline must be ErrTimeout")
	}
	if !Retryable(Classify(429, nil)) || !Retryable(Classify(0, context.DeadlineExceeded)) || !Retryable(ErrTruncated) {
		t.Fatal("retryable classification wrong")
	}
	if Retryable(Classify(401, nil)) || Retryable(Classify(404, nil)) {
		t.Fatal("auth/not-found must not be retryable")
	}
	var ue *UpstreamError
	if !errors.As(Classify(500, nil), &ue) {
		t.Fatal("expected *UpstreamError")
	}
}
