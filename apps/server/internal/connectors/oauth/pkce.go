package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// pkce 持有一对授权码交换校验码（RFC 7636，S256 方法）。
// 流程参考上游 internal/auth/codex/pkce.go，按 Go 标准库独立改写。
type pkce struct {
	// verifier 高熵随机串（96 字节，base64url 无填充，128 字符）。
	verifier string
	// challenge = BASE64URL(SHA256(verifier))，随授权 URL 发出。
	challenge string
}

// newPKCE 生成一对新的 PKCE 校验码。
func newPKCE() (*pkce, error) {
	raw := make([]byte, 96)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("oauth: read random bytes: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return &pkce{
		verifier:  verifier,
		challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// newState 生成 256 位随机 state（base64url，无填充）。
func newState() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("oauth: read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
