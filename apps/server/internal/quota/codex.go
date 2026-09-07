package quota

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// headerPrefix 是 Codex 额度信号头的统一前缀。
const headerPrefix = "x-codex-"

// windowSuffixes 是窗口命名空间下的已知字段后缀（上游 quota_signals.go 归纳）。
// 其余 x-codex-* 头（如 credits）不构成窗口，暂不入快照。
var windowSuffixes = []string{
	"-used-percent",
	"-window-minutes",
	"-reset-after-seconds",
	"-reset-at",
	"-allowed",
	"-limit-reached",
}

// ParseCodexQuotaHeaders 从推理响应头解析 Codex 额度观测。
// 无任何 x-codex-* 头时返回 nil observation（调用方保留上次成功快照）。
// 单个字段非法时跳过该字段并记录 warning，不影响其余字段。
func ParseCodexQuotaHeaders(h http.Header, observedAt time.Time) (*Observation, error) {
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	obs := &Observation{}
	byLimit := map[string]*Snapshot{}
	found := false

	for name, values := range h {
		if len(values) == 0 {
			continue
		}
		lower := strings.ToLower(strings.TrimSpace(name))
		if !strings.HasPrefix(lower, headerPrefix) {
			continue
		}
		found = true
		rest := lower[len(headerPrefix):]
		value := strings.TrimSpace(values[len(values)-1])
		switch rest {
		case "plan-type":
			obs.PlanType = value
			continue
		case "active-limit":
			obs.ActiveLimit = value
			continue
		}
		limit, suffix, ok := splitWindowField(rest)
		if !ok {
			// credits 等非窗口字段：识别但暂不入快照
			continue
		}
		snap := byLimit[limit]
		if snap == nil {
			snap = &Snapshot{LimitName: limit, Source: SourceObserved, CollectedAt: observedAt}
			byLimit[limit] = snap
		}
		if warn := applyField(snap, suffix, value); warn != "" {
			obs.Warnings = append(obs.Warnings, warn)
		}
	}
	if !found {
		return nil, nil
	}

	for _, s := range byLimit {
		obs.Snapshots = append(obs.Snapshots, *s)
	}
	// 稳定输出顺序，便于测试与展示
	sort.Slice(obs.Snapshots, func(i, j int) bool { return obs.Snapshots[i].LimitName < obs.Snapshots[j].LimitName })
	return obs, nil
}

// splitWindowField 把 "primary-used-percent" 拆成窗口名与字段名。
func splitWindowField(rest string) (limit, suffix string, ok bool) {
	for _, suf := range windowSuffixes {
		if strings.HasSuffix(rest, suf) {
			return strings.TrimSuffix(rest, suf), suf, true
		}
	}
	return "", "", false
}

// applyField 把单个字段写入窗口快照，非法值跳过并返回 warning。
func applyField(s *Snapshot, suffix, value string) string {
	switch suffix {
	case "-used-percent":
		v, err := strconv.ParseFloat(value, 64)
		if err != nil || v < 0 || v > 100 {
			return skipMsg(s.LimitName+suffix, value)
		}
		s.UsedPercent = &v
	case "-window-minutes":
		v, err := strconv.Atoi(value)
		if err != nil || v <= 0 {
			return skipMsg(s.LimitName+suffix, value)
		}
		s.WindowMinutes = &v
	case "-reset-after-seconds":
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil || v < 0 {
			return skipMsg(s.LimitName+suffix, value)
		}
		s.ResetAfterSeconds = &v
	case "-reset-at":
		if t, err := parseUpstreamTime(value); err == nil {
			s.ResetAt = &t
		} else {
			s.RawResetAt = value
		}
	case "-allowed":
		s.Allowed = parseBoolPtr(value, s.LimitName+suffix)
	case "-limit-reached":
		s.LimitReached = parseBoolPtr(value, s.LimitName+suffix)
	}
	return ""
}

func parseBoolPtr(value, field string) *bool {
	switch strings.ToLower(value) {
	case "true", "1", "yes":
		v := true
		return &v
	case "false", "0", "no":
		v := false
		return &v
	default:
		return nil
	}
}

// parseUpstreamTime 尝试常见时间格式；都不匹配返回错误（由调用方保留原值）。
func parseUpstreamTime(v string) (time.Time, error) {
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC3339} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("quota: unrecognized time %q", v)
}

func skipMsg(field, value string) string {
	return "skipped invalid " + field + " value (length " + strconv.Itoa(len(value)) + ")"
}
