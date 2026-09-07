package billing

import (
	"math"
	"time"
)

// Reconcile 将官方用量与本地观测对账，输出逐项差异。
// 不做盲目求和：official 与 observed 分列展示，差异与解释由调用方决策。
func Reconcile(p Provider, window Window, official, observed *Usage) *Reconciliation {
	r := &Reconciliation{Provider: p, Window: window, Generated: time.Now().Unix()}
	add := func(metric string, off, obs float64, offConf, obsConf Confidence) {
		diff := off - obs
		interp := "一致"
		abs := math.Abs(diff)
		switch {
		case offConf == ConfidenceUnavailable:
			interp = "官方不可用，以本地观测为准"
		case abs > 0 && abs/maxF(off, 1) > 0.1:
			interp = "差异较大，需检查统计窗口/口径差异"
		case abs > 0:
			interp = "小差异（窗口边界/取整）"
		}
		r.Entries = append(r.Entries, DiffEntry{
			Metric: metric, Official: off, Observed: obs, Diff: diff,
			OfficialConf: offConf, ObservedConf: obsConf, Interpretation: interp,
		})
	}
	if official != nil {
		add("input_tokens", float64(official.InputTokens), obsF(observed, func(u *Usage) float64 { return float64(u.InputTokens) }), official.Confidence, obsConf(observed))
		add("output_tokens", float64(official.OutputTokens), obsF(observed, func(u *Usage) float64 { return float64(u.OutputTokens) }), official.Confidence, obsConf(observed))
		add("cache_tokens", float64(official.CacheTokens), obsF(observed, func(u *Usage) float64 { return float64(u.CacheTokens) }), official.Confidence, obsConf(observed))
		add("requests", float64(official.Requests), obsF(observed, func(u *Usage) float64 { return float64(u.Requests) }), official.Confidence, obsConf(observed))
	}
	return r
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func obsConf(u *Usage) Confidence {
	if u == nil {
		return ConfidenceUnavailable
	}
	return u.Confidence
}

func obsF(u *Usage, fn func(*Usage) float64) float64 {
	if u == nil {
		return 0
	}
	return fn(u)
}
