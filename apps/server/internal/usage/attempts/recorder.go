package attempts

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"midroute/internal/repository"
	"midroute/internal/router"
)

// Recorder 实现 router.AttemptHook：把每次真实上游尝试写入 request_attempts（MR-015）。
// 幂等键 (request_id, attempt_id)；终态写入使用独立有界 context，不因客户端取消丢记录。
type Recorder struct {
	Store *repository.Store
}

// NewRecorder 创建尝试记录器。
func NewRecorder(store *repository.Store) *Recorder {
	return &Recorder{Store: store}
}

type ctxKey string

const (
	accessTokenCtxKey ctxKey = "midroute.access_token_id"
	projectCtxKey     ctxKey = "midroute.project_id"
)

// WithIdentity 把访问令牌/项目注入 context（推理请求链路）。
func WithIdentity(ctx context.Context, tokenID, projectID string) context.Context {
	ctx = context.WithValue(ctx, accessTokenCtxKey, tokenID)
	return context.WithValue(ctx, projectCtxKey, projectID)
}

func identityFrom(ctx context.Context) (tokenID, projectID string) {
	tokenID, _ = ctx.Value(accessTokenCtxKey).(string)
	projectID, _ = ctx.Value(projectCtxKey).(string)
	return
}

// StartAttempt 记录尝试开始，返回 attempt_id。
func (r *Recorder) StartAttempt(ctx context.Context, info router.AttemptInfo) string {
	tokenID, projectID := identityFrom(ctx)
	attemptID := "att_" + randHex(8)
	recordCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = r.Store.CreateAttempt(recordCtx, repository.RequestAttempt{
		ID:            "ra_" + randHex(8),
		RequestID:     info.RequestID,
		AttemptID:     attemptID,
		AccountID:     info.AccountID,
		LogicalModel:  info.LogicalModel,
		ActualModel:   info.ActualModel,
		Protocol:      info.Protocol,
		AccessTokenID: tokenID,
		ProjectID:     projectID,
		Status:        "started",
		OccurredAt:    time.Now().UTC().Format(time.RFC3339),
	})
	return attemptID
}

// FinishAttempt 幂等写入尝试终态。
func (r *Recorder) FinishAttempt(ctx context.Context, requestID, attemptID string, res router.AttemptResult) {
	recordCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = r.Store.FinishAttempt(recordCtx, repository.RequestAttempt{
		RequestID:        requestID,
		AttemptID:        attemptID,
		Status:           res.Status,
		InputTokens:      res.InputTokens,
		OutputTokens:     res.OutputTokens,
		CacheReadTokens:  res.CacheReadTokens,
		CacheWriteTokens: res.CacheWriteTokens,
		ReasoningTokens:  res.ReasoningTokens,
		Metering:         res.Metering,
		LatencyMS:        res.LatencyMS,
		StatusCode:       res.StatusCode,
		ErrorClass:       res.ErrorClass,
		ReasonCode:       res.ReasonCode,
		FinishedAt:       time.Now().UTC().Format(time.RFC3339),
	})
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}
