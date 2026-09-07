package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func fixedWindow() Window {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return Window{Start: start, End: start.Add(24 * time.Hour)}
}

func TestOpenAIMatrixNoAdminKey(t *testing.T) {
	a := NewOpenAIAdapter(OpenAIOptions{})
	res, err := a.CapabilityMatrix(context.Background(), "sk-abc123")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Supported {
			t.Fatalf("capability %s should be unsupported without admin key", r.Capability)
		}
		if r.Reason == "" {
			t.Fatalf("unsupported capability %s must carry reason", r.Capability)
		}
	}
}

func TestOpenAIFetchUsageUnavailableWithoutAdmin(t *testing.T) {
	a := NewOpenAIAdapter(OpenAIOptions{})
	u, err := a.FetchUsage(context.Background(), "sk-abc123", fixedWindow())
	if err != nil {
		t.Fatal(err)
	}
	if u.Confidence != ConfidenceUnavailable {
		t.Fatalf("confidence=%v want unavailable", u.Confidence)
	}
	if u.InputTokens != 0 {
		t.Fatalf("must not fabricate numbers")
	}
}

func TestOpenAIFetchUsageDryRun(t *testing.T) {
	a := NewOpenAIAdapter(OpenAIOptions{})
	u, err := a.FetchUsage(context.Background(), "dry-run", fixedWindow())
	if err != nil {
		t.Fatal(err)
	}
	if u.Confidence != ConfidenceEstimated {
		t.Fatalf("confidence=%v want estimated", u.Confidence)
	}
}

func TestOpenAIFetchUsageParsesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-admin-test123456789" {
			t.Errorf("bad auth header")
		}
		body := `{"data":[{"start_time":1,"results":[{"input_tokens":100,"output_tokens":50,"input_cached_tokens":10,"num_requests":3}]}]}`
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(body))
	}))
	defer srv.Close()
	a := NewOpenAIAdapter(OpenAIOptions{BaseURL: srv.URL})
	u, err := a.FetchUsage(context.Background(), "sk-admin-test123456789", fixedWindow())
	if err != nil {
		t.Fatal(err)
	}
	if u.Confidence != ConfidenceExact || u.Source != SourceOfficial {
		t.Fatalf("conf=%v src=%v", u.Confidence, u.Source)
	}
	if u.InputTokens != 100 || u.OutputTokens != 50 || u.CacheTokens != 10 || u.Requests != 3 {
		t.Fatalf("usage mismatch: %+v", u)
	}
}

func TestAnthropicFetchUsageParsesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-ant-admin-test1234567890" {
			t.Errorf("bad x-api-key")
		}
		body := `{"total":{"input_tokens":200,"output_tokens":80,"cache_read_tokens":5,"cache_write_tokens":3}}`
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(body))
	}))
	defer srv.Close()
	a := NewAnthropicAdapter(AnthropicOptions{BaseURL: srv.URL})
	u, err := a.FetchUsage(context.Background(), "sk-ant-admin-test1234567890", fixedWindow())
	if err != nil {
		t.Fatal(err)
	}
	if u.InputTokens != 200 || u.OutputTokens != 80 || u.CacheTokens != 8 {
		t.Fatalf("usage mismatch: %+v", u)
	}
	if u.Confidence != ConfidenceExact {
		t.Fatalf("confidence=%v", u.Confidence)
	}
}

func TestGeminiMatrixUnsupported(t *testing.T) {
	a := NewGeminiAdapter()
	res, err := a.CapabilityMatrix(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Supported {
			t.Fatalf("gemini capability should be unsupported without GCP creds")
		}
		if r.Reason == "" {
			t.Fatalf("must carry reason")
		}
	}
}

func TestReconcile(t *testing.T) {
	official := &Usage{Provider: ProviderOpenAI, Source: SourceOfficial, Confidence: ConfidenceExact,
		InputTokens: 1000, OutputTokens: 500, CacheTokens: 100, Requests: 10}
	observed := &Usage{Provider: ProviderOpenAI, Source: SourceObserved, Confidence: ConfidenceExact,
		InputTokens: 980, OutputTokens: 500, CacheTokens: 100, Requests: 9}
	r := Reconcile(ProviderOpenAI, fixedWindow(), official, observed)
	if len(r.Entries) != 4 {
		t.Fatalf("entries=%d", len(r.Entries))
	}
	found := false
	for _, e := range r.Entries {
		if e.Metric == "input_tokens" && e.Official == 1000 && e.Observed == 980 && e.Diff == 20 {
			found = true
		}
	}
	if !found {
		t.Fatalf("input_tokens entry missing/wrong: %+v", r.Entries)
	}
}

func TestReconcileOfficialUnavailable(t *testing.T) {
	official := &Usage{Provider: ProviderOpenAI, Source: SourceOfficial, Confidence: ConfidenceUnavailable, Note: "no perm"}
	observed := &Usage{Provider: ProviderOpenAI, Source: SourceObserved, Confidence: ConfidenceExact, InputTokens: 500}
	r := Reconcile(ProviderOpenAI, fixedWindow(), official, observed)
	for _, e := range r.Entries {
		if e.Interpretation != "官方不可用，以本地观测为准" {
			t.Fatalf("interpretation=%q", e.Interpretation)
		}
	}
}
