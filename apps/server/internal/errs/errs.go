// Package errs 定义稳定的错误码与 HTTP 语义（对齐 PRD FR-8/§9 与 US-006 错误映射）。
package errs

import (
	"errors"
	"net/http"
)

// Code 是稳定的错误码。
type Code string

const (
	CodeInvalidRequest   Code = "invalid_request"
	CodeUnauthorized     Code = "unauthorized"
	CodeForbidden        Code = "forbidden"
	CodeNotFound         Code = "not_found"
	CodeRateLimited      Code = "rate_limited"
	CodeUpstreamError    Code = "upstream_error"
	CodeUpstreamTimeout  Code = "upstream_timeout"
	CodeUpstreamAuth     Code = "upstream_auth"
	CodeQuotaExhausted   Code = "quota_exhausted"
	CodeModelUnavailable Code = "model_unavailable"
	CodeInternal         Code = "internal_error"
	CodeUnsupported      Code = "unsupported"
	CodeConflict         Code = "conflict"
)

// Error 携带稳定错误码的业务错误。
type Error struct {
	Code      Code   `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
	Err       error  `json:"-"`
}

func (e *Error) Error() string {
	if e.Err != nil {
		return string(e.Code) + ": " + e.Message + " (" + e.Err.Error() + ")"
	}
	return string(e.Code) + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// HTTPStatus 返回错误对应的 HTTP 状态码。
func (e *Error) HTTPStatus() int {
	switch e.Code {
	case CodeInvalidRequest, CodeUnsupported:
		return http.StatusBadRequest
	case CodeConflict:
		return http.StatusConflict
	case CodeUnauthorized, CodeUpstreamAuth:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeRateLimited, CodeQuotaExhausted:
		return http.StatusTooManyRequests
	case CodeUpstreamTimeout:
		return http.StatusGatewayTimeout
	case CodeUpstreamError, CodeModelUnavailable:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// New 创建错误。
func New(code Code, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

// Wrap 创建带底层错误的错误。
func Wrap(code Code, msg string, err error) *Error {
	return &Error{Code: code, Message: msg, Err: err}
}

// Retryable 标记错误可安全重试（429、超时、部分 5xx；鉴权类不可重试）。
func Retryable(e *Error) *Error {
	e.Retryable = true
	return e
}

// As 从错误链中提取 *Error。
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// From 将任意错误转为稳定错误。
func From(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := As(err); ok {
		return e
	}
	return Wrap(CodeInternal, "内部错误", err)
}
