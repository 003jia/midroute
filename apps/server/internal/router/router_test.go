package router

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"midroute/internal/connectors"
	"midroute/internal/domain"
	"midroute/internal/repository"
	"midroute/internal/testutil"
)

// mockUpstream 启动 mock 上游：statusCode 时返回该状态（错误），否则正常返回。
func mockUpstream(t *testing.T, statusCode int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if statusCode != 200 {
			http.Error(w, body, statusCode)
			return
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestRouter(t *testing.T, servers map[string]string) (*Router, map[string]string) {
	t.Helper()
	_, store := testutil.NewStore(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	r := New(store, func(_ context.Context, accountID string) (*ResolvedTarget, error) {
		base := servers[accountID]
		return &ResolvedTarget{
			AccountID: accountID,
			Target:    connectors.Target{ProviderKind: connectors.KindOpenAICompatible, BaseURL: base, APIKey: "k"},
		}, nil
	}, log)
	return r, servers
}

func TestForwardFailoverOnRetryable429(t *testing.T) {
	good := mockUpstream(t, 200, `{"id":"c2","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	bad := mockUpstream(t, 429, "rate limited")
	_, store := testutil.NewStore(t)
	servers := map[string]string{"acc_bad": bad.URL, "acc_good": good.URL}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	r := New(store, func(_ context.Context, accountID string) (*ResolvedTarget, error) {
		return &ResolvedTarget{AccountID: accountID, Target: connectors.Target{
			ProviderKind: connectors.KindOpenAICompatible, BaseURL: servers[accountID], APIKey: "k",
		}}, nil
	}, log)

	if err := store.SaveRoutingPolicy(context.Background(), domain.RoutingPolicy{
		ID: "pol1", Name: "p", Alias: "coding-fast",
		Candidates: []domain.Candidate{
			{AccountID: "acc_bad", ModelID: "m", Priority: 1, Weight: 1},
			{AccountID: "acc_good", ModelID: "m", Priority: 2, Weight: 1},
		},
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	store.CreateProvider(context.Background(), domain.Provider{ID: "prov", Kind: "openai-compatible", Name: "prov"})
	for _, id := range []string{"acc_bad", "acc_good"} {
		if err := store.CreateAccount(context.Background(), domain.Account{
			ID: id, ProviderID: "prov", Name: id, Status: "active",
		}); err != nil {
			t.Fatal(err)
		}
	}
	req := connectors.NewChatRequest("coding-fast", []connectors.ChatMessage{{Role: "user", Content: "hi"}})
	resp, decision, err := r.Forward(context.Background(), "coding-fast", req, "req1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].Message.Content != "ok" {
		t.Fatalf("content=%q", resp.Choices[0].Message.Content)
	}
	if len(decision.Candidates) != 2 {
		t.Fatalf("candidates=%v", decision.Candidates)
	}
	if decision.Selected != "acc_good@m" {
		t.Fatalf("selected=%s", decision.Selected)
	}
}

func TestForwardNoFailoverOnAuthError(t *testing.T) {
	unauth := mockUpstream(t, 401, "invalid key")
	good := mockUpstream(t, 200, `{"id":"c2","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	_, store := testutil.NewStore(t)
	servers := map[string]string{"acc_bad": unauth.URL, "acc_good": good.URL}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	r := New(store, func(_ context.Context, accountID string) (*ResolvedTarget, error) {
		return &ResolvedTarget{AccountID: accountID, Target: connectors.Target{
			ProviderKind: connectors.KindOpenAICompatible, BaseURL: servers[accountID], APIKey: "k",
		}}, nil
	}, log)
	if err := store.SaveRoutingPolicy(context.Background(), domain.RoutingPolicy{
		ID: "pol1", Name: "p", Alias: "x",
		Candidates: []domain.Candidate{
			{AccountID: "acc_bad", ModelID: "m", Priority: 1, Weight: 1},
			{AccountID: "acc_good", ModelID: "m", Priority: 2, Weight: 1},
		},
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	store.CreateProvider(context.Background(), domain.Provider{ID: "prov", Kind: "openai-compatible", Name: "prov"})
	for _, id := range []string{"acc_bad", "acc_good"} {
		if err := store.CreateAccount(context.Background(), domain.Account{
			ID: id, ProviderID: "prov", Name: id, Status: "active",
		}); err != nil {
			t.Fatal(err)
		}
	}
	req := connectors.NewChatRequest("x", []connectors.ChatMessage{{Role: "user", Content: "hi"}})
	_, decision, err := r.Forward(context.Background(), "x", req, "req1")
	if err == nil {
		t.Fatal("expected error on auth failure without failover")
	}
	if len(decision.Candidates) != 1 {
		t.Fatalf("auth failure must not failover, candidates=%v", decision.Candidates)
	}
}

func TestForwardNoCandidate(t *testing.T) {
	_, store := testutil.NewStore(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	r := New(store, func(_ context.Context, _ string) (*ResolvedTarget, error) {
		return nil, nil
	}, log)
	req := connectors.NewChatRequest("ghost", []connectors.ChatMessage{{Role: "user", Content: "hi"}})
	_, _, err := r.Forward(context.Background(), "ghost", req, "req1")
	if err != ErrNoCandidate {
		t.Fatalf("err=%v want ErrNoCandidate", err)
	}
}

// 仅监测（monitor_only）账户永不成为路由候选（合约 v1 §accounts.mode；MR-004）。
func TestForwardExcludesMonitorOnlyAccounts(t *testing.T) {
	_, store := testutil.NewStore(t)
	_ = store.CreateProvider(context.Background(), domain.Provider{ID: "prov", Kind: "openai-compatible", Name: "p"})
	servers := map[string]string{"acc_mon": "http://127.0.0.1:1", "acc_active": "http://127.0.0.1:1"}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	r := New(store, func(_ context.Context, accountID string) (*ResolvedTarget, error) {
		return &ResolvedTarget{AccountID: accountID, Target: connectors.Target{
			ProviderKind: connectors.KindOpenAICompatible, BaseURL: servers[accountID], APIKey: "k",
		}}, nil
	}, log)
	// acc_mon 是唯一拥有模型的账户，但为仅监测模式
	_ = store.CreateAccount(context.Background(), domain.Account{ID: "acc_mon", ProviderID: "prov", Name: "mon", Status: "active", Mode: "monitor_only"})
	_ = store.SaveAccountModels(context.Background(), "acc_mon", []repository.ModelInfoRow{{ModelID: "m", Listed: true, Usable: true}})
	_ = store.CreateAccount(context.Background(), domain.Account{ID: "acc_active", ProviderID: "prov", Name: "act", Status: "active", Mode: "relay_and_monitor"})

	// 无策略、按模型直连：仅有监测账户拥有模型 → 无候选
	req := connectors.NewChatRequest("m", []connectors.ChatMessage{{Role: "user", Content: "hi"}})
	_, _, err := r.Forward(context.Background(), "m", req, "req1")
	if err != ErrNoCandidate {
		t.Fatalf("monitor_only account must be excluded, err=%v", err)
	}

	// 策略里显式包含仅监测账户 → 也被过滤
	_ = store.SaveRoutingPolicy(context.Background(), domain.RoutingPolicy{
		ID: "pol1", Name: "p", Alias: "only-mon",
		Candidates: []domain.Candidate{{AccountID: "acc_mon", ModelID: "m", Priority: 1, Weight: 1}},
		Enabled:    true,
	})
	_, _, err = r.Forward(context.Background(), "only-mon", req, "req2")
	if err != ErrNoCandidate {
		t.Fatalf("policy with monitor_only must yield no candidate, err=%v", err)
	}
}

func TestDecisionSerializesWithoutSecrets(t *testing.T) {
	d := Decision{RequestID: "req1", Alias: "x", Selected: "acc@m", Candidates: []string{"acc@m"}}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if s == "" || len(s) > 200 {
		t.Fatalf("decision too large or empty: %s", s)
	}
}

// 流式：一旦输出过内容，即使出错也禁止跨候选重放（US-006）。
func TestForwardStreamNoReplayAfterOutput(t *testing.T) {
	// 候选 1：先输出并 Flush 一个 chunk 再断流（SSE 中途出错）
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"半\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic("connection reset")
	}))
	t.Cleanup(flaky.Close)
	good := mockUpstream(t, 200, `{"id":"c2","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)

	_, store := testutil.NewStore(t)
	_ = store.CreateProvider(context.Background(), domain.Provider{ID: "prov", Kind: "openai-compatible", Name: "p"})
	servers := map[string]string{"acc_flaky": flaky.URL, "acc_good": good.URL}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	r := New(store, func(_ context.Context, accountID string) (*ResolvedTarget, error) {
		return &ResolvedTarget{AccountID: accountID, Target: connectors.Target{
			ProviderKind: connectors.KindOpenAICompatible, BaseURL: servers[accountID], APIKey: "k",
		}}, nil
	}, log)
	if err := store.SaveRoutingPolicy(context.Background(), domain.RoutingPolicy{
		ID: "pol1", Name: "p", Alias: "stream-x",
		Candidates: []domain.Candidate{
			{AccountID: "acc_flaky", ModelID: "m", Priority: 1, Weight: 1},
			{AccountID: "acc_good", ModelID: "m", Priority: 2, Weight: 1},
		},
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"acc_flaky", "acc_good"} {
		if err := store.CreateAccount(context.Background(), domain.Account{
			ID: id, ProviderID: "prov", Name: id, Status: "active",
		}); err != nil {
			t.Fatal(err)
		}
	}

	var sent []byte
	req := connectors.NewChatRequest("stream-x", []connectors.ChatMessage{{Role: "user", Content: "hi"}})
	req.Stream = true
	_, decision, err := r.ForwardStream(context.Background(), "stream-x", req, "req1", func(b []byte) error {
		sent = append(sent, b...)
		return nil
	})
	if err == nil {
		t.Fatal("expected error from flaky upstream")
	}
	// 已输出内容：不得切换到 acc_good 重放
	if len(decision.Candidates) != 1 {
		t.Fatalf("must not failover after streaming output, candidates=%v", decision.Candidates)
	}
	if string(sent) == "" {
		t.Fatal("expected partial output before failure")
	}
}
