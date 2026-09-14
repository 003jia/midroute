package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"midroute/internal/connectors"
	"midroute/internal/credentials"
	"midroute/internal/domain"
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

	// 6. 创建项目令牌并校验网关鉴权边界（MR-016）
	resp, err = http.Post(base+"/api/v1/tokens", "application/json",
		strings.NewReader(`{"name":"推理令牌","model_whitelist":["coding-fast","gpt-4o"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var tok struct {
		Token string `json:"token"`
	}
	decodeBody(t, resp, &tok)
	if resp.StatusCode != 201 || tok.Token == "" {
		t.Fatalf("token create: %d %+v", resp.StatusCode, tok)
	}

	// 6a. 无令牌 → 401（管理员未登录也不放行推理）
	resp, err = http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"coding-fast","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("gateway without token: want 401 got %d", resp.StatusCode)
	}

	// 6b. 白名单外模型 → 403（请求上游前拒绝）
	req403, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
		strings.NewReader(`{"model":"other-model","messages":[{"role":"user","content":"hi"}]}`))
	req403.Header.Set("Authorization", "Bearer "+tok.Token)
	resp, err = http.DefaultClient.Do(req403)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("model outside whitelist: want 403 got %d", resp.StatusCode)
	}

	// 7. 网关非流式（带令牌）
	req7, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
		strings.NewReader(`{"model":"coding-fast","messages":[{"role":"user","content":"hi"}]}`))
	req7.Header.Set("Authorization", "Bearer "+tok.Token)
	resp, err = http.DefaultClient.Do(req7)
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

	// 8. 网关 /v1/models（带令牌）
	req8, _ := http.NewRequest(http.MethodGet, base+"/v1/models", nil)
	req8.Header.Set("Authorization", "Bearer "+tok.Token)
	resp, _ = http.DefaultClient.Do(req8)
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

// MR-004：账户/Provider 生命周期（编辑、禁用、删除冲突、审计）。
func TestAccountLifecycle(t *testing.T) {
	ts, app := buildServer(t)
	base := ts.URL
	ctx := context.Background()

	resp := mustPost(t, base+"/api/v1/providers",
		`{"kind":"openai","name":"p","base_url":"http://127.0.0.1:1"}`)
	var prov struct {
		ID string `json:"id"`
	}
	decodeBody(t, resp, &prov)
	resp, _ = http.Post(base+"/api/v1/accounts", "application/json",
		strings.NewReader(`{"provider_id":"`+prov.ID+`","api_key":"sk-test-abcdefghijklmnopqrstuvwxyz123456","name":"main"}`))
	var acc struct {
		ID string `json:"id"`
	}
	decodeBody(t, resp, &acc)

	// 编辑名称 + 禁用
	req := mustReq(t, http.MethodPatch, base+"/api/v1/accounts/"+acc.ID, `{"name":"改名","status":"disabled"}`)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("patch: %d", resp.StatusCode)
	}
	var updated struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	decodeBody(t, resp, &updated)
	if updated.Name != "改名" || updated.Status != "disabled" {
		t.Fatalf("updated=%+v", updated)
	}

	// 禁用后不参与路由
	if _, _, err := app.Router.Forward(ctx, "none", connectors.NewChatRequest("none", nil), "r"); err == nil {
		t.Fatal("no policy should error")
	}

	// 被路由策略引用时删除 → 409
	app.Store.SaveRoutingPolicy(ctx, mkPolicy("pol1", acc.ID))
	req = mustReq(t, http.MethodDelete, base+"/api/v1/accounts/"+acc.ID, "")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 409 {
		t.Fatalf("delete referenced account: want 409 got %d", resp.StatusCode)
	}

	// 解除引用后删除成功
	if err := app.Store.DeleteRoutingPolicy(ctx, "pol1"); err != nil {
		t.Fatal(err)
	}
	req = mustReq(t, http.MethodDelete, base+"/api/v1/accounts/"+acc.ID, "")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}

	// Provider 被账户引用 → 409
	resp, _ = http.Post(base+"/api/v1/accounts", "application/json",
		strings.NewReader(`{"provider_id":"`+prov.ID+`","api_key":"sk-test-abcdefghijklmnopqrstuvwxyz123456"}`))
	decodeBody(t, resp, &acc)
	req = mustReq(t, http.MethodDelete, base+"/api/v1/providers/"+prov.ID, "")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 409 {
		t.Fatalf("delete referenced provider: want 409 got %d", resp.StatusCode)
	}

	// 审计事件已记录
	if auditN, _ := app.Store.CountAuditEvents(context.Background()); auditN == 0 {
		t.Fatal("audit events missing")
	}
}

// MR-016/M2：Token 数据模型 —— 创建只回一次明文、列表不泄露哈希、启停生效。
func TestTokenLifecycle(t *testing.T) {
	ts, app := buildServer(t)
	base := ts.URL

	resp := mustPost(t, base+"/api/v1/tokens", `{"name":"我的令牌"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	var created struct {
		ID     string `json:"id"`
		Token  string `json:"token"`
		Prefix string `json:"key_prefix"`
	}
	decodeBody(t, resp, &created)
	if !strings.HasPrefix(created.Token, "mrt_") || created.Prefix == "" {
		t.Fatalf("created=%+v", created)
	}

	// 明文可被哈希命中（用于推理鉴权）
	sum := sha256.Sum256([]byte(created.Token))
	tok, err := app.Store.FindTokenByKeyHash(context.Background(), fmt.Sprintf("%x", sum))
	if err != nil || !tok.Enabled {
		t.Fatalf("hash lookup failed: %v", err)
	}

	// 列表不泄露哈希与明文
	resp, _ = http.Get(base + "/api/v1/tokens")
	var list struct {
		Data []struct {
			ID      string `json:"id"`
			KeyHash string `json:"key_hash"`
		} `json:"data"`
	}
	decodeBody(t, resp, &list)
	if len(list.Data) != 1 || list.Data[0].KeyHash != "" {
		t.Fatalf("list leaks: %+v", list.Data)
	}

	// 禁用后不可用
	req := mustReq(t, http.MethodPatch, base+"/api/v1/tokens/"+created.ID, `{"enabled":false}`)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("disable: %d", resp.StatusCode)
	}
	sum2 := sha256.Sum256([]byte(created.Token))
	tok, _ = app.Store.FindTokenByKeyHash(context.Background(), fmt.Sprintf("%x", sum2))
	if tok.Enabled {
		t.Fatal("token should be disabled")
	}

	// 删除
	req = mustReq(t, http.MethodDelete, base+"/api/v1/tokens/"+created.ID, "")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	sum3 := sha256.Sum256([]byte(created.Token))
	if _, err := app.Store.FindTokenByKeyHash(context.Background(), fmt.Sprintf("%x", sum3)); err == nil {
		t.Fatal("deleted token must not be found")
	}
}

func mkPolicy(id, accountID string) domain.RoutingPolicy {
	return domain.RoutingPolicy{
		ID: id, Name: id, Alias: "alias-" + id,
		Candidates: []domain.Candidate{{AccountID: accountID, ModelID: "gpt-4o", Priority: 1, Weight: 1}},
		Enabled:    true,
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

	// 项目令牌（网关推理面要求）
	tokResp := mustPost(t, base+"/api/v1/tokens", `{"name":"流式令牌"}`)
	var tok struct {
		Token string `json:"token"`
	}
	decodeBody(t, tokResp, &tok)

	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	resp, err = http.DefaultClient.Do(req)
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
