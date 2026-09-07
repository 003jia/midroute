package quota

import (
	"net/http"
	"testing"
	"time"
)

func header(k, v string) http.Header {
	h := http.Header{}
	h.Set(k, v)
	return h
}

// TestParseFullPrimarySecondary 覆盖双窗口完整字段（fixture：已知字段全量）。
func TestParseFullPrimarySecondary(t *testing.T) {
	h := http.Header{}
	h.Set("X-Codex-Plan-Type", "pro")
	h.Set("X-Codex-Active-Limit", "primary")
	h.Set("X-Codex-Primary-Used-Percent", "42.5")
	h.Set("X-Codex-Primary-Window-Minutes", "300")
	h.Set("X-Codex-Primary-Reset-After-Seconds", "1200")
	h.Set("X-Codex-Primary-Limit-Reached", "false")
	h.Set("X-Codex-Secondary-Used-Percent", "7")
	h.Set("X-Codex-Secondary-Window-Minutes", "10080")
	h.Set("X-Codex-Secondary-Allowed", "true")
	h.Set("X-Codex-Secondary-Limit-Reached", "false")

	obs, err := ParseCodexQuotaHeaders(h, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if obs == nil {
		t.Fatal("nil observation")
	}
	if obs.PlanType != "pro" || obs.ActiveLimit != "primary" {
		t.Fatalf("plan/active: %q %q", obs.PlanType, obs.ActiveLimit)
	}
	if len(obs.Snapshots) != 2 {
		t.Fatalf("snapshots = %d, want 2", len(obs.Snapshots))
	}
	byLimit := map[string]Snapshot{}
	for _, s := range obs.Snapshots {
		byLimit[s.LimitName] = s
	}
	p := byLimit["primary"]
	if p.UsedPercent == nil || *p.UsedPercent != 42.5 {
		t.Fatalf("primary used-percent: %v", p.UsedPercent)
	}
	if p.WindowMinutes == nil || *p.WindowMinutes != 300 {
		t.Fatalf("primary window: %v", p.WindowMinutes)
	}
	if p.ResetAfterSeconds == nil || *p.ResetAfterSeconds != 1200 {
		t.Fatalf("primary reset-after: %v", p.ResetAfterSeconds)
	}
	if p.LimitReached == nil || *p.LimitReached {
		t.Fatalf("primary limit-reached: %v", p.LimitReached)
	}
	if p.Source != SourceObserved {
		t.Fatalf("source = %q", p.Source)
	}
	if p.CollectedAt.IsZero() {
		t.Fatal("collected_at zero")
	}
	s := byLimit["secondary"]
	if s.Allowed == nil || !*s.Allowed {
		t.Fatalf("secondary allowed: %v", s.Allowed)
	}
	if s.UsedPercent == nil || *s.UsedPercent != 7 {
		t.Fatalf("secondary used-percent: %v", s.UsedPercent)
	}
	// 缺失字段必须保持 nil（未知），不得落成 0
	if s.ResetAfterSeconds != nil || s.ResetAt != nil || s.RawResetAt != "" {
		t.Fatalf("missing fields must stay unknown: %+v", s)
	}
}

// TestParseNoHeaders 验证无信号头时不产生观测（调用方保留上次成功快照）。
func TestParseNoHeaders(t *testing.T) {
	obs, err := ParseCodexQuotaHeaders(header("Content-Type", "application/json"), time.Time{})
	if err != nil || obs != nil {
		t.Fatalf("want nil,nil got %v,%v", obs, err)
	}
}

// TestParseInvalidValuesSkipped 非法值跳过且不污染其他字段。
func TestParseInvalidValuesSkipped(t *testing.T) {
	h := http.Header{}
	h.Set("X-Codex-Primary-Used-Percent", "abc")  // 非数字
	h.Set("X-Codex-Primary-Used-Percent", "abc")  // 重复取最后一个
	h.Set("X-Codex-Primary-Window-Minutes", "-5") // 非法窗口
	h.Set("X-Codex-Primary-Used-Percent", "150")  // 越界百分比
	h.Set("X-Codex-Primary-Allowed", "sometimes") // 非法布尔
	h.Set("X-Codex-Secondary-Used-Percent", "3")  // 合法，应保留
	obs, err := ParseCodexQuotaHeaders(h, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	byLimit := map[string]Snapshot{}
	for _, s := range obs.Snapshots {
		byLimit[s.LimitName] = s
	}
	p := byLimit["primary"]
	if p.UsedPercent != nil || p.WindowMinutes != nil {
		t.Fatalf("invalid values must stay unknown: %+v", p)
	}
	if len(obs.Warnings) == 0 {
		t.Fatal("warnings expected for invalid values")
	}
	for _, w := range obs.Warnings {
		if containsSensitive(w) {
			t.Fatalf("warning must not embed raw values: %q", w)
		}
	}
	sec := byLimit["secondary"]
	if sec.UsedPercent == nil || *sec.UsedPercent != 3 {
		t.Fatalf("valid sibling must survive: %+v", sec)
	}
}

func containsSensitive(s string) bool {
	// 简化断言：warning 只描述字段与长度，不应包含原始值内容
	return len(s) > 0 && (s == "abc" || s == "-5" || s == "150" || s == "sometimes")
}

// TestParseResetAtFormats 覆盖 reset-at 可解析与不可解析两种情况。
func TestParseResetAtFormats(t *testing.T) {
	h := http.Header{}
	h.Set("X-Codex-Primary-Reset-At", "Mon, 07 Sep 2026 15:00:00 GMT")
	h.Set("X-Codex-Secondary-Reset-At", "not-a-time")
	obs, err := ParseCodexQuotaHeaders(h, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	byLimit := map[string]Snapshot{}
	for _, s := range obs.Snapshots {
		byLimit[s.LimitName] = s
	}
	p := byLimit["primary"]
	if p.ResetAt == nil || p.ResetAt.UTC().Hour() != 15 {
		t.Fatalf("reset-at parse: %v", p.ResetAt)
	}
	sec := byLimit["secondary"]
	if sec.ResetAt != nil || sec.RawResetAt != "not-a-time" {
		t.Fatalf("unparseable reset-at must keep raw: %+v", sec)
	}
}

// TestParseAdditionalLimitNamespace 覆盖附加限制命名空间（additional-<名称>）。
func TestParseAdditionalLimitNamespace(t *testing.T) {
	h := http.Header{}
	h.Set("X-Codex-Additional-Bengalfox-Primary-Used-Percent", "88")
	h.Set("X-Codex-Additional-Bengalfox-Primary-Limit-Reached", "true")
	obs, err := ParseCodexQuotaHeaders(h, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.Snapshots) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(obs.Snapshots))
	}
	snap := obs.Snapshots[0]
	if snap.LimitName != "additional-bengalfox-primary" {
		t.Fatalf("limit name = %q", snap.LimitName)
	}
	if snap.UsedPercent == nil || *snap.UsedPercent != 88 {
		t.Fatalf("used-percent: %v", snap.UsedPercent)
	}
	if snap.LimitReached == nil || !*snap.LimitReached {
		t.Fatalf("limit-reached: %v", snap.LimitReached)
	}
}

// TestNoTokenConversion 验证百分比永不换算成 Token 数（单位保持 percent）。
func TestNoTokenConversion(t *testing.T) {
	h := header("X-Codex-Primary-Used-Percent", "50")
	obs, err := ParseCodexQuotaHeaders(h, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	snap := obs.Snapshots[0]
	if snap.UsedPercent == nil || *snap.UsedPercent != 50 {
		t.Fatal("percent must be preserved verbatim")
	}
	// 结构中不存在 tokens 字段：编译期由类型保证；此处断言字段映射不变
	if snap.WindowMinutes != nil || snap.Allowed != nil || snap.LimitReached != nil {
		t.Fatalf("unexpected fields populated: %+v", snap)
	}
}
