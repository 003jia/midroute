package responses

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestForwardNonStreamExtractsMeta(t *testing.T) {
	upstreamBody := `{"id":"resp_1","model":"gpt-5","usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":3},"output_tokens":20,"output_tokens_details":{"reasoning_tokens":4}},"output":[]}`
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"gpt-5"`) {
			t.Errorf("model not passed through: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer srv.Close()

	var passthrough []byte
	meta, err := Forward(context.Background(), srv.Client(), Target{BaseURL: srv.URL, Token: "tok-A"}, []byte(`{"model":"gpt-5","input":"hi"}`), false, func(b []byte) error {
		passthrough = append([]byte(nil), b...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok-A" {
		t.Fatalf("auth header: %q", gotAuth)
	}
	if string(passthrough) != upstreamBody {
		t.Fatalf("body not passed through verbatim")
	}
	if meta.ID != "resp_1" || meta.Model != "gpt-5" {
		t.Fatalf("meta: %+v", meta)
	}
	if !meta.HasUsage || meta.Usage.InputTokens != 10 || meta.Usage.OutputTokens != 20 ||
		meta.Usage.CachedTokens != 3 || meta.Usage.ReasoningTokens != 4 {
		t.Fatalf("usage: %+v", meta.Usage)
	}
}

func TestForwardStreamPassthroughAndUsage(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_2"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_2","model":"gpt-5","usage":{"input_tokens":3,"output_tokens":5}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("accept: %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	var out strings.Builder
	meta, err := Forward(context.Background(), srv.Client(), Target{BaseURL: srv.URL, Token: "t"}, []byte(`{"model":"m","stream":true}`), true, func(b []byte) error {
		out.Write(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != sse {
		t.Fatalf("stream not verbatim:\n%q\nwant\n%q", out.String(), sse)
	}
	if meta.ID != "resp_2" || !meta.HasUsage || meta.Usage.InputTokens != 3 || meta.Usage.OutputTokens != 5 {
		t.Fatalf("meta: %+v %+v", meta, meta.Usage)
	}
}

func TestForwardUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`))
	}))
	defer srv.Close()
	_, err := Forward(context.Background(), srv.Client(), Target{BaseURL: srv.URL}, []byte(`{}`), false, func([]byte) error { return nil })
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("want StatusError, got %v", err)
	}
	if !se.RateLimited() || se.Code != "rate_limit_exceeded" {
		t.Fatalf("status error: %+v", se)
	}
}

// TestForwardClientCancelPropagates 客户端取消传播到上游（上游观察到
// 请求上下文取消），且 Forward 返回错误。
func TestForwardClientCancelPropagates(t *testing.T) {
	release := make(chan struct{})
	var upstreamCancelled int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\"}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
		atomic.AddInt64(&upstreamCancelled, 1)
		close(release)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := Forward(ctx, srv.Client(), Target{BaseURL: srv.URL}, []byte(`{"stream":true}`), true, func([]byte) error { return nil })
		result <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled forward must return error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("forward did not return after cancel")
	}
	select {
	case <-release:
		if atomic.LoadInt64(&upstreamCancelled) != 1 {
			t.Fatal("upstream cancel not observed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not observe cancel")
	}
}
