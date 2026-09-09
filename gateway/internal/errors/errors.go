// Package errors 定义全网关统一的错误模型（契约 §0.3）。
//
// 规则：任何对外错误都必须使用本包的 Code，禁止自造错误码。
package errors

import (
	"errors"
	"fmt"
	"net/http"
)

// Code 稳定错误码，写入审计日志与 HTTP 响应。
type Code string

const (
	CodeInvalidRequest      Code = "invalid_request"
	CodeInvalidConfig       Code = "invalid_config"
	CodeDetectorUnavailable Code = "detector_unavailable"
	CodeDetectorTimeout     Code = "detector_timeout"
	CodeCircuitOpen         Code = "circuit_open"
	CodeReplaceFailed       Code = "replace_failed"
	CodeRestoreFailed       Code = "restore_failed"
	CodeUnauthorized        Code = "unauthorized"
	CodeUpstreamError       Code = "upstream_error"
	CodeVaultSealFailed     Code = "vault_seal_failed"
	CodeNotFound            Code = "not_found"
)

// Error 统一错误类型。cause 仅用于内部日志，不对外暴露。
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	cause   error
}

// Error 实现 error 接口。
func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// Unwrap 支持 errors.Is/As 链式判定。
func (e *Error) Unwrap() error { return e.cause }

// Cause 返回内部原因，仅供日志使用。
func (e *Error) Cause() error { return e.cause }

// New 构造一个错误。
func New(code Code, msg string) *Error { return &Error{Code: code, Message: msg} }

// Wrap 用 cause 包装一个错误。
func Wrap(code Code, msg string, cause error) *Error {
	return &Error{Code: code, Message: msg, cause: cause}
}

// Errorf 格式化构造错误。
func Errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Body HTTP 错误响应体。
type Body struct {
	Error ErrorPayload `json:"error"`
}

// ErrorPayload 错误响应负载。
type ErrorPayload struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

// Response 将错误写成 OpenAI 风格 JSON 错误响应。
func Response(e *Error) (int, Body) {
	return HTTPStatus(e.Code), Body{Error: ErrorPayload{Code: e.Code, Message: e.Message}}
}

// HTTPStatus 错误码 → HTTP 状态码映射（契约 §0.3）。
func HTTPStatus(c Code) int {
	switch c {
	case CodeInvalidRequest, CodeInvalidConfig:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeNotFound:
		return http.StatusNotFound
	case CodeDetectorTimeout, CodeCircuitOpen, CodeUpstreamError:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// As 从任意 error 中提取 *Error。
func As(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// Sentinels 常用哨兵错误，便于测试与 errors.Is 判定。
var (
	ErrDetectorTimeout     = New(CodeDetectorTimeout, "PII detection exceeded deadline")
	ErrDetectorUnavailable = New(CodeDetectorUnavailable, "detector sidecar unavailable")
	ErrCircuitOpen         = New(CodeCircuitOpen, "detector circuit breaker is open")
	ErrNotFound            = New(CodeNotFound, "mapping table not found or expired")
)
