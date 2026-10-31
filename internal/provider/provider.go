// Package provider adapts upstream inference APIs to the gateway's canonical format
// [PRV-01].
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/catalog"
	"github.com/SureshKhemka/llm-gateway/internal/cost"
)

// Stream yields canonical chunks; Recv returns io.EOF at the end.
type Stream interface {
	Recv() (*api.ChatChunk, error)
	Close() error
}

type Provider interface {
	ID() string
	Chat(ctx context.Context, d *catalog.Deployment, req *api.ChatRequest, to catalog.Timeouts) (*api.ChatResponse, error)
	ChatStream(ctx context.Context, d *catalog.Deployment, req *api.ChatRequest, to catalog.Timeouts) (Stream, error)
	Embeddings(ctx context.Context, d *catalog.Deployment, req *api.EmbeddingsRequest, to catalog.Timeouts) (*api.EmbeddingsResponse, error)
	ListModels(ctx context.Context) ([]string, error)
}

// Kind classifies upstream failures for retry/fallback decisions [RES-03].
type Kind string

const (
	KindConnect       Kind = "connect"
	KindTimeout       Kind = "timeout"
	KindRateLimited   Kind = "rate_limited"
	KindServer        Kind = "server_error"
	KindContextLength Kind = "context_length"
	KindClient        Kind = "client_error"
	KindAuth          Kind = "auth"
	KindProtocol      Kind = "protocol"
	KindCanceled      Kind = "canceled"
)

type UpstreamError struct {
	Kind       Kind
	Status     int
	Message    string
	RetryAfter time.Duration
}

func (e *UpstreamError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("upstream %s (%d): %s", e.Kind, e.Status, e.Message)
	}
	return fmt.Sprintf("upstream %s: %s", e.Kind, e.Message)
}

// Retriable reports whether another candidate (or a retry) may succeed.
func (e *UpstreamError) Retriable() bool {
	switch e.Kind {
	case KindConnect, KindTimeout, KindRateLimited, KindServer, KindContextLength, KindProtocol:
		return true
	}
	return false
}

// CountsAgainstHealth reports whether the failure says something about the deployment.
func (e *UpstreamError) CountsAgainstHealth() bool {
	switch e.Kind {
	case KindConnect, KindTimeout, KindServer, KindRateLimited, KindProtocol, KindAuth:
		return true
	}
	return false
}

// AsUpstream extracts an UpstreamError, wrapping unknown errors as connect errors.
func AsUpstream(err error) *UpstreamError {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue
	}
	if errors.Is(err, context.Canceled) {
		return &UpstreamError{Kind: KindCanceled, Message: "request canceled"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &UpstreamError{Kind: KindTimeout, Message: "deadline exceeded"}
	}
	return &UpstreamError{Kind: KindConnect, Message: err.Error()}
}

var contextLengthHints = []string{"context length", "context_length", "maximum context", "too many tokens", "context window", "prompt is too long"}

// ClassifyHTTP maps an upstream HTTP error to an UpstreamError.
func ClassifyHTTP(status int, header http.Header, msg string) *UpstreamError {
	e := &UpstreamError{Status: status, Message: truncate(msg, 300)}
	lower := strings.ToLower(msg)
	switch {
	case status == http.StatusTooManyRequests:
		e.Kind = KindRateLimited
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Kind = KindAuth
	case status >= 500:
		e.Kind = KindServer
	case status == http.StatusRequestTimeout:
		e.Kind = KindTimeout
	default:
		e.Kind = KindClient
	}
	if status == 400 || status == 413 || status == 422 {
		for _, h := range contextLengthHints {
			if strings.Contains(lower, h) {
				e.Kind = KindContextLength
			}
		}
	}
	e.RetryAfter = ParseRetryAfter(header.Get("Retry-After"))
	return e
}

// ParseRetryAfter accepts delta-seconds or an HTTP date.
func ParseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// UsageOf converts OpenAI usage to the cost split [CST-01].
func UsageOf(u *api.Usage) cost.Usage {
	if u == nil {
		return cost.Usage{}
	}
	cached := u.Cached()
	reasoning := u.Reasoning()
	in := u.PromptTokens - cached
	out := u.CompletionTokens - reasoning
	if in < 0 {
		in = 0
	}
	if out < 0 {
		out = 0
	}
	return cost.Usage{Input: in, CachedInput: cached, Output: out, Reasoning: reasoning}
}

// Registry holds one Provider per configured provider ID.
type Registry struct {
	byID  map[string]Provider
	specs map[string]string
}

func NewRegistry() *Registry {
	return &Registry{byID: map[string]Provider{}, specs: map[string]string{}}
}

func (r *Registry) Set(p Provider)           { r.byID[p.ID()] = p }
func (r *Registry) Get(id string) Provider   { return r.byID[id] }
func (r *Registry) All() map[string]Provider { return r.byID }

// SetWithSpec registers an adapter with the provider definition it was built from, so
// reloads can reuse adapters (and connection pools) whose definition did not change.
func (r *Registry) SetWithSpec(p Provider, spec *catalog.Provider) {
	r.byID[spec.ID] = p
	r.specs[spec.ID] = specKey(spec)
}

func (r *Registry) SameSpec(id string, spec *catalog.Provider) bool {
	return r.specs[id] == specKey(spec)
}

func specKey(p *catalog.Provider) string {
	return fmt.Sprintf("%s|%s|%s|%v|%v", p.Type, p.BaseURL, p.APIKey, p.Headers, p.Timeouts)
}

// MergeTimeouts returns the first non-zero value of each field across ts.
func MergeTimeouts(ts ...catalog.Timeouts) catalog.Timeouts {
	var out catalog.Timeouts
	for _, t := range ts {
		if out.Connect == 0 {
			out.Connect = t.Connect
		}
		if out.TTFT == 0 {
			out.TTFT = t.TTFT
		}
		if out.Idle == 0 {
			out.Idle = t.Idle
		}
		if out.Total == 0 {
			out.Total = t.Total
		}
	}
	return out
}
