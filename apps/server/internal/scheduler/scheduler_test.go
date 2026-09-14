package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"midroute/internal/domain"
	"midroute/internal/repository"
	"midroute/internal/testutil"
)

func newScheduler(t *testing.T) (*Scheduler, *repository.Store) {
	t.Helper()
	_, store := testutil.NewStore(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	s := New(store, log)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var tick int64
	s.SetClock(func() time.Time { return base.Add(time.Duration(atomic.LoadInt64(&tick)) * time.Minute) })
	return s, store
}

func advance(s *Scheduler) { _ = s.now }

// 用一个可共享的 tick 计数
func TestSchedulerSuccessAndDedup(t *testing.T) {
	s, store := newScheduler(t)
	var calls atomic.Int32
	s.Register("models", func(ctx context.Context, accountID string) error {
		calls.Add(1)
		return nil
	})
	ctx := context.Background()
	_ = store.CreateProvider(ctx, provider1())
	_ = store.CreateAccount(ctx, account1())

	// 第一次执行
	job1, err := s.Enqueue(ctx, "acc1", "models")
	if err != nil {
		t.Fatal(err)
	}
	if job1.State != "success" {
		t.Fatalf("state=%s", job1.State)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}

	// 第二次执行（任务按 账户+能力 去重，复用同一 job 行并再次执行）
	job2, err := s.Enqueue(ctx, "acc1", "models")
	if err != nil {
		t.Fatal(err)
	}
	if job2.State != "success" || job2.ID != job1.ID {
		t.Fatalf("job2=%s id=%s", job2.State, job2.ID)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

// 并发同一 key 只触发一次执行（单飞）。
func TestSchedulerConcurrentDedup(t *testing.T) {
	s, store := newScheduler(t)
	var calls atomic.Int32
	s.Register("models", func(ctx context.Context, accountID string) error {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return nil
	})
	ctx := context.Background()
	_ = store.CreateProvider(ctx, provider1())
	_ = store.CreateAccount(ctx, account1())

	const n = 8
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Enqueue(ctx, "acc1", "models")
			results[i] = err
		}(i)
	}
	wg.Wait()
	success := 0
	busy := 0
	for _, err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrBusy) {
			busy++
		} else {
			t.Fatalf("unexpected err: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("success=%d want 1", success)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d want 1 (dedup)", calls.Load())
	}
}

// 失败退避与重试上限。
func TestSchedulerBackoffAndRetryLimit(t *testing.T) {
	s, store := newScheduler(t)
	s.SetConfig(Config{JobTimeout: time.Second, BaseBackoff: 10 * time.Second, MaxRetries: 2})
	alwaysFail := errors.New("boom")
	var calls atomic.Int32
	s.Register("capabilities", func(ctx context.Context, accountID string) error {
		calls.Add(1)
		return alwaysFail
	})
	ctx := context.Background()
	_ = store.CreateProvider(ctx, provider1())
	_ = store.CreateAccount(ctx, account1())

	// 第 1 次失败
	j1, err := s.Enqueue(ctx, "acc1", "capabilities")
	if err != nil {
		t.Fatal(err)
	}
	if j1.State != "failed" || j1.RetryCount != 1 {
		t.Fatalf("j1=%s retry=%d", j1.State, j1.RetryCount)
	}
	// next_run_at 存在（退避）
	if j1.NextRunAt == "" {
		t.Fatal("missing next_run_at")
	}
	// 第 2 次：retry=2 到达上限
	j2, err := s.Enqueue(ctx, "acc1", "capabilities")
	if err != nil {
		t.Fatal(err)
	}
	if j2.RetryCount != 2 || j2.State != "failed" {
		t.Fatalf("j2=%s retry=%d", j2.State, j2.RetryCount)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

// 重启把遗留 running/pending 标 interrupted。
func TestMarkInterruptedOnRestart(t *testing.T) {
	_, store := testutil.NewStore(t)
	ctx := context.Background()
	_ = store.CreateProvider(ctx, provider1())
	_ = store.CreateAccount(ctx, account1())
	_ = store.UpsertRefreshJob(ctx, repository.RefreshJob{ID: "job_x", AccountID: "acc1", Capability: "models", State: "running"})
	_ = store.UpsertRefreshJob(ctx, repository.RefreshJob{ID: "job_y", AccountID: "acc1", Capability: "capabilities", State: "pending"})
	n, err := store.MarkInterruptedRefreshJobs(ctx)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	all, _ := store.ListRefreshJobs(ctx, "acc1")
	for _, j := range all {
		if j.State != "interrupted" {
			t.Fatalf("job %s state=%s", j.ID, j.State)
		}
	}
}

func provider1() domain.Provider { return domain.Provider{ID: "p1", Kind: "openai", Name: "p"} }
func account1() domain.Account {
	return domain.Account{ID: "acc1", ProviderID: "p1", Name: "n", Status: "active"}
}
