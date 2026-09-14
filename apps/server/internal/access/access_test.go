package access

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"midroute/internal/domain"
	"midroute/internal/testutil"
)

func reqWithToken(tok string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	return r
}

func TestAuthenticateLifecycle(t *testing.T) {
	_, store := testutil.NewStore(t)
	svc := New(store)
	raw := "mrt_testtoken123456"
	tok := domain.Token{
		ID: "tok_1", Name: "t", KeyHash: Hash(raw), KeyPrefix: raw[:10],
		Enabled: true, CreatedAt: "2026-01-01T00:00:00Z",
		ModelWhitelist: `["gpt-x"]`, MaxConcurrency: 2,
	}
	if err := store.CreateToken(context.Background(), tok); err != nil {
		t.Fatal(err)
	}

	// 无令牌 / 错令牌 / 正确令牌
	if _, err := svc.Authenticate(context.Background(), reqWithToken("")); err == nil {
		t.Fatal("missing token must fail")
	}
	if _, err := svc.Authenticate(context.Background(), reqWithToken("mrt_wrong")); err == nil {
		t.Fatal("wrong token must fail")
	}
	got, err := svc.Authenticate(context.Background(), reqWithToken(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "tok_1" {
		t.Fatalf("id=%s", got.ID)
	}

	// 禁用后拒绝
	if err := store.SetTokenEnabled(context.Background(), "tok_1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(context.Background(), reqWithToken(raw)); err == nil {
		t.Fatal("disabled token must fail")
	}
	_ = store.SetTokenEnabled(context.Background(), "tok_1", true)

	// 过期拒绝（注入时钟）
	svc.SetNow(func() time.Time { return time.Now().UTC().Add(time.Hour) })
	expTok := domain.Token{
		ID: "tok_2", Name: "e", KeyHash: Hash("mrt_expiring00000"), KeyPrefix: "mrt_expir",
		Enabled: true, CreatedAt: "2026-01-01T00:00:00Z",
		ExpiresAt: time.Now().UTC().Add(time.Minute).Format(time.RFC3339),
	}
	if err := store.CreateToken(context.Background(), expTok); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(context.Background(), reqWithToken("mrt_expiring00000")); err == nil {
		t.Fatal("expired token must fail")
	}
	svc.SetNow(func() time.Time { return time.Now().UTC() })
}

func TestModelAllowed(t *testing.T) {
	cases := []struct {
		whitelist string
		model     string
		want      bool
	}{
		{"", "anything", true},
		{"[]", "anything", true},
		{`["gpt-x", "coding-fast"]`, "gpt-x", true},
		{`["gpt-x"]`, "gpt-y", false},
		{`["gpt-x"]`, "gpt-xyz", false}, // 精确匹配，不做前缀
		{"corrupt-json", "gpt-x", false},
	}
	for i, c := range cases {
		tok := domain.Token{ModelWhitelist: c.whitelist}
		if got := ModelAllowed(tok, c.model); got != c.want {
			t.Fatalf("case %d: whitelist=%q model=%q got=%v", i, c.whitelist, c.model, got)
		}
	}
}

func TestConcurrencyAcquireRelease(t *testing.T) {
	_, store := testutil.NewStore(t)
	svc := New(store)

	r1, err := svc.Acquire("tok_c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Acquire("tok_c", 1); err == nil {
		t.Fatal("second acquire must hit limit")
	}
	r1()
	r2, err := svc.Acquire("tok_c", 1)
	if err != nil {
		t.Fatalf("after release must succeed: %v", err)
	}
	// 重复 release 不产生负数
	r2()
	r2()
	if got := svc.InUse("tok_c"); got != 0 {
		t.Fatalf("inUse=%d", got)
	}

	// max=0 不限制
	for i := 0; i < 5; i++ {
		if _, err := svc.Acquire("tok_u", 0); err != nil {
			t.Fatal(err)
		}
	}
}

// TestConcurrencyParallel 并发获取/释放的数据竞争（配合 -race）。
func TestConcurrencyParallel(t *testing.T) {
	_, store := testutil.NewStore(t)
	svc := New(store)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if rel, err := svc.Acquire("tok_p", 4); err == nil {
					rel()
				}
			}
		}()
	}
	wg.Wait()
	if got := svc.InUse("tok_p"); got != 0 {
		t.Fatalf("leaked slots: %d", got)
	}
}

func TestBearerExtraction(t *testing.T) {
	r := reqWithToken("abc")
	if bearerToken(r) != "abc" {
		t.Fatal("bearer extraction failed")
	}
	r2, _ := http.NewRequest(http.MethodGet, "/", nil)
	r2.Header.Set("Authorization", "bearer abc") // 大小写不敏感
	if bearerToken(r2) != "abc" {
		t.Fatal("case-insensitive prefix expected")
	}
	if !strings.Contains(Hash("x"), "") {
		t.Fatal("unreachable")
	}
}
