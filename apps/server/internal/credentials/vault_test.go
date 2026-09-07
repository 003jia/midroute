package credentials

import (
	"os/exec"
	"strings"
	"testing"
)

func TestFingerprintStableAndUnique(t *testing.T) {
	a := Fingerprint([]byte("sk-test-aaaa"))
	b := Fingerprint([]byte("sk-test-aaaa"))
	c := Fingerprint([]byte("sk-test-bbbb"))
	if a != b {
		t.Fatalf("fingerprint not stable: %s != %s", a, b)
	}
	if a == c {
		t.Fatalf("fingerprint collision")
	}
	if len(ShortFingerprint([]byte("x"))) > 12 {
		t.Fatalf("short fingerprint too long")
	}
}

func TestInMemoryVaultRoundTrip(t *testing.T) {
	v := NewInMemoryVault()
	ref, err := v.Store("openai", "conn-1", []byte("sk-test-secret-123"))
	if err != nil {
		t.Fatal(err)
	}
	if ref.Fingerprint != ShortFingerprint([]byte("sk-test-secret-123")) {
		t.Fatalf("fingerprint mismatch")
	}
	s, err := v.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Zero()
	if string(s.Value) != "sk-test-secret-123" {
		t.Fatalf("round trip mismatch")
	}
	if err := v.Delete(ref); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Get(ref); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSecretZeroClearsMemory(t *testing.T) {
	s := Secret{Value: []byte("sk-test-secret-123")}
	s.Zero()
	if len(s.Value) != 0 {
		t.Fatalf("value not cleared")
	}
}

func TestRedactorHidesSecrets(t *testing.T) {
	r := NewRedactor()
	cases := []string{
		"Authorization: Bearer abcdefghijklmnopqrstuvwxyz123456",
		"key=sk-test-abcdefghijklmnop",
		"x apiKey: AIzaSyABCDEFGHIJKLMNOPQRSTUVWXYZ12345678",
		"ant sk-ant-test-abcdefghijklmnop",
	}
	for _, c := range cases {
		out := r.Redact(c)
		if strings.Contains(out, "abcdefghijklmnopqrstuvwxyz") {
			t.Fatalf("secret leaked in %q -> %q", c, out)
		}
		if strings.Contains(out, "sk-test-abcdefghijklmnop") {
			t.Fatalf("api key leaked in %q -> %q", c, out)
		}
		if !strings.Contains(out, "*") {
			t.Fatalf("no masking applied: %q -> %q", c, out)
		}
	}
}

func TestRedactorLeavesPlainText(t *testing.T) {
	r := NewRedactor()
	in := "model=gpt-4o latency=120ms ok"
	if out := r.Redact(in); out != in {
		t.Fatalf("plain text changed: %q", out)
	}
}

func TestRedactQuery(t *testing.T) {
	r := NewRedactor()
	q := "auth_token=secretvalue&model=gpt-4o&api_key=anothersecret"
	out := r.RedactQuery(q)
	if strings.Contains(out, "secretvalue") || strings.Contains(out, "anothersecret") {
		t.Fatalf("query secret leaked: %q", out)
	}
	if !strings.Contains(out, "model=gpt-4o") {
		t.Fatalf("innocent param damaged: %q", out)
	}
}

func TestKeychainUnsupportedOnNonDarwin(t *testing.T) {
	// 仅验证错误类型路径（通过注入 execCommand 在 linux 上也只会在 New 阶段拒绝）。
	// 这里直接构造并调用 run 以覆盖 KeychainError 包装。
	k := &KeychainVault{service: "test", execCommand: fakeExec}
	_, err := k.run("find-generic-password", "-w")
	if err == nil {
		t.Fatal("expected error")
	}
	ke, ok := err.(*KeychainError)
	if !ok {
		t.Fatalf("expected *KeychainError, got %T", err)
	}
	if !strings.Contains(ke.Error(), "keychain") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func fakeExec(_ string, _ ...string) *exec.Cmd {
	return exec.Command("sh", "-c", "echo boom >&2; exit 1")
}
