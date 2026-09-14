package repository

import (
	"context"
	"database/sql"
	"testing"

	"midroute/internal/db"
	"midroute/internal/domain"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := db.Migrate(ctx, conn); err != nil {
		t.Fatal(err)
	}
	return New(conn)
}

var _ *sql.DB

func TestProviderAndAccountLifecycle(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	// Provider 增查改删
	p := domain.Provider{ID: "prov1", Kind: "openai", Name: "OpenAI", BaseURL: "https://api.openai.com"}
	if err := store.CreateProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateProvider(ctx, "prov1", strPtr("OpenAI 官方"), strPtr("https://api.openai.com/v1")); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetProvider(ctx, "prov1")
	if err != nil || got.Name != "OpenAI 官方" || got.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("update failed: %+v %v", got, err)
	}
	if err := store.UpdateProvider(ctx, "ghost", strPtr("x"), nil); err == nil {
		t.Fatal("update missing provider must fail")
	}

	// 账户
	acc := domain.Account{ID: "acc1", ProviderID: "prov1", Name: "主账户", Status: "active"}
	if err := store.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAccountName(ctx, "acc1", "改名"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAccountStatus(ctx, "acc1", "disabled"); err != nil {
		t.Fatal(err)
	}
	gotAcc, _ := store.GetAccount(ctx, "acc1")
	if gotAcc.Name != "改名" || gotAcc.Status != "disabled" {
		t.Fatalf("account update failed: %+v", gotAcc)
	}

	// Provider 被账户引用 → 删除冲突检查
	if n, _ := store.CountAccountsByProvider(ctx, "prov1"); n != 1 {
		t.Fatalf("count=%d", n)
	}

	// 账户删除
	if err := store.DeleteAccount(ctx, "acc1"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteAccount(ctx, "acc1"); err == nil {
		t.Fatal("double delete must fail")
	}
	if err := store.DeleteProvider(ctx, "prov1"); err != nil {
		t.Fatal(err)
	}
}

func TestTokenCRUDAndLookup(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	tok := domain.Token{
		ID: "tok1", Name: "默认令牌",
		KeyHash: "aaaa", KeyPrefix: "mrt_aaa",
		Enabled: true, CreatedAt: "now",
	}
	if err := store.CreateToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	// 哈希查找
	found, err := store.FindTokenByKeyHash(ctx, "aaaa")
	if err != nil || found.ID != "tok1" || !found.Enabled {
		t.Fatalf("lookup failed: %+v %v", found, err)
	}
	if _, err := store.FindTokenByKeyHash(ctx, "nope"); err == nil {
		t.Fatal("unknown hash must not be found")
	}
	// 列表不含哈希
	list, _ := store.ListTokens(ctx)
	if len(list) != 1 || list[0].KeyHash != "" {
		t.Fatalf("list must not expose hash: %+v", list)
	}
	// 禁用
	if err := store.SetTokenEnabled(ctx, "tok1", false); err != nil {
		t.Fatal(err)
	}
	found, _ = store.FindTokenByKeyHash(ctx, "aaaa")
	if found.Enabled {
		t.Fatal("token must be disabled")
	}
	// 删除
	if err := store.DeleteToken(ctx, "tok1"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteToken(ctx, "tok1"); err == nil {
		t.Fatal("double delete must fail")
	}
}

func TestAccountModelAndUsageRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.CreateProvider(ctx, domain.Provider{ID: "p1", Kind: "openai", Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAccount(ctx, domain.Account{ID: "a1", ProviderID: "p1", Name: "n", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertModels(ctx, []domain.Model{
		{ID: "p1|gpt-4o", ProviderID: "p1", UpstreamID: "gpt-4o"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAccountModels(ctx, "a1", []ModelInfoRow{{ModelID: "p1|gpt-4o", Listed: true, Usable: true}}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := store.CountAccountModel(ctx, "a1", "p1|gpt-4o", &n); err != nil || n != 1 {
		t.Fatalf("account model count=%d err=%v", n, err)
	}

	if err := store.RecordUsageEvent(ctx, UsageEvent{
		ID: "ue1", RequestID: "r1", AccountID: "a1", ModelID: "gpt-4o",
		InputTokens: 10, OutputTokens: 5, StatusCode: 200,
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.CountUsageEventsByAccount(ctx, "a1"); n != 1 {
		t.Fatalf("usage count=%d", n)
	}

	// 路由候选引用计数
	if err := store.SaveRoutingPolicy(ctx, domain.RoutingPolicy{
		ID: "pol1", Name: "p", Alias: "alias-x",
		Candidates: []domain.Candidate{{AccountID: "a1", ModelID: "gpt-4o", Priority: 1, Weight: 1}},
		Enabled:    true,
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.CountRoutingCandidatesByAccount(ctx, "a1"); n != 1 {
		t.Fatalf("candidate refs=%d", n)
	}
}

func TestAttemptIdempotency(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	// 幂等键 (request_id, attempt_id)：CreateAttempt 重复不新增行
	for i := 0; i < 2; i++ {
		if err := store.CreateAttempt(ctx, RequestAttempt{
			ID: "ra1", RequestID: "req1", AttemptID: "att1", AccountID: "a1", LogicalModel: "x", ActualModel: "gpt-4o",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// FinishAttempt 重复提交不会新增计费（仍是同一行，Token 只记一次）
	f := RequestAttempt{
		RequestID: "req1", AttemptID: "att1", Status: "success",
		InputTokens: 10, OutputTokens: 5, Metering: "reported",
	}
	for i := 0; i < 3; i++ {
		if err := store.FinishAttempt(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	list, err := store.ListAttemptsByRequest(ctx, "req1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("attempts=%d want 1 (idempotent)", len(list))
	}
	if list[0].Status != "success" || list[0].InputTokens != 10 {
		t.Fatalf("attempt=%+v", list[0])
	}
}

// 重启恢复：started 尝试标 interrupted。
func TestMarkStaleAttemptsInterrupted(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	_ = store.CreateAttempt(ctx, RequestAttempt{ID: "ra1", RequestID: "r1", AttemptID: "a1"})
	_ = store.CreateAttempt(ctx, RequestAttempt{ID: "ra2", RequestID: "r2", AttemptID: "a2"})
	_ = store.FinishAttempt(ctx, RequestAttempt{RequestID: "r2", AttemptID: "a2", Status: "success"})
	n, err := store.MarkStaleAttemptsInterrupted(ctx)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	ra1, _ := store.ListAttemptsByRequest(ctx, "r1")
	if len(ra1) != 1 || ra1[0].Status != "interrupted" {
		t.Fatalf("r1=%+v", ra1)
	}
}

func strPtr(s string) *string { return &s }
