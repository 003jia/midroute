package errs

import (
	"errors"
	"net/http"
	"testing"
)

func TestCodeHTTPStatus(t *testing.T) {
	cases := map[Code]int{
		CodeInvalidRequest:   http.StatusBadRequest,
		CodeUnauthorized:     http.StatusUnauthorized,
		CodeForbidden:        http.StatusForbidden,
		CodeNotFound:         http.StatusNotFound,
		CodeRateLimited:      http.StatusTooManyRequests,
		CodeQuotaExhausted:   http.StatusTooManyRequests,
		CodeUpstreamTimeout:  http.StatusGatewayTimeout,
		CodeUpstreamError:    http.StatusBadGateway,
		CodeModelUnavailable: http.StatusBadGateway,
		CodeInternal:         http.StatusInternalServerError,
	}
	for code, want := range cases {
		if got := New(code, "x").HTTPStatus(); got != want {
			t.Fatalf("%s: got %d want %d", code, got, want)
		}
	}
}

func TestWrapAndAs(t *testing.T) {
	base := errors.New("boom")
	e := Wrap(CodeUpstreamError, "上游错误", base)
	if !errors.Is(e, base) {
		t.Fatal("unwrap failed")
	}
	got, ok := As(e)
	if !ok || got.Code != CodeUpstreamError {
		t.Fatalf("As failed: %+v", got)
	}
	if got.HTTPStatus() != http.StatusBadGateway {
		t.Fatal("status wrong")
	}
}

func TestFromWrapsPlainError(t *testing.T) {
	e := From(errors.New("plain"))
	if e.Code != CodeInternal {
		t.Fatalf("code=%s", e.Code)
	}
}

func TestRetryable(t *testing.T) {
	e := Retryable(New(CodeRateLimited, "限流"))
	if !e.Retryable {
		t.Fatal("retryable flag not set")
	}
	// 鉴权错误不可重试
	if New(CodeUnauthorized, "鉴权失败").Retryable {
		t.Fatal("auth must not be retryable")
	}
}
