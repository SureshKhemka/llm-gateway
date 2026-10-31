package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Stable gateway error codes [API-10].
const (
	CodeBudgetExceeded      = "budget_exceeded"
	CodeRateLimited         = "rate_limited"
	CodeNoEligibleModel     = "no_eligible_model"
	CodeUpstreamUnavailable = "upstream_unavailable"
	CodePolicyBlocked       = "policy_blocked"
	CodeContextTooLong      = "context_too_long"
	CodeInvalidRequest      = "invalid_request"
	CodeUnauthorized        = "unauthorized"
	CodeForbidden           = "forbidden"
	CodeNotFound            = "not_found"
	CodeBudgetUnavailable   = "budget_unavailable"
	CodeOverloaded          = "overloaded"
	CodeConflict            = "conflict"
	CodeInternal            = "internal_error"
)

var codeStatus = map[string]int{
	CodeBudgetExceeded:      http.StatusPaymentRequired, // 402: SDKs auto-retry 429
	CodeRateLimited:         http.StatusTooManyRequests,
	CodeNoEligibleModel:     http.StatusUnprocessableEntity,
	CodeUpstreamUnavailable: http.StatusServiceUnavailable,
	CodePolicyBlocked:       http.StatusForbidden,
	CodeContextTooLong:      http.StatusBadRequest,
	CodeInvalidRequest:      http.StatusBadRequest,
	CodeUnauthorized:        http.StatusUnauthorized,
	CodeForbidden:           http.StatusForbidden,
	CodeNotFound:            http.StatusNotFound,
	CodeBudgetUnavailable:   http.StatusServiceUnavailable,
	CodeOverloaded:          http.StatusServiceUnavailable,
	CodeConflict:            http.StatusConflict,
	CodeInternal:            http.StatusInternalServerError,
}

// Error is a gateway error with a stable code.
type Error struct {
	Code       string
	Message    string
	Status     int
	RetryAfter time.Duration
	Details    any // e.g. exclusion reasons for no_eligible_model
	Headers    map[string]string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func NewError(code, format string, args ...any) *Error {
	st, ok := codeStatus[code]
	if !ok {
		st = http.StatusInternalServerError
	}
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Status: st}
}

func StatusFor(code string) int {
	if st, ok := codeStatus[code]; ok {
		return st
	}
	return http.StatusInternalServerError
}

func openAIType(code string) string {
	switch code {
	case CodeUnauthorized:
		return "authentication_error"
	case CodeForbidden, CodePolicyBlocked:
		return "permission_error"
	case CodeRateLimited:
		return "rate_limit_error"
	case CodeBudgetExceeded:
		return "insufficient_quota"
	case CodeNotFound:
		return "not_found_error"
	case CodeUpstreamUnavailable, CodeInternal, CodeBudgetUnavailable, CodeOverloaded:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

func anthropicType(code string) string {
	switch code {
	case CodeUnauthorized:
		return "authentication_error"
	case CodeForbidden, CodePolicyBlocked, CodeBudgetExceeded:
		return "permission_error"
	case CodeRateLimited:
		return "rate_limit_error"
	case CodeNotFound:
		return "not_found_error"
	case CodeOverloaded:
		return "overloaded_error"
	case CodeUpstreamUnavailable, CodeInternal, CodeBudgetUnavailable:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

// OpenAIBody renders the error in the OpenAI error shape.
func (e *Error) OpenAIBody() []byte {
	inner := map[string]any{"message": e.Message, "type": openAIType(e.Code), "code": e.Code, "param": nil}
	if e.Details != nil {
		inner["details"] = e.Details
	}
	b, _ := json.Marshal(map[string]any{"error": inner})
	return b
}

// AnthropicBody renders the error in the Anthropic error shape.
func (e *Error) AnthropicBody() []byte {
	inner := map[string]any{"type": anthropicType(e.Code), "message": e.Message, "code": e.Code}
	if e.Details != nil {
		inner["details"] = e.Details
	}
	b, _ := json.Marshal(map[string]any{"type": "error", "error": inner})
	return b
}

type Format int

const (
	FormatOpenAI Format = iota
	FormatAnthropic
)

// WriteError writes e in the client's format with the matching status code.
func WriteError(w http.ResponseWriter, f Format, e *Error) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Gw-Error-Code", e.Code)
	for k, v := range e.Headers {
		h.Set(k, v)
	}
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int((e.RetryAfter+time.Second-1)/time.Second)))
	}
	status := e.Status
	if status == 0 {
		status = StatusFor(e.Code)
	}
	w.WriteHeader(status)
	if f == FormatAnthropic {
		_, _ = w.Write(e.AnthropicBody())
	} else {
		_, _ = w.Write(e.OpenAIBody())
	}
}
