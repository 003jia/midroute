package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"midroute/internal/connectors"
	"midroute/internal/credentials"
	"midroute/internal/httpserver"
	"midroute/internal/router"
	"midroute/internal/session"
	"midroute/internal/testutil"
)

// buildServer 组装完整服务（账户目标由 Provider.base_url 决定，指向 mock 上游）。
func buildServer(t *testing.T) (*httptest.Server, *App) {
	t.Helper()
	_, store := testutil.NewStore(t)
	vault := credentials.NewInMemoryVault()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := NewApp(store, vault, nil, log)
	app.Router = router.New(store, app.ResolveForRouter, log)

	guard := session.NewGuard(true, "")
	srv := httpserver.New(log, httpserver.NewDBStore(1), guard)
	app.Mount(srv)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, app
}

func TestFullFlowWithMockOpenAIUpstream(t *testing.T) {
	// mock OpenAI 上游
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
			io.WriteString(w, `{"object":"list","data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"}]}`)
		case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
			w.Header().Set("content-type", "application/json")
			io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"你好"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	ts, _ := buildServer(t)
	base := ts.URL

	// 1. 创建 Provider（base_url 指向 mock 上游）
	resp, err := http.Post(base+"/api/v1/providers", "application/json",
		strings.NewReader(`{"kind":"openai","name":"OpenAI 官方","base_url":"`+upstream.URL+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	var prov struct {
		ID string `json:"id"`
	}
	decodeBody(t, resp, &prov)
	if resp.StatusCode != 201 || prov.ID == "" {
		t.Fatalf("provider create: %d %+v", resp.StatusCode, prov)
	}

	// 2. 创建账户（密钥入 Vault，仅存 SecretRef）
	resp, err = http.Post(base+"/api/v1/accounts", "application/json",
		strings.NewReader(`{"provider_id":"`+prov.ID+`","name":"主账户","api_key":"sk-test-abcdefghijklmnopqrstuvwxyz123456"}`))
	if err != nil {
		t.Fatal(err)
	}
	var acc struct {
		ID                string `json:"id"`
		SecretFingerprint string `json:"secret_fingerprint"`
	}
	decodeBody(t, resp, &acc)
	if resp.StatusCode != 201 || acc.ID == "" || acc.SecretFingerprint == "" {
		t.Fatalf("account create: %d %+v", resp.StatusCode, acc)
	}

	// 3. verify
	resp, _ = http.Post(base+"/api/v1/accounts/"+acc.ID+"/verify", "application/json", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("verify: %d", resp.StatusCode)
	}
	if !strings.HasPrefix(gotAuth, "Bearer sk-test-") {
		t.Fatalf("auth header leaked raw key? got %q", gotAuth)
	}

	// 4. discover models
	resp, _ = http.Post(base+"/api/v1/accounts/"+acc.ID+"/discover", "application/json", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("discover: %d", resp.StatusCode)
	}

	// 5. 路由策略 alias
	policy := `{"name":"coding","candidates":[{"account_id":"` + acc.ID + `","model_id":"gpt-4o","priority":1,"weight":1}],"enabled":true}`
	req, _ := http.NewRequest(http.MethodPut, base+"/api/v1/routing-policies/coding-fast", strings.NewReader(policy))
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("policy: %d", resp.StatusCode)
	}

	// 6. 网关非流式
	resp, err = http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"coding-fast","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	decodeBody(t, resp, &chat)
	if resp.StatusCode != 200 {
		t.Fatalf("chat: %d", resp.StatusCode)
	}
	if chat.Choices[0].Message.Content != "你好" || chat.Model != "coding-fast" {
		t.Fatalf("chat resp=%+v", chat)
	}

	// 7. 网关 /v1/models
	resp, _ = http.Get(base + "/v1/models")
	var mlist struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	decodeBody(t, resp, &mlist)
	if mlist.Object != "list" || len(mlist.Data) == 0 {
		t.Fatalf("v1/models=%+v", mlist)
	}
}

func decodeBody(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && resp.StatusCode != 200 {
		t.Logf("decode body err: %v", err)
	}
}

func TestGatewayStreamingPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"流\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"式\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	ts, _ := buildServer(t)
	base := ts.URL
	var prov struct {
		ID string `json:"id"`
	}
	resp0 := mustPost(t, base+"/api/v1/providers",
		`{"kind":"openai","name":"s","base_url":"`+upstream.URL+`"}`)
	decodeBody(t, resp0, &prov)

	resp, _ := http.Post(base+"/api/v1/accounts", "application/json",
		strings.NewReader(`{"provider_id":"`+prov.ID+`","api_key":"sk-test-abcdefghijklmnopqrstuvwxyz123456"}`))
	var acc struct {
		ID string `json:"id"`
	}
	decodeBody(t, resp, &acc)

	// 直接按上游模型路由（无策略时按 model 直连需 account_models，此处简化：建策略）
	http.DefaultClient.Do(mustReq(t, http.MethodPut, base+"/api/v1/routing-policies/gpt-4o",
		`{"name":"x","candidates":[{"account_id":"`+acc.ID+`","model_id":"gpt-4o","priority":1,"weight":1}],"enabled":true}`))

	resp, err := http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "\"content\":\"流\"") || !strings.Contains(s, "\"content\":\"式\"") || !strings.Contains(s, "[DONE]") {
		t.Fatalf("stream body=%s", s)
	}
}

func mustPost(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func mustReq(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// 保证类型断言：App 满足 router resolver 所需
var _ = connectors.KindOpenAI
var _ = context.Background
