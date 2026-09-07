package credentials

import (
	"regexp"
	"strings"
)

// 常见凭据形态的脱敏模式。与 build/ci/secret-scan.sh 保持一致。
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9-]{16,}`),       // OpenAI / 通用
	regexp.MustCompile(`sk-ant-[A-Za-z0-9-]{16,}`),   // Anthropic
	regexp.MustCompile(`AIza[A-Za-z0-9_-]{16,}`),     // Gemini
	regexp.MustCompile(`Bearer [A-Za-z0-9._-]{20,}`), // OAuth token
	regexp.MustCompile(`\bkey[=:][^&\s"']{16,}`),     // 任意 key 赋值
	regexp.MustCompile(`cmp_admin_[A-Za-z0-9]{8,}`),  // CPA 管理台
}

// Redactor 将文本中的疑似凭据替换为脱敏占位符。
// 保留前缀与长度量级，便于排查但不可逆。
type Redactor struct {
	repl func([]byte) []byte
}

// NewRedactor 创建默认脱敏器。
func NewRedactor() *Redactor {
	return &Redactor{repl: redactMatch}
}

func redactMatch(b []byte) []byte {
	s := string(b)
	// 保留前 4 字符，其余替换为 *
	keep := 4
	if len(s) <= keep {
		return []byte(strings.Repeat("*", len(s)))
	}
	return []byte(s[:keep] + strings.Repeat("*", len(s)-keep))
}

// Redact 脱敏单个字符串。
func (r *Redactor) Redact(s string) string {
	out := s
	for _, re := range secretPatterns {
		out = re.ReplaceAllStringFunc(out, func(m string) string {
			return string(r.repl([]byte(m)))
		})
	}
	return out
}

// RedactHeaderValue 脱敏单个 header 值（例如 Authorization: Bearer xxx）。
func (r *Redactor) RedactHeaderValue(v string) string {
	return r.Redact(v)
}

// RedactQuery 脱敏 URL query 中的敏感参数值。
func (r *Redactor) RedactQuery(query string) string {
	parts := strings.Split(query, "&")
	for i, p := range parts {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.ToLower(kv[0])
		if strings.Contains(key, "token") || strings.Contains(key, "key") || strings.Contains(key, "auth") || strings.Contains(key, "secret") {
			parts[i] = kv[0] + "=" + strings.Repeat("*", len(kv[1]))
		}
	}
	return strings.Join(parts, "&")
}
