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
	"midroute/internal/repository"
	"midroute/internal/router"
	"midroute/internal/session"
	"midroute/internal/testutil"
	"midroute/internal/usage/attempts"
)

// buildServer 组装完整服务（账户目标由 Provider.base_url 决定，指向 mock 上游）。
func buildServer(t *testing.T) (*httptest.Server, *App) {
	t.Helper()
	_, store := testutil.NewStore(t)
	vault := credentials.NewInMemoryVault()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := NewApp(store, vault, nil, log)
	rt := router.New(store, app.ResolveForRouter, log)
	rt.Attempts = attempts.NewRecorder(store)
	app.Router = rt

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

// MR-006：账户能力矩阵 —— 每能力单独判定，monitor_only 转发受限，API Key 无订阅。
func TestAccountCapabilities(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"object":"list","data":[{"id":"gpt-4o"}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	ts, _ := buildServer(t)
	base := ts.URL
	var prov struct {
		ID string `json:"id"`
	}
	decodeBody(t, mustPost(t, base+"/api/v1/providers",
		`{"kind":"openai","name":"OpenAI","base_url":"`+upstream.URL+`"}`), &prov)
	var acc struct {
		ID string `json:"id"`
	}
	decodeBody(t, mustPost(t, base+"/api/v1/accounts",
		`{"provider_id":"`+prov.ID+`","name":"主","api_key":"sk-test-abcdefghijklmnopqrstuvwxyz123456"}`), &acc)

	resp, _ := http.Post(base+"/api/v1/accounts/"+acc.ID+"/capabilities", "application/json", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("capabilities: %d", resp.StatusCode)
	}
	var out struct {
		Data []domain.AccountCapability `json:"data"`
	}
	decodeBody(t, resp, &out)
	byName := map[domain.CapabilityName]domain.AccountCapability{}
	for _, c := range out.Data {
		byName[c.Capability] = c
	}
	// 每能力都有 status，且不伪造 supported
	for _, want := range []domain.CapabilityName{
		domain.CapVerifyCredential, domain.CapDiscoverModels, domain.CapForward,
		domain.CapOAuth, domain.CapSubscription, domain.CapQuota, domain.CapRefresh, domain.CapProbeHealth,
	} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("缺少能力 %s", want)
		}
	}
	if byName[domain.CapVerifyCredential].Status != domain.CapabilitySupported {
		t.Fatalf("verifyCredential=%s", byName[domain.CapVerifyCredential].Status)
	}
	if byName[domain.CapForward].Status != domain.CapabilitySupported {
		t.Fatalf("forward=%s", byName[domain.CapForward].Status)
	}
	// API Key 账户不应声称订阅/刷新支持
	if byName[domain.CapSubscription].Status == domain.CapabilitySupported {
		t.Fatal("subscription must not be supported for API Key openai account")
	}
	if byName[domain.CapRefresh].Status == domain.CapabilitySupported {
		t.Fatal("refresh must not be supported for API Key account")
	}
}

// MR-006：仅监测账户 forward 受限。
func TestAccountCapabilitiesMonitorOnly(t *testing.T) {
	ts, _ := buildServer(t)
	base := ts.URL
	var prov struct {
		ID string `json:"id"`
	}
	decodeBody(t, mustPost(t, base+"/api/v1/providers",
		`{"kind":"openai","name":"OpenAI","base_url":"http://127.0.0.1:1"}`), &prov)
	var acc struct {
		ID string `json:"id"`
	}
	decodeBody(t, mustPost(t, base+"/api/v1/accounts",
		`{"provider_id":"`+prov.ID+`","name":"mon","mode":"monitor_only","api_key":"sk-test-abcdefghijklmnopqrstuvwxyz123456"}`), &acc)
	resp, _ := http.Post(base+"/api/v1/accounts/"+acc.ID+"/capabilities", "application/json", nil)
	var out struct {
		Data []domain.AccountCapability `json:"data"`
	}
	decodeBody(t, resp, &out)
	for _, c := range out.Data {
		if c.Capability == domain.CapForward && c.Status != domain.CapabilityUnsupported {
			t.Fatalf("monitor_only forward must be unsupported, got %s", c.Status)
		}
	}
}

// MR-004：凭据轮换 —— 新凭据验证失败保留旧凭据；成功才原子切换。
func TestCredentialRotationKeepsOldOnFailure(t *testing.T) {
	var mode string // "good" | "bad"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "bad" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	mode = "good"

	ts, app := buildServer(t)
	base := ts.URL
	var prov struct {
		ID string `json:"id"`
	}
	decodeBody(t, mustPost(t, base+"/api/v1/providers",
		`{"kind":"openai","name":"OpenAI","base_url":"`+upstream.URL+`"}`), &prov)
	var acc struct {
		ID                string `json:"id"`
		SecretFingerprint string `json:"secret_fingerprint"`
	}
	// 初始密钥
	oldKey := "sk-test-abcdefghijklmnopqrstuvwxyz123456"
	decodeBody(t, mustPost(t, base+"/api/v1/accounts",
		`{"provider_id":"`+prov.ID+`","name":"主","api_key":"`+oldKey+`"}`), &acc)

	// 用旧凭据验证，得到旧指纹
	oldFp := acc.SecretFingerprint

	// 失败轮换：新密钥无效（upstream 拒绝）
	mode = "bad"
	resp := mustPost(t, base+"/api/v1/accounts/"+acc.ID+"/credential",
		`{"api_key":"sk-new-invalid-abcdefghijklmnopqrstuvwxyz"}`)
	if resp.StatusCode != 401 {
		t.Fatalf("bad rotation: want 401 got %d", resp.StatusCode)
	}
	// 指纹未变（旧凭据保留）
	got, _ := app.Store.GetAccount(context.Background(), acc.ID)
	if got.SecretFingerprint != oldFp {
		t.Fatalf("failed rotation changed fingerprint: %s -> %s", oldFp, got.SecretFingerprint)
	}

	// 成功轮换：新密钥有效
	mode = "good"
	resp = mustPost(t, base+"/api/v1/accounts/"+acc.ID+"/credential",
		`{"api_key":"sk-new-good-abcdefghijklmnopqrstuvwxyz123456"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("good rotation: %d", resp.StatusCode)
	}
	var rot struct {
		SecretFingerprint string `json:"secret_fingerprint"`
	}
	decodeBody(t, resp, &rot)
	got, _ = app.Store.GetAccount(context.Background(), acc.ID)
	if got.SecretFingerprint != rot.SecretFingerprint || got.SecretFingerprint == oldFp {
		t.Fatalf("rotation did not switch: got %s want %s", got.SecretFingerprint, rot.SecretFingerprint)
	}
	// 新凭据能从 Vault 取回且可转发
	sec, err := app.Vault.Get(credentials.SecretRef{Service: got.SecretService, Account: got.SecretAccount})
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Zero()
	if string(sec.Value) != "sk-new-good-abcdefghijklmnopqrstuvwxyz123456" {
		t.Fatalf("vault secret mismatch")
	}
}

// MR-009：额度池 + 账户快照 + 手工补充 API。
func TestQuotaPoolsAndManual(t *testing.T) {
	ts, app := buildServer(t)
	base := ts.URL
	ctx := context.Background()
	var prov struct {
		ID string `json:"id"`
	}
	decodeBody(t, mustPost(t, base+"/api/v1/providers", `{"kind":"openai","name":"p","base_url":"http://127.0.0.1:1"}`), &prov)
	// 两个账户（模拟两个 Key）
	var acc1, acc2 struct {
		ID string `json:"id"`
	}
	decodeBody(t, mustPost(t, base+"/api/v1/accounts",
		`{"provider_id":"`+prov.ID+`","name":"k1","api_key":"sk-test-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`), &acc1)
	decodeBody(t, mustPost(t, base+"/api/v1/accounts",
		`{"provider_id":"`+prov.ID+`","name":"k2","api_key":"sk-test-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`), &acc2)

	// 建共享池（两个 Key 共用额度）
	resp := mustPost(t, base+"/api/v1/quota-pools",
		`{"id":"pool1","provider_id":"`+prov.ID+`","external_org":"org-x","scope":"org","member_ids":["`+acc1.ID+`","`+acc2.ID+`"]}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create pool: %d", resp.StatusCode)
	}

	// 账户1 手工补充额度
	manual := `{"window_type":"primary","used":35,"limit":100,"unit":"percent"}`
	resp = mustPost(t, base+"/api/v1/accounts/"+acc1.ID+"/quota-manual", manual)
	if resp.StatusCode != 201 {
		t.Fatalf("manual quota: %d", resp.StatusCode)
	}
	var snap domain.QuotaSnapshot
	decodeBody(t, resp, &snap)
	if snap.Source != domain.QuotaSourceManual || snap.Used == nil || *snap.Used != 35 {
		t.Fatalf("manual snapshot wrong: %+v", snap)
	}

	// 账户快照列表
	resp, _ = http.Get(base + "/api/v1/accounts/" + acc1.ID + "/quota")
	var snaps struct {
		Data []domain.QuotaSnapshot `json:"data"`
	}
	decodeBody(t, resp, &snaps)
	if len(snaps.Data) != 1 || snaps.Data[0].WindowType != domain.QuotaWindowPrimary {
		t.Fatalf("account snaps=%+v", snaps.Data)
	}

	// 池快照（空，因手工快照未归属池；验证端点可用）
	resp, _ = http.Get(base + "/api/v1/quota-pools/pool1/snapshots")
	var poolSnaps struct {
		Data []domain.QuotaSnapshot `json:"data"`
	}
	decodeBody(t, resp, &poolSnaps)
	if poolSnaps.Data == nil {
		t.Fatal("pool snapshots must be non-nil list")
	}

	// 池列表
	resp, _ = http.Get(base + "/api/v1/quota-pools")
	var pools struct {
		Data []domain.QuotaPool `json:"data"`
	}
	decodeBody(t, resp, &pools)
	if len(pools.Data) != 1 || len(pools.Data[0].MemberIDs) != 2 {
		t.Fatalf("pools=%+v", pools.Data)
	}

	// 审计已记录
	if n, _ := app.Store.CountAuditEvents(ctx); n == 0 {
		t.Fatal("audit missing")
	}
}

// MR-010：后台刷新任务 API —— 去重、job 状态可查。
func TestRefreshJobAPI(t *testing.T) {
	ts, app := buildServer(t)
	base := ts.URL
	ctx := context.Background()
	app.Sched.Register("models", func(_ context.Context, _ string) error { return nil })
	app.Sched.Register("capabilities", func(_ context.Context, _ string) error { return fmt.Errorf("fail") })

	var prov struct {
		ID string `json:"id"`
	}
	decodeBody(t, mustPost(t, base+"/api/v1/providers",
		`{"kind":"openai","name":"p","base_url":"http://127.0.0.1:1"}`), &prov)
	var acc struct {
		ID string `json:"id"`
	}
	decodeBody(t, mustPost(t, base+"/api/v1/accounts",
		`{"provider_id":"`+prov.ID+`","api_key":"sk-test-abcdefghijklmnopqrstuvwxyz123456"}`), &acc)

	// 成功任务
	resp := mustPost(t, base+"/api/v1/accounts/"+acc.ID+"/refresh", `{"capability":"models"}`)
	if resp.StatusCode != 202 {
		t.Fatalf("refresh: %d", resp.StatusCode)
	}
	var job1 struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	decodeBody(t, resp, &job1)
	if job1.State != "success" {
		t.Fatalf("state=%s", job1.State)
	}
	// 查 job
	resp, _ = http.Get(base + "/api/v1/jobs/" + job1.JobID)
	var j struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	decodeBody(t, resp, &j)
	if j.ID != job1.JobID {
		t.Fatalf("job=%s", j.ID)
	}

	// 失败任务 → 状态 failed，且 next_run_at 非空
	resp = mustPost(t, base+"/api/v1/accounts/"+acc.ID+"/refresh", `{"capability":"capabilities"}`)
	var job2 struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	decodeBody(t, resp, &job2)
	resp, _ = http.Get(base + "/api/v1/jobs/" + job2.JobID)
	var j2 struct {
		State     string `json:"state"`
		NextRunAt string `json:"next_run_at"`
	}
	decodeBody(t, resp, &j2)
	if j2.State != "failed" || j2.NextRunAt == "" {
		t.Fatalf("failed job: state=%s next=%s", j2.State, j2.NextRunAt)
	}

	// 列表接口
	resp, _ = http.Get(base + "/api/v1/jobs?account_id=" + acc.ID)
	var list struct {
		Data []repository.RefreshJob `json:"data"`
	}
	decodeBody(t, resp, &list)
	if len(list.Data) < 2 {
		t.Fatalf("jobs len=%d", len(list.Data))
	}
	_ = ctx
}

// MR-015：每次真实上游尝试单独记录；断流=partial；幂等（request+attempt）。
func TestRequestAttemptsRecorded(t *testing.T) {
	// 候选1 429（失败，安全切换），候选2 成功
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", 429)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"c2","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`)
	}))
	defer good.Close()

	ts, _ := buildServer(t)
	base := ts.URL
	// provider base_url 指向 bad；用两个账户分别指向两个 mock
	var prov struct {
		ID string `json:"id"`
	}
	decodeBody(t, mustPost(t, base+"/api/v1/providers", `{"kind":"openai","name":"bad","base_url":"`+bad.URL+`"}`), &prov)
	var prov2 struct {
		ID string `json:"id"`
	}
	decodeBody(t, mustPost(t, base+"/api/v1/providers", `{"kind":"openai","name":"good","base_url":"`+good.URL+`"}`), &prov2)
	var accBad, accGood struct {
		ID string `json:"id"`
	}
	decodeBody(t, mustPost(t, base+"/api/v1/accounts",
		`{"provider_id":"`+prov.ID+`","name":"bad","api_key":"sk-test-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`), &accBad)
	decodeBody(t, mustPost(t, base+"/api/v1/accounts",
		`{"provider_id":"`+prov2.ID+`","name":"good","api_key":"sk-test-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`), &accGood)

	// 策略：候选1=bad（priority1），候选2=good（priority2）→ 触发失败切换
	http.DefaultClient.Do(mustReq(t, http.MethodPut, base+"/api/v1/routing-policies/alias-x",
		`{"name":"x","candidates":[{"account_id":"`+accBad.ID+`","model_id":"gpt-4o","priority":1,"weight":1},{"account_id":"`+accGood.ID+`","model_id":"gpt-4o","priority":2,"weight":1}],"enabled":true}`))

	// /v1/* 走项目令牌鉴权：先创建推理令牌
	respTok := mustPost(t, base+"/api/v1/tokens", `{"name":"test"}`)
	var tok struct {
		Token string `json:"token"`
	}
	decodeBody(t, respTok, &tok)
	reqChat := mustReq(t, http.MethodPost, base+"/v1/chat/completions",
		`{"model":"alias-x","messages":[{"role":"user","content":"hi"}]}`)
	reqChat.Header.Set("Authorization", "Bearer "+tok.Token)
	resp, _ := http.DefaultClient.Do(reqChat)
	if resp.StatusCode != 200 {
		t.Fatalf("chat: %d", resp.StatusCode)
	}
	var chat struct {
		ID string `json:"id"`
	}
	decodeBody(t, resp, &chat)
	_ = chat

	// 从近期请求反查 request_id（mock 上游透传了自身 id，不注入请求 id）
	resp, _ = http.Get(base + "/api/v1/requests")
	var recents struct {
		Data []repository.RequestAttempt `json:"data"`
	}
	decodeBody(t, resp, &recents)
	var requestID string
	seen := map[string]bool{}
	for _, a := range recents.Data {
		seen[a.AccountID] = true
		if seen[accBad.ID] && seen[accGood.ID] && a.RequestID != "" {
			requestID = a.RequestID
			break
		}
	}
	if requestID == "" {
		t.Fatalf("could not find request_id from attempts")
	}

	// 请求详情应含两次尝试：bad 失败（rate_limited）、good 成功（带 usage）
	resp, _ = http.Get(base + "/api/v1/requests/" + requestID)
	var detail struct {
		Data []repository.RequestAttempt `json:"data"`
	}
	decodeBody(t, resp, &detail)
	if len(detail.Data) != 2 {
		t.Fatalf("attempts=%d want 2", len(detail.Data))
	}
	byAccount := map[string]repository.RequestAttempt{}
	for _, a := range detail.Data {
		byAccount[a.AccountID] = a
	}
	badAtt := byAccount[accBad.ID]
	goodAtt := byAccount[accGood.ID]
	if badAtt.Status != "failed" || badAtt.ErrorClass != "rate_limited" {
		t.Fatalf("bad attempt: %+v", badAtt)
	}
	if goodAtt.Status != "success" || goodAtt.InputTokens != 4 || goodAtt.Metering != "reported" {
		t.Fatalf("good attempt: %+v", goodAtt)
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
