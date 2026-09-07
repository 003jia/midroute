package health

import (
	"math"
	"testing"
)

func sample(ttft, lat float64, code int, ec string) Sample {
	return Sample{ModelID: "m", TTFTMS: ttft, LatencyMS: lat, StatusCode: code, ErrorClass: ec, SampledAt: 1}
}

func TestComputeMetrics(t *testing.T) {
	samples := []Sample{
		sample(100, 500, 200, ""),
		sample(200, 600, 200, ""),
		sample(300, 700, 429, ""),
		sample(400, 800, 500, ""),
		sample(500, 900, 0, "timeout"),
	}
	m := ComputeMetrics(samples)
	if m.SampleCount != 5 {
		t.Fatalf("count=%d", m.SampleCount)
	}
	if m.TTFTP50 != 300 {
		t.Fatalf("ttft p50=%v", m.TTFTP50)
	}
	if math.Abs(m.Rate429-0.2) > 1e-9 || math.Abs(m.Rate5xx-0.2) > 1e-9 || math.Abs(m.RateTimeout-0.2) > 1e-9 {
		t.Fatalf("rates wrong: %+v", m)
	}
}

func TestClassifyAccountLimitedNotBusy(t *testing.T) {
	samples := []Sample{sample(200, 500, 429, ""), sample(200, 500, 429, "")}
	m := ComputeMetrics(samples)
	if f := m.Classify(true); f != FaultAccountLimited {
		t.Fatalf("want account_limited, got %v", f)
	}
	if f := m.Classify(false); f != FaultModelBusy {
		t.Fatalf("want model_busy_inferred, got %v", f)
	}
}

func TestClassifyNetworkAndProvider(t *testing.T) {
	netSamples := []Sample{sample(0, 0, 0, "network"), sample(0, 0, 0, "network")}
	if f := ComputeMetrics(netSamples).Classify(false); f != FaultNetwork {
		t.Fatalf("want network_issue, got %v", f)
	}
	fivexx := []Sample{sample(100, 500, 500, ""), sample(100, 500, 503, "")}
	if f := ComputeMetrics(fivexx).Classify(false); f != FaultProviderDegraded {
		t.Fatalf("want provider_degraded, got %v", f)
	}
}

func TestConfidenceBySampleCount(t *testing.T) {
	if ConfidenceOf(5) != ConfidenceLow {
		t.Fatal("want low")
	}
	if ConfidenceOf(50) != ConfidenceMedium {
		t.Fatal("want medium")
	}
	if ConfidenceOf(200) != ConfidenceHigh {
		t.Fatal("want high")
	}
}

func TestEngineInsufficientSamplesNoChange(t *testing.T) {
	e := NewEngine()
	r, err := e.Observe([]Sample{sample(200, 500, 429, "")}, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Stable {
		t.Fatal("insufficient samples must not be stable")
	}
	if r.Status != StatusUnknown {
		t.Fatalf("insufficient samples status=%v", r.Status)
	}
}

func TestEngineHysteresisDowngrade(t *testing.T) {
	e := NewEngine()
	e.MinSamples = 1
	e.DowngradeSustained = 2
	busy := []Sample{sample(30000, 5000, 429, ""), sample(30000, 5000, 429, "")}
	// 第一次：不降级
	r1, _ := e.Observe(busy, false)
	if r1.Stable {
		t.Fatal("first window must not be stable (hysteresis)")
	}
	if r1.Status != StatusHealthy {
		t.Fatalf("first window status=%v, want healthy", r1.Status)
	}
	// 第二次：降级生效
	r2, _ := e.Observe(busy, false)
	if !r2.Stable || r2.Status != StatusBusy {
		t.Fatalf("second window status=%v stable=%v, want busy", r2.Status, r2.Stable)
	}
}

func TestEngineRecoveryRequiresSustained(t *testing.T) {
	e := NewEngine()
	e.MinSamples = 1
	e.DowngradeSustained = 1
	e.RecoverSustained = 2
	busy := []Sample{sample(30000, 5000, 429, "")}
	ok := []Sample{sample(100, 500, 200, "")}
	e.Observe(busy, false) // -> busy stable
	r1, _ := e.Observe(ok, false)
	if r1.Status != StatusBusy || r1.Stable {
		t.Fatalf("first recovery window status=%v stable=%v", r1.Status, r1.Stable)
	}
	r2, _ := e.Observe(ok, false)
	if r2.Status != StatusHealthy || !r2.Stable {
		t.Fatalf("second recovery window status=%v stable=%v", r2.Status, r2.Stable)
	}
}

func TestEvaluateNoSamplesUnknownScore(t *testing.T) {
	r := Evaluate(nil, false)
	if r.Score != 0 {
		t.Fatalf("score=%v", r.Score)
	}
}
