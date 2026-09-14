package quota

import (
	"context"
	"testing"
	"time"

	"midroute/internal/domain"
	"midroute/internal/testutil"
)

func newQuotaTest(t *testing.T) *Service {
	t.Helper()
	_, store := testutil.NewStore(t)
	svc := New(store)
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return now }
	return svc
}

func TestObservationToSnapshotsSemantics(t *testing.T) {
	svc := newQuotaTest(t)
	pct := 35.0
	win := 300
	reset := time.Date(2026, 9, 1, 5, 0, 0, 0, time.UTC)
	obs := Observation{
		PlanType:    "pro",
		ActiveLimit: "primary",
		Snapshots: []Snapshot{
			{LimitName: "primary", UsedPercent: &pct, WindowMinutes: &win, ResetAt: &reset, Allowed: boolp(true)},
			{LimitName: "secondary", UsedPercent: nil, WindowMinutes: nil, ResetAt: nil}, // 未知
		},
	}
	snaps := svc.ObservationToSnapshots("acc1", obs)
	if len(snaps) != 2 {
		t.Fatalf("len=%d", len(snaps))
	}
	// 未知值保持 nil，不写成 0
	sec := snaps[1]
	if sec.Used != nil || sec.Limit != nil || sec.Remaining != nil {
		t.Fatalf("unknown must stay nil: %+v", sec)
	}
	// 已知值换算 remaining = 100 - used（分母明确）
	pri := snaps[0]
	if pri.Used == nil || *pri.Used != 35 || pri.Remaining == nil || *pri.Remaining != 65 {
		t.Fatalf("primary semantics wrong: %+v", pri)
	}
	if pri.ResetAt == nil || *pri.ResetAt != "2026-09-01T05:00:00Z" {
		t.Fatalf("resetAt=%v", pri.ResetAt)
	}
}

func TestRecordAndLatest(t *testing.T) {
	svc := newQuotaTest(t)
	ctx := context.Background()
	store := svc.Store
	if err := store.CreateProvider(ctx, domain.Provider{ID: "p1", Kind: "codex", Name: "codex"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAccount(ctx, domain.Account{ID: "a1", ProviderID: "p1", Name: "n", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	pct := 10.0
	obs := Observation{Snapshots: []Snapshot{{LimitName: "primary", UsedPercent: &pct}}}
	n, err := svc.RecordObservation(ctx, "a1", obs)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	// 二次记录产生历史（不覆盖）
	pct2 := 20.0
	obs2 := Observation{Snapshots: []Snapshot{{LimitName: "primary", UsedPercent: &pct2}}}
	if _, err := svc.RecordObservation(ctx, "a1", obs2); err != nil {
		t.Fatal(err)
	}
	snaps, _ := store.ListQuotaSnapshots(ctx, "a1")
	if len(snaps) != 2 {
		t.Fatalf("history len=%d", len(snaps))
	}
	// latest 是最新一条且 source != manual
	latest, ok, _ := store.LatestQuotaSnapshot(ctx, "a1", "primary")
	if !ok || latest.Used == nil || *latest.Used != 20 {
		t.Fatalf("latest=%+v ok=%v", latest, ok)
	}
}

func TestManualDoesNotOverwriteOfficial(t *testing.T) {
	svc := newQuotaTest(t)
	ctx := context.Background()
	store := svc.Store
	_ = store.CreateProvider(ctx, domain.Provider{ID: "p1", Kind: "codex", Name: "c"})
	_ = store.CreateAccount(ctx, domain.Account{ID: "a1", ProviderID: "p1", Name: "n", Status: "active"})

	// 官方观测
	pct := 10.0
	_, _ = svc.RecordObservation(ctx, "a1", Observation{Snapshots: []Snapshot{{LimitName: "primary", UsedPercent: &pct}}})
	// 手工补充
	used := 90.0
	m, err := svc.ManualSupplement(ctx, "a1", "admin", domain.QuotaSnapshot{
		WindowType: domain.QuotaWindowPrimary, Used: &used,
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.Source != domain.QuotaSourceManual || m.Operator != "admin" {
		t.Fatalf("manual meta wrong: %+v", m)
	}
	// 手工与官方并存；LatestQuotaSnapshot 排除 manual → 仍返回官方最新
	latest, ok, _ := store.LatestQuotaSnapshot(ctx, "a1", "primary")
	if !ok || latest.Source != domain.QuotaSourceObserved || *latest.Used != 10 {
		t.Fatalf("latest should be official observed: %+v ok=%v", latest, ok)
	}
	// 全部快照（含手工）能列出
	all, _ := store.ListQuotaSnapshots(ctx, "a1")
	if len(all) != 2 {
		t.Fatalf("manual+official len=%d", len(all))
	}
}

func boolp(b bool) *bool { return &b }
