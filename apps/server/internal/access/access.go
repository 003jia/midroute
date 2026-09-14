// Package access 实现推理项目与访问令牌的鉴权逻辑（MR-016 / FR-41/42）。
//
// 职责边界：
//   - 管理会话与管理员密钥绝不自动成为推理凭证（httpserver 的 Guard 不覆盖 /v1/*）；
//   - 令牌明文只在签发时返回一次，库中仅存 SHA-256 摘要与前缀；
//   - 鉴权检查顺序：存在性 → 启用 → 未过期 → 模型白名单 → 并发上限，
//     全部在请求上游之前完成；
//   - 并发上限为进程内计数（单进程部署）；取消/出错路径经 release 归还。
package access

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"midroute/internal/domain"
	"midroute/internal/repository"
)

// ErrUnauthorized 令牌缺失/未知/禁用/过期（请求上游前拒绝）。
var ErrUnauthorized = errors.New("access: token missing, unknown, disabled or expired")

// ErrModelForbidden 令牌模型白名单不包含请求的模型（请求上游前拒绝）。
var ErrModelForbidden = errors.New("access: model not allowed for token")

// ErrConcurrencyLimit 令牌并发达到上限（请求上游前拒绝）。
var ErrConcurrencyLimit = errors.New("access: token concurrency limit reached")

// Service 提供令牌鉴权与并发控制。
type Service struct {
	store *repository.Store

	mu    sync.Mutex
	inUse map[string]int // tokenID -> 当前并发推理数
	nowFn func() time.Time
}

// New 创建 Service。
func New(store *repository.Store) *Service {
	return &Service{store: store, inUse: map[string]int{}, nowFn: func() time.Time { return time.Now().UTC() }}
}

// SetNow 注入时钟（测试用）。
func (s *Service) SetNow(fn func() time.Time) { s.nowFn = fn }

// Hash 计算令牌摘要。
func Hash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", sum)
}

// Authenticate 校验 Bearer 令牌并返回令牌信息；同时刷新 last_used_at。
// 未知/禁用/过期统一返回 ErrUnauthorized（不区分原因，避免探测）。
func (s *Service) Authenticate(ctx context.Context, r *http.Request) (domain.Token, error) {
	raw := bearerToken(r)
	if raw == "" {
		return domain.Token{}, ErrUnauthorized
	}
	t, err := s.store.FindTokenByKeyHash(ctx, Hash(raw))
	if err != nil {
		return domain.Token{}, ErrUnauthorized
	}
	if !t.Enabled {
		return domain.Token{}, ErrUnauthorized
	}
	if t.ExpiresAt != "" {
		exp, err := time.Parse(time.RFC3339, t.ExpiresAt)
		if err != nil || s.nowFn().After(exp) {
			return domain.Token{}, ErrUnauthorized
		}
	}
	_ = s.store.TouchToken(ctx, t.ID)
	return t, nil
}

// ModelAllowed 判断令牌是否可调用该模型。白名单为空数组时允许全部。
// 匹配使用逻辑模型名（别名）与上游模型 ID 的并集语义：白名单条目与
// 请求模型名精确相等才放行，不做前缀/模糊匹配。
func ModelAllowed(t domain.Token, model string) bool {
	if strings.TrimSpace(t.ModelWhitelist) == "" || t.ModelWhitelist == "[]" {
		return true
	}
	var list []string
	if err := json.Unmarshal([]byte(t.ModelWhitelist), &list); err != nil {
		// 白名单损坏按最严格处理：全拒
		return false
	}
	for _, m := range list {
		if m == model {
			return true
		}
	}
	return false
}

// Acquire 占用一个并发槽。max<=0 表示不限。达到上限返回 ErrConcurrencyLimit。
// 返回的 release 必须被调用（defer），取消/出错路径同样归还。
func (s *Service) Acquire(tokenID string, max int) (release func(), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if max > 0 && s.inUse[tokenID] >= max {
		return nil, ErrConcurrencyLimit
	}
	s.inUse[tokenID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.inUse[tokenID] > 0 {
				s.inUse[tokenID]--
			}
		})
	}, nil
}

// InUse 返回令牌当前占用数（诊断用）。
func (s *Service) InUse(tokenID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inUse[tokenID]
}

// bearerToken 提取 Authorization: Bearer <token>。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}
