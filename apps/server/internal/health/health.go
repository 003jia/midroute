// Package health 实现模型健康度/拥挤度评分引擎（M2-G1）。
//
// 对齐计划 §6.5 Health & Congestion Engine：
//   - 输入：TTFT P50/P95、总延迟 P50/P95、429/5xx/超时比率、错误分类、样本数。
//   - 输出：status、score(0-100)、confidence、样本数、故障分类、计算时间。
//   - 小样本不触发自动路由变化（confidence=low 且 status 不参与路由）。
//   - 通过滞回（hysteresis）与熔断窗口抑制路由抖动。
//   - 不把“本账号额度用尽”（account_limited）误判为全平台拥堵。
package health

import (
	"errors"
	"sort"
	"time"
)

// FaultClass 故障分类。
type FaultClass string

const (
	FaultNone             FaultClass = "healthy"
	FaultAccountLimited   FaultClass = "account_limited"
	FaultProviderDegraded FaultClass = "provider_degraded"
	FaultModelBusy        FaultClass = "model_busy_inferred"
	FaultNetwork          FaultClass = "network_issue"
)

// Status 健康状态。
type Status string

const (
	StatusHealthy  Status = "healthy"
	StatusDegraded Status = "degraded"
	StatusBusy     Status = "busy"
	StatusLimited  Status = "limited"
	StatusUnknown  Status = "unknown"
)

// Confidence 可信度。
type Confidence string

const (
	ConfidenceLow    Confidence = "low"
	ConfidenceMedium Confidence = "medium"
	ConfidenceHigh   Confidence = "high"
)

// Sample 单次请求观测。
type Sample struct {
	ModelID      string  `json:"model_id"`
	ConnectionID string  `json:"connection_id"`
	TTFTMS       float64 `json:"ttft_ms"`
	LatencyMS    float64 `json:"latency_ms"`
	StatusCode   int     `json:"status_code"`
	ErrorClass   string  `json:"error_class,omitempty"`
	SampledAt    int64   `json:"sampled_at"` // unix ms
}

// Percentile 计算有序样本的百分位数。
func Percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	idx := p / 100 * float64(n-1)
	lo := int(idx)
	hi := lo + 1
	if hi >= n {
		hi = n - 1
	}
	frac := idx - float64(lo)
	return sorted[lo] + (sorted[hi]-sorted[lo])*frac
}

// Metrics 窗口聚合指标。
type Metrics struct {
	SampleCount int
	TTFTP50     float64
	TTFTP95     float64
	LatencyP50  float64
	LatencyP95  float64
	Rate429     float64 // 0..1
	Rate5xx     float64 // 0..1
	RateTimeout float64 // 0..1
	RateNetwork float64 // 0..1
}

// ComputeMetrics 从样本计算窗口指标。
func ComputeMetrics(samples []Sample) Metrics {
	m := Metrics{SampleCount: len(samples)}
	if len(samples) == 0 {
		return m
	}
	var ttft, lat []float64
	for _, s := range samples {
		ttft = append(ttft, s.TTFTMS)
		lat = append(lat, s.LatencyMS)
		switch {
		case s.StatusCode == 429:
			m.Rate429++
		case s.StatusCode >= 500 && s.StatusCode < 600:
			m.Rate5xx++
		case s.ErrorClass == "timeout":
			m.RateTimeout++
		case s.ErrorClass == "network":
			m.RateNetwork++
		}
	}
	sort.Float64s(ttft)
	sort.Float64s(lat)
	m.TTFTP50 = Percentile(ttft, 50)
	m.TTFTP95 = Percentile(ttft, 95)
	m.LatencyP50 = Percentile(lat, 50)
	m.LatencyP95 = Percentile(lat, 95)
	scale := 1 / float64(len(samples))
	m.Rate429 *= scale
	m.Rate5xx *= scale
	m.RateTimeout *= scale
	m.RateNetwork *= scale
	return m
}

// Score 计算健康得分（0-100）。
func (m Metrics) Score() float64 {
	if m.SampleCount == 0 {
		return 0
	}
	score := 100.0
	// 错误率惩罚
	score -= m.Rate429 * 30
	score -= m.Rate5xx * 45
	score -= m.RateTimeout * 40
	score -= m.RateNetwork * 35
	// 延迟惩罚（基准：TTFT P95 与 Latency P95）
	if m.TTFTP95 > 3000 {
		score -= 10
	}
	if m.TTFTP95 > 10000 {
		score -= 10
	}
	if m.LatencyP95 > 20000 {
		score -= 10
	}
	if score < 0 {
		score = 0
	}
	return score
}

// Classify 结合评分、错误比率与本账号额度信号进行故障分类。
// accountLimited 由外部传入（例如剩余额度/配额头或 usage API 判定），
// 避免把额度用尽误判为平台拥堵。
func (m Metrics) Classify(accountLimited bool) FaultClass {
	if m.SampleCount == 0 {
		return FaultNone
	}
	if accountLimited {
		return FaultAccountLimited
	}
	// 网络层错误优先
	if m.RateNetwork > 0.3 || m.RateTimeout > 0.5 {
		return FaultNetwork
	}
	// 多凭据、多地区或大范围 5xx 视为 provider 降级
	if m.Rate5xx > 0.4 {
		return FaultProviderDegraded
	}
	// 429 高但 5xx 低：模型/端点繁忙
	if m.Rate429 > 0.3 {
		return FaultModelBusy
	}
	// 延迟显著偏高但无明显错误：推断模型繁忙
	if m.TTFTP95 > 10000 || m.TTFTP50 > 5000 {
		return FaultModelBusy
	}
	if m.Score() < 60 {
		return FaultProviderDegraded
	}
	return FaultNone
}

// ToStatus 将故障分类映射为对外状态。
func (f FaultClass) ToStatus() Status {
	switch f {
	case FaultAccountLimited:
		return StatusLimited
	case FaultProviderDegraded, FaultNetwork:
		return StatusDegraded
	case FaultModelBusy:
		return StatusBusy
	default:
		return StatusHealthy
	}
}

// ConfidenceOf 根据样本量给出可信度。
func ConfidenceOf(n int) Confidence {
	switch {
	case n >= 100:
		return ConfidenceHigh
	case n >= 20:
		return ConfidenceMedium
	default:
		return ConfidenceLow
	}
}

// Result 一次评分结果。
type Result struct {
	ModelID      string     `json:"model_id"`
	ConnectionID string     `json:"connection_id,omitempty"`
	Status       Status     `json:"status"`
	Score        float64    `json:"score"`
	Confidence   Confidence `json:"confidence"`
	Fault        FaultClass `json:"fault_class"`
	SampleCount  int        `json:"sample_count"`
	Metrics      Metrics    `json:"metrics"`
	ComputedAt   int64      `json:"computed_at"`
	Stable       bool       `json:"stable"` // 是否达到滞回稳定，稳定后才允许自动路由变化
}

// Evaluate 对窗口样本做一次评分。
func Evaluate(samples []Sample, accountLimited bool) Result {
	m := ComputeMetrics(samples)
	fault := m.Classify(accountLimited)
	r := Result{
		Status:      fault.ToStatus(),
		Score:       m.Score(),
		Confidence:  ConfidenceOf(m.SampleCount),
		Fault:       fault,
		SampleCount: m.SampleCount,
		Metrics:     m,
		ComputedAt:  time.Now().UnixMilli(),
		Stable:      m.SampleCount >= minStableSamples,
	}
	if len(samples) > 0 {
		r.ModelID = samples[0].ModelID
		r.ConnectionID = samples[0].ConnectionID
	}
	return r
}

// Engine 带滞回与熔断窗口的评分引擎，避免路由抖动。
type Engine struct {
	// Thresholds 覆盖默认阈值（可用 defaultThresholds() 初始化）。
	Thresholds Thresholds
	// DowngradeSustained 需要连续多少个窗口低于阈值才允许降级。
	DowngradeSustained int
	// RecoverSustained 需要连续多少个窗口恢复才允许回到健康。
	RecoverSustained int
	// MinSamples 不足此样本数不参与自动路由（计划 FR/§6.5：样本不足时不自动切换）。
	MinSamples int
	nowUnixMS  func() int64
	state      map[string]*windowState
}

type windowState struct {
	downgradeCount int
	recoverCount   int
	currentStatus  Status
}

// Thresholds 评分阈值。
type Thresholds struct {
	BusyScore     float64 // 低于此视为繁忙
	DegradedScore float64 // 低于此视为降级
}

// DefaultThresholds 返回默认阈值。
func DefaultThresholds() Thresholds {
	return Thresholds{BusyScore: 70, DegradedScore: 40}
}

const minStableSamples = 20

// NewEngine 创建带默认阈值的引擎。
func NewEngine() *Engine {
	return &Engine{
		Thresholds:         DefaultThresholds(),
		DowngradeSustained: 2,
		RecoverSustained:   2,
		MinSamples:         minStableSamples,
		nowUnixMS:          func() int64 { return time.Now().UnixMilli() },
		state:              map[string]*windowState{},
	}
}

// SetClock 注入时钟（测试用）。
func (e *Engine) SetClock(fn func() int64) { e.nowUnixMS = fn }

var errNoSamples = errors.New("health: no samples")

// Observe 输入一个新窗口的样本，输出带滞回的状态。
func (e *Engine) Observe(samples []Sample, accountLimited bool) (Result, error) {
	if len(samples) == 0 {
		return Result{}, errNoSamples
	}
	r := Evaluate(samples, accountLimited)
	key := r.ModelID
	if r.ConnectionID != "" {
		key = r.ConnectionID + "|" + r.ModelID
	}
	st := e.state[key]
	if st == nil {
		st = &windowState{currentStatus: StatusHealthy}
		e.state[key] = st
	}
	// 样本不足：不改变路由状态
	if r.SampleCount < e.MinSamples {
		r.Stable = false
		r.Status = StatusUnknown
		r.ComputedAt = e.nowUnixMS()
		return r, nil
	}
	desired := r.Status
	if desired == st.currentStatus {
		st.downgradeCount = 0
		st.recoverCount = 0
	} else if worse(desired, st.currentStatus) {
		st.downgradeCount++
		st.recoverCount = 0
		if st.downgradeCount < e.DowngradeSustained {
			r.Status = st.currentStatus
			r.Stable = false
		} else {
			st.currentStatus = desired
			st.downgradeCount = 0
			r.Stable = true
		}
	} else { // 恢复
		st.recoverCount++
		st.downgradeCount = 0
		if st.recoverCount < e.RecoverSustained {
			r.Status = st.currentStatus
			r.Stable = false
		} else {
			st.currentStatus = desired
			st.recoverCount = 0
			r.Stable = true
		}
	}
	r.ComputedAt = e.nowUnixMS()
	return r, nil
}

var statusRank = map[Status]int{
	StatusHealthy:  0,
	StatusBusy:     1,
	StatusDegraded: 2,
	StatusLimited:  3,
}

func worse(a, b Status) bool {
	return statusRank[a] > statusRank[b]
}
