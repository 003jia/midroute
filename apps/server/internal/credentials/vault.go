// Package vault 提供凭据安全存储与脱敏能力。
//
// 设计原则（对齐计划 §6.2 Credential Vault 与 §10 安全基线）：
//   - 数据库/配置文件只保存 credential reference（服务名 + 账号名 + 指纹），不保存原始密钥。
//   - 原始凭据仅在调用时短暂进入后端内存，读取后由调用方尽快清零。
//   - 所有日志与结构化输出经 Redactor 统一脱敏，防止密钥外泄。
//   - macOS 优先使用系统 Keychain；其他平台由调用方注入自定义实现。
package credentials

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
)

// ErrNotFound 表示密钥库中不存在对应条目。
var ErrNotFound = errors.New("vault: entry not found")

// SecretRef 是存储在配置/数据库中的安全引用，不含任何原始密钥。
type SecretRef struct {
	VaultProvider string `json:"vault_provider"`
	Service       string `json:"service"`
	Account       string `json:"account"`
	Fingerprint   string `json:"fingerprint"`
	CreatedAt     string `json:"created_at"`
}

// Secret 是一次读取到的原始凭据。使用完必须调用 Zero() 清零内存。
type Secret struct {
	Value []byte
}

// Zero 清零内存中的凭据，减少残留风险。
func (s *Secret) Zero() {
	for i := range s.Value {
		s.Value[i] = 0
	}
	s.Value = s.Value[:0]
}

// Fingerprint 计算凭据的脱敏指纹（SHA-256 十六进制，前 12 位作为短指纹）。
func Fingerprint(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

// ShortFingerprint 返回用于展示的短指纹。
func ShortFingerprint(value []byte) string {
	fp := Fingerprint(value)
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}

// Vault 抽象密钥库存取。实现方只负责机密存取，不感知业务语义。
type Vault interface {
	// Store 保存凭据并返回引用。
	Store(service, account string, secret []byte) (SecretRef, error)
	// Get 读取凭据。
	Get(ref SecretRef) (Secret, error)
	// Delete 删除凭据。
	Delete(ref SecretRef) error
	// Persistent 报告凭据是否跨重启持久保存。
	// 内存实现返回 false：OAuth 等长期凭据在非持久库中必须明确失败，
	// 不允许以"重启即丢"的内存库伪装持久连接（MR-007 验收）。
	Persistent() bool
}

// InMemoryVault 是供测试使用的内存实现。
type InMemoryVault struct {
	mu  sync.RWMutex
	db  map[string][]byte
	ref map[string]SecretRef
}

// NewInMemoryVault 创建内存密钥库。
func NewInMemoryVault() *InMemoryVault {
	return &InMemoryVault{db: map[string][]byte{}, ref: map[string]SecretRef{}}
}

// Store 实现 Vault。
func (v *InMemoryVault) Store(service, account string, secret []byte) (SecretRef, error) {
	key := service + "|" + account
	ref := SecretRef{
		VaultProvider: "memory",
		Service:       service,
		Account:       account,
		Fingerprint:   ShortFingerprint(secret),
		CreatedAt:     nowUTC(),
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.db[key] = append([]byte(nil), secret...)
	v.ref[key] = ref
	return ref, nil
}

// Get 实现 Vault。
func (v *InMemoryVault) Get(ref SecretRef) (Secret, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	val, ok := v.db[ref.Service+"|"+ref.Account]
	if !ok {
		return Secret{}, ErrNotFound
	}
	return Secret{Value: append([]byte(nil), val...)}, nil
}

// Delete 实现 Vault。
func (v *InMemoryVault) Delete(ref SecretRef) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	key := ref.Service + "|" + ref.Account
	if _, ok := v.db[key]; !ok {
		return ErrNotFound
	}
	delete(v.db, key)
	delete(v.ref, key)
	return nil
}

// Persistent 实现 Vault：内存库不持久。
func (v *InMemoryVault) Persistent() bool { return false }

func nowUTC() string {
	return fmt.Sprintf("%d", timeNowUnix())
}
