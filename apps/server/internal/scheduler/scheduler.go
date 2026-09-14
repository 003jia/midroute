// Package scheduler 提供后台刷新任务（MR-010）。
// 去重（账户+能力单飞）、超时、退避、失败恢复、重启中断标记；时钟可注入。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"midroute/internal/errs"
	"midroute/internal/repository"
)

// JobFunc 一个刷新能力的具体执行体。返回 err 表示该轮失败。
type JobFunc func(ctx context.Context, accountID string) error

// Config 调度配置。
type Config struct {
	// JobTimeout 单次执行超时。
	JobTimeout time.Duration
	// BaseBackoff 失败退避基数。
	BaseBackoff time.Duration
	// MaxRetries 失败最大重试次数（超过后标记 failed 且暂停自动重试）。
	MaxRetries int
}

// DefaultConfig 默认配置（MR-010：单次 15s、全局并发由调用方控制）。
func DefaultConfig() Config {
	return Config{JobTimeout: 15 * time.Second, BaseBackoff: 5 * time.Second, MaxRetries: 5}
}

// Scheduler 任务调度器。
type Scheduler struct {
	store    *repository.Store
	log      *slog.Logger
	cfg      Config
	jobs     map[string]JobFunc
	mu       sync.Mutex
	inflight map[string]bool
	now      func() time.Time
}

// New 创建调度器。
func New(store *repository.Store, log *slog.Logger) *Scheduler {
	return &Scheduler{
		store: store, log: log, cfg: DefaultConfig(),
		jobs: map[string]JobFunc{}, inflight: map[string]bool{},
		now: func() time.Time { return time.Now().UTC() },
	}
}

// SetClock 注入时钟（测试用）。
func (s *Scheduler) SetClock(fn func() time.Time) { s.now = fn }

// SetConfig 覆盖默认配置。
func (s *Scheduler) SetConfig(c Config) { s.cfg = c }

// Register 注册一个刷新能力。
func (s *Scheduler) Register(capability string, fn JobFunc) { s.jobs[capability] = fn }

// ErrBusy 同账户同能力已有任务在执行/排队。
var ErrBusy = errors.New("scheduler: job already in flight")

// Enqueue 入队一个刷新任务：去重后立即执行（同步完成并返回任务状态）。
// 若同 key 已有 running/pending 任务，返回该任务（不产生并发风暴，MR-010）。
func (s *Scheduler) Enqueue(ctx context.Context, accountID, capability string) (*repository.RefreshJob, error) {
	fn, ok := s.jobs[capability]
	if !ok {
		return nil, errs.New(errs.CodeUnsupported, "不支持的刷新能力: "+capability)
	}
	key := accountID + "|" + capability

	// 内存单飞
	s.mu.Lock()
	if s.inflight[key] {
		s.mu.Unlock()
		if j, ok, _ := s.store.GetRefreshJobByKey(ctx, accountID, capability); ok {
			return &j, ErrBusy
		}
		return nil, ErrBusy
	}
	s.inflight[key] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inflight, key)
		s.mu.Unlock()
	}()

	// 已有运行中任务（重启场景下）→ 复用
	if j, ok, _ := s.store.GetRefreshJobByKey(ctx, accountID, capability); ok && (j.State == "running" || j.State == "pending") {
		return &j, ErrBusy
	}

	now := s.now()
	prevRetry := 0
	if cur, ok, _ := s.store.GetRefreshJobByKey(ctx, accountID, capability); ok {
		prevRetry = cur.RetryCount
	}
	job := repository.RefreshJob{
		ID:         "job_" + fmt.Sprintf("%x", now.UnixNano()),
		AccountID:  accountID,
		Capability: capability,
		State:      "running",
		StartedAt:  now.Format(time.RFC3339),
		CreatedAt:  now.Format(time.RFC3339),
		RetryCount: prevRetry,
	}
	if err := s.store.UpsertRefreshJob(ctx, job); err != nil {
		return nil, err
	}

	s.runJob(ctx, job, fn)
	final, err := s.store.GetRefreshJob(ctx, job.ID)
	return &final, err
}

// runJob 执行任务并落库终态（含退避计算）。
func (s *Scheduler) runJob(ctx context.Context, job repository.RefreshJob, fn JobFunc) {
	jobCtx, cancel := context.WithTimeout(ctx, s.cfg.JobTimeout)
	defer cancel()

	err := fn(jobCtx, job.AccountID)
	finished := s.now().Format(time.RFC3339)
	if err == nil {
		_ = s.store.UpdateRefreshJobState(context.Background(), job.ID, "success", finished, finished, finished, 0, "")
		if s.log != nil {
			s.log.Info("refresh job ok", "job", job.ID, "account", job.AccountID, "cap", job.Capability)
		}
		return
	}
	// 失败：退避 + 重试上限
	var code string
	if e, ok := errs.As(err); ok {
		code = string(e.Code)
	} else {
		code = "error"
	}
	retry := 0
	if cur, ok, _ := s.store.GetRefreshJobByKey(context.Background(), job.AccountID, job.Capability); ok {
		retry = cur.RetryCount + 1
	}
	nextRun := s.now().Add(s.backoff(retry)).Format(time.RFC3339)
	if retry >= s.cfg.MaxRetries {
		_ = s.store.UpdateRefreshJobState(context.Background(), job.ID, "failed", finished, nextRun, "", retry, code)
		if s.log != nil {
			s.log.Warn("refresh job failed", "job", job.ID, "retry", retry, "err", err)
		}
		return
	}
	_ = s.store.UpdateRefreshJobState(context.Background(), job.ID, "failed", finished, nextRun, "", retry, code)
	if s.log != nil {
		s.log.Warn("refresh job failed (will retry)", "job", job.ID, "retry", retry, "next", nextRun, "err", err)
	}
}

// backoff 指数退避（含 Retry-After 语义的上限）。
func (s *Scheduler) backoff(retry int) time.Duration {
	if retry <= 0 {
		return s.cfg.BaseBackoff
	}
	d := s.cfg.BaseBackoff * time.Duration(1<<min(retry, 5))
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
