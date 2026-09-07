package oauth

import (
	"sync"
	"time"
)

// stateTTL 是授权会话（state）的有效期。超时未完成的授权必须重新发起。
const stateTTL = 10 * time.Minute

// stateEntry 是一次待完成的授权会话。verifier 只保存在服务端内存，
// 绝不进入 URL 或日志。
type stateEntry struct {
	verifier  string
	service   string
	account   string
	expiresAt time.Time
}

// stateStore 保存待消费的授权 state：随机、短期、单次消费。
type stateStore struct {
	mu      sync.Mutex
	entries map[string]stateEntry
}

func newStateStore() *stateStore {
	return &stateStore{entries: map[string]stateEntry{}}
}

// put 登记一个新的授权会话，返回 state。now 由调用方注入，保证可测试。
func (s *stateStore) put(now time.Time, verifier, service, account string) (string, error) {
	state, err := newState()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked(now)
	s.entries[state] = stateEntry{
		verifier:  verifier,
		service:   service,
		account:   account,
		expiresAt: now.Add(stateTTL),
	}
	return state, nil
}

// consume 取走并删除一个 state（单次消费）。不存在返回 ErrStateInvalid，
// 已过期返回 ErrStateExpired——两者都要求重新发起授权。
// 过期项的内存回收在 put 时统一进行，consume 保持精确判别。
func (s *stateStore) consume(now time.Time, state string) (stateEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[state]
	if !ok {
		return stateEntry{}, ErrStateInvalid
	}
	delete(s.entries, state)
	if now.After(entry.expiresAt) {
		return stateEntry{}, ErrStateExpired
	}
	return entry, nil
}

// purgeLocked 清理过期会话。调用方须持锁。
func (s *stateStore) purgeLocked(now time.Time) {
	for k, e := range s.entries {
		if now.After(e.expiresAt) {
			delete(s.entries, k)
		}
	}
}
