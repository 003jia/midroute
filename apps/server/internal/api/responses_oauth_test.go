package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"midroute/internal/connectors/oauth"
	"midroute/internal/credentials"
	"midroute/internal/domain"
	"midroute/internal/httpserver"
	"midroute/internal/repository"
	"midroute/internal/router"
	"midroute/internal/session"
	"midroute/internal/testutil"
)

// persistentVault 模拟 Keychain 语义（OAuth 需要持久库）。
type persistentVault struct {
	*credentials.InMemoryVault
}

func (persistentVault) Persistent() bool { return true }

// fakeTokenServer 假 Codex 令牌端点：按 PKCE 校验 verifier，支持交换与刷新。
type fakeTokenServer struct {
	*httptest.Server
	mu        sync.Mutex
	challenge string
}

func newFakeTokenServer(t *testing.T) *fakeTokenServer {
	t.Helper()
	ts := &fakeTokenServer{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code") != "auth-code-9" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// PKCE：BASE64URL(SHA256(verifier)) 必须匹配授权时登记的 challenge
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != ts.current() {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			writeOAuthToken(w)
		case "refresh_token":
			if r.Form.Get("refresh_token") == "stale" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			writeOAuthToken(w)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (ts *fakeTokenServer) current() string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.challenge
}

func (ts *fakeTokenServer) setChallenge(v string) {
	ts.mu.Lock()
	ts.challenge = v
	ts.mu.Unlock()
}

func writeOAuthToken(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  "at-" + fmt.Sprint(time.Now().UnixNano()),
		"refresh_token": "rt-" + fmt.Sprint(time.Now().UnixNano()),
		"id_token":      "idt",
		"expires_in":    3600,
	})
}

// buildOAuthServer 组装带 OAuth 注册的完整服务。
func buildOAuthServer(t *testing.T, tokenSrv *fakeTokenServer) (*httptest.Server, *App, *persistentVault) {
	t.Helper()
	_, store := testutil.NewStore(t)
	vault := persistentVault{InMemoryVault: credentials.NewInMemoryVault()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := NewApp(store, vault, nil, log)
	app.Router = router.New(store, app.ResolveForRouter, log)
	prov := oauth.Codex
	prov.TokenURL = tokenSrv.URL
	app.RegisterOAuth("codex", oauth.NewService(vault, prov, tokenSrv.Client()))
	guard := session.NewGuard(true, "")
	srv := httpserver.New(log, httpserver.NewDBStore(1), guard)
	app.Mount(srv)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, app, &vault
}

// TestResponsesRelayAndStickyBinding /v1/responses：转发、句柄绑定与粘性续接。
func TestResponsesRelayAndStickyBinding(t *testing.T) {
	var callCount int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.created\n")
		io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_bind_1\"}}\n\n")
		io.WriteString(w, "event: response.completed\n")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bind_1\",\"model\":\"gpt-5\",\"usage\":{\"input_tokens\":7,\"output_tokens\":2}}}\n\n")
	}))
	defer upstream.Close()

	ts, app := buildServer(t)
	base := ts.URL

	provR := mustPost(t, base+"/api/v1/providers", `{"kind":"codex","base_url":"`+upstream.URL+`"}`)
	var p struct {
		ID string `json:"id"`
	}
	decodeBody(t, provR, &p)
	accR := mustPost(t, base+"/api/v1/accounts",
		`{"provider_id":"`+p.ID+`","name":"codex 账户","api_key":"sk-test-codex-0000000000000000"}`)
	var a struct {
		ID string `json:"id"`
	}
	decodeBody(t, accR, &a)
	if err := app.Store.UpsertModels(context.Background(), []domain.Model{{
		ID: p.ID + "|gpt-5", ProviderID: p.ID, UpstreamID: "gpt-5",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := app.Store.SaveAccountModels(context.Background(), a.ID, []repository.ModelInfoRow{{ModelID: p.ID + "|gpt-5", Listed: true, Usable: true}}); err != nil {
		t.Fatal(err)
	}
	mustPut(t, base+"/api/v1/routing-policies/gpt-5-codex",
		`{"name":"codex","candidates":[{"account_id":"`+a.ID+`","model_id":"gpt-5","priority":1}],"enabled":true}`)

	tokResp := mustPost(t, base+"/api/v1/tokens", `{"name":"t","model_whitelist":["gpt-5-codex"]}`)
	var tok struct {
		Token string `json:"token"`
	}
	decodeBody(t, tokResp, &tok)

	// 首次请求：建立会话
	resp := postJSONWithToken(t, base+"/v1/responses", `{"model":"gpt-5-codex","stream":true,"input":"hi"}`, tok.Token)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("first responses: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "resp_bind_1") {
		t.Fatalf("stream missing id: %s", body)
	}
	// 用量已落库（7 input / 2 output）
	evs, err := app.Store.ListUsageEvents(context.Background(), 10)
	if err != nil || len(evs) == 0 || evs[0].InputTokens != 7 || evs[0].OutputTokens != 2 {
		t.Fatalf("usage events: %+v err=%v", evs, err)
	}

	// 粘性续接：previous_response_id 回到原账户
	resp2 := postJSONWithToken(t, base+"/v1/responses",
		`{"model":"gpt-5-codex","stream":true,"previous_response_id":"resp_bind_1","input":"continue"}`, tok.Token)
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("sticky follow-up: %d", resp2.StatusCode)
	}
	if callCount != 2 {
		t.Fatalf("upstream calls = %d, want 2", callCount)
	}

	// 未知句柄 → 404 且可解释（不换账户续接）
	resp3 := postJSONWithToken(t, base+"/v1/responses",
		`{"model":"gpt-5-codex","previous_response_id":"resp_missing","input":"x"}`, tok.Token)
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeBody(t, resp3, &e)
	if resp3.StatusCode != 404 || !strings.Contains(e.Error.Message, "过期") {
		t.Fatalf("missing handle: %d %s", resp3.StatusCode, e.Error.Message)
	}
}

// TestOAuthAPIFlow OAuth 管理 API：start → complete → 账户创建 → revoke。
func TestOAuthAPIFlow(t *testing.T) {
	tokenSrv := newFakeTokenServer(t)
	ts, app, vault := buildOAuthServer(t, tokenSrv)
	base := ts.URL

	// 1. start
	resp := mustPost(t, base+"/api/v1/oauth/codex/start", `{"name":"我的 Codex"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	var st struct {
		AuthURL string `json:"auth_url"`
	}
	decodeBody(t, resp, &st)
	if !strings.Contains(st.AuthURL, "code_challenge=") || !strings.Contains(st.AuthURL, "state=") {
		t.Fatalf("auth url: %q", st.AuthURL)
	}
	state := extractStateParam(st.AuthURL)
	if i := strings.Index(st.AuthURL, "code_challenge="); i >= 0 {
		ch := st.AuthURL[i+len("code_challenge="):]
		if amp := strings.IndexAny(ch, "&#"); amp >= 0 {
			ch = ch[:amp]
		}
		tokenSrv.setChallenge(ch)
	}

	// 2. complete（手动粘贴回调）
	callback := "http://localhost:1455/auth/callback?code=auth-code-9&state=" + state
	resp = mustPost(t, base+"/api/v1/oauth/codex/complete", `{"callback_url":"`+callback+`"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("complete: %d", resp.StatusCode)
	}
	var acc struct {
		ID string `json:"id"`
	}
	decodeBody(t, resp, &acc)
	if acc.ID == "" {
		t.Fatal("no account created")
	}
	// 账户为 OAuth 类型且 auth_state=ok
	got, err := app.Store.GetAccount(context.Background(), acc.ID)
	if err != nil || got.AuthType != "oauth" || got.AuthState != "ok" {
		t.Fatalf("account: %+v err=%v", got, err)
	}

	// 3. flows 状态
	resp = mustGet(t, base+"/api/v1/oauth/codex/flows/"+state)
	var fl struct {
		Status string `json:"status"`
	}
	decodeBody(t, resp, &fl)
	if fl.Status != "completed" {
		t.Fatalf("flow status: %+v", fl)
	}

	// 4. revoke：本地断开 + reauth_required
	resp = mustPost(t, base+"/api/v1/accounts/"+acc.ID+"/revoke", "")
	if resp.StatusCode != 200 {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	revoked, _ := app.Store.GetAccount(context.Background(), acc.ID)
	if revoked.AuthState != "reauth_required" {
		t.Fatalf("auth_state after revoke: %q", revoked.AuthState)
	}
	// Vault 条目已删除
	if _, err := vault.Get(credentials.SecretRef{Service: revoked.SecretService, Account: revoked.SecretAccount}); err == nil {
		t.Fatal("credential should be deleted from vault")
	}
}

// TestTokenManagementAPI 令牌管理面：项目创建/引用冲突/白名单字段回读。
func TestTokenManagementAPI(t *testing.T) {
	ts, _ := buildServer(t)
	base := ts.URL

	proj := mustPost(t, base+"/api/v1/projects", `{"name":"个人项目"}`)
	if proj.StatusCode != 201 {
		t.Fatalf("project: %d", proj.StatusCode)
	}
	var p struct {
		ID string `json:"id"`
	}
	decodeBody(t, proj, &p)

	tok := mustPost(t, base+"/api/v1/tokens",
		`{"name":"受限令牌","project_id":"`+p.ID+`","model_whitelist":["gpt-x"],"max_concurrency":2}`)
	var c struct {
		ID             string `json:"id"`
		MaxConcurrency int    `json:"max_concurrency"`
	}
	decodeBody(t, tok, &c)
	if c.ID == "" || c.MaxConcurrency != 2 {
		t.Fatalf("created: %+v", c)
	}

	list, _ := http.Get(base + "/api/v1/tokens")
	var l struct {
		Data []struct {
			ID             string `json:"id"`
			ProjectID      string `json:"project_id"`
			ModelWhitelist string `json:"model_whitelist"`
		} `json:"data"`
	}
	decodeBody(t, list, &l)
	if len(l.Data) != 1 || l.Data[0].ProjectID != p.ID || l.Data[0].ModelWhitelist != `["gpt-x"]` {
		t.Fatalf("list: %+v", l.Data)
	}

	// 项目被令牌引用 → 删除冲突
	dr := mustReq(t, http.MethodDelete, base+"/api/v1/projects/"+p.ID, "")
	resp, _ := http.DefaultClient.Do(dr)
	if resp.StatusCode != 409 {
		t.Fatalf("delete referenced project: want 409 got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 删除令牌后项目可删
	dd := mustReq(t, http.MethodDelete, base+"/api/v1/tokens/"+c.ID, "")
	resp, _ = http.DefaultClient.Do(dd)
	resp.Body.Close()
	dr2 := mustReq(t, http.MethodDelete, base+"/api/v1/projects/"+p.ID, "")
	resp, _ = http.DefaultClient.Do(dr2)
	if resp.StatusCode != 200 {
		t.Fatalf("delete project: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// ---- 辅助 ----

func postJSONWithToken(t *testing.T, url, body, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func mustGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func mustPut(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
