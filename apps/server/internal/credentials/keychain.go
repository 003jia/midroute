package credentials

import (
	"bytes"
	"os/exec"
	"runtime"
	"strings"
)

// KeychainVault 使用 macOS 系统 Keychain（security CLI）存取凭据。
// 仅在 darwin 上可用；其他平台返回 UnsupportedPlatformError。
type KeychainVault struct {
	service string
	// execLookPath 可注入，便于测试。
	execLookPath func(string) (string, error)
	execCommand  func(string, ...string) *exec.Cmd
}

// UnsupportedPlatformError 表示当前平台不支持系统 Keychain。
type UnsupportedPlatformError struct{ OS string }

func (e *UnsupportedPlatformError) Error() string {
	return "system keychain unsupported on " + e.OS
}

// NewKeychainVault 创建 macOS Keychain 凭据库。
func NewKeychainVault(service string) (*KeychainVault, error) {
	if runtime.GOOS != "darwin" {
		return nil, &UnsupportedPlatformError{OS: runtime.GOOS}
	}
	if _, err := exec.LookPath("security"); err != nil {
		return nil, err
	}
	return &KeychainVault{
		service:      service,
		execLookPath: exec.LookPath,
		execCommand:  exec.Command,
	}, nil
}

func (k *KeychainVault) run(args ...string) (string, error) {
	cmd := k.execCommand("security", args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", &KeychainError{Op: args[0], Msg: msg}
	}
	return strings.TrimSpace(out.String()), nil
}

// KeychainError 表示 security CLI 调用失败。
type KeychainError struct {
	Op  string
	Msg string
}

func (e *KeychainError) Error() string {
	return "keychain " + e.Op + ": " + e.Msg
}

// Store 实现 Vault（add-generic-password）。
func (k *KeychainVault) Store(service, account string, secret []byte) (SecretRef, error) {
	ref := SecretRef{
		VaultProvider: "macos-keychain",
		Service:       k.service + "-" + service,
		Account:       account,
		Fingerprint:   ShortFingerprint(secret),
		CreatedAt:     nowUTC(),
	}
	_, err := k.run("add-generic-password", "-s", ref.Service, "-a", account, "-w", string(secret), "-U")
	if err != nil {
		return SecretRef{}, err
	}
	return ref, nil
}

// Get 实现 Vault（find-generic-password -w）。
func (k *KeychainVault) Get(ref SecretRef) (Secret, error) {
	out, err := k.run("find-generic-password", "-s", ref.Service, "-a", ref.Account, "-w")
	if err != nil {
		if strings.Contains(err.Error(), "could not be found") {
			return Secret{}, ErrNotFound
		}
		return Secret{}, err
	}
	return Secret{Value: []byte(out)}, nil
}

// Persistent 实现 Vault：Keychain 跨重启持久保存。
func (k *KeychainVault) Persistent() bool { return true }

// Delete 实现 Vault（delete-generic-password）。
func (k *KeychainVault) Delete(ref SecretRef) error {
	_, err := k.run("delete-generic-password", "-s", ref.Service, "-a", ref.Account)
	if err != nil {
		if strings.Contains(err.Error(), "could not be found") {
			return ErrNotFound
		}
		return err
	}
	return nil
}
