package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/cost"
	"github.com/SureshKhemka/llm-gateway/internal/governance"
	"github.com/SureshKhemka/llm-gateway/internal/observe"
	"gopkg.in/yaml.v3"
)

// Hints are per-request routing hints from headers or body metadata [API-08]. They can
// only narrow operator policy [RTE-10].
type Hints struct {
	MaxCostUSD      *float64 `json:"max_cost_usd,omitempty"`
	LatencyTargetMs *float64 `json:"latency_target_ms,omitempty"`
	MinTier         *int     `json:"min_tier,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
	AllowProviders  []string `json:"allow_providers,omitempty"`
	DenyProviders   []string `json:"deny_providers,omitempty"`
	Cache           string   `json:"cache,omitempty"` // "" | bypass | refresh | no-store
	Session         string   `json:"session_id,omitempty"`
	EndUser         string   `json:"end_user,omitempty"`
	Tags            []string `json:"tags,omitempty"`
	Sensitivity     string   `json:"sensitivity,omitempty"`
	TimeoutMs       *int     `json:"timeout_ms,omitempty"`
	TTFTTimeoutMs   *int     `json:"ttft_timeout_ms,omitempty"`
	IdempotencyKey  string   `json:"idempotency_key,omitempty"`
	Priority        *int     `json:"priority,omitempty"`
}

// RequestContext carries everything about a request except its body.
type RequestContext struct {
	ID        string
	Start     time.Time
	Endpoint  string // chat | messages | embeddings | responses | explain
	Format    api.Format
	Principal *governance.Principal
	Hints     Hints
	ClientIP  netip.Addr
	TraceID   string
	EvalRun   string
	Shadow    bool
	// Explain asks the engine to plan only (route-explain) [RTE-15].
	Explain bool
	// BodyBytes is the request body size (for caps).
	BodyBytes int
	// PolicyOverride routes with this policy (eval runs of candidate versions) [EVL-02].
	PolicyOverride *config.Policy

	budgets     []governance.BudgetRef
	reservation *governance.Reservation
	admission   *governance.Admission
	maxOutCap   int
	overheadUSD float64
}

func (rc *RequestContext) EndUser() string { return rc.Hints.EndUser }

func (rc *RequestContext) Tags() []string {
	tags := append([]string(nil), rc.Principal.Scopes.DefaultTags...)
	return append(tags, rc.Hints.Tags...)
}

func NewRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

// Meta is the per-request outcome returned to the client as headers [API-09].
type Meta struct {
	RequestID          string            `json:"request_id"`
	RequestedModel     string            `json:"requested_model"`
	Deployment         string            `json:"served_model"`
	Provider           string            `json:"provider"`
	Policy             string            `json:"policy"`
	PolicyVersion      int               `json:"policy_version"`
	RouteReason        string            `json:"route_reason"`
	CacheStatus        string            `json:"cache_status"`
	Usage              cost.Usage        `json:"usage"`
	Cost               cost.Breakdown    `json:"cost_breakdown"`
	CostUSD            float64           `json:"cost_usd"`
	OverheadUSD        float64           `json:"overhead_usd"`
	BaselineUSD        float64           `json:"baseline_cost_usd"`
	CacheSavedUSD      float64           `json:"cache_saved_usd"`
	BudgetRemainingUSD *float64          `json:"budget_remaining_usd,omitempty"`
	FallbackAttempts   int               `json:"fallback_attempts"`
	Attempts           []observe.Attempt `json:"attempts"`
	Difficulty         float64           `json:"difficulty_score"`
	Candidates         int               `json:"candidates"`
	TTFTMs             float64           `json:"ttft_ms,omitempty"`
	LatencyMs          float64           `json:"latency_ms"`
	UpstreamMs         float64           `json:"upstream_ms"`
	Status             int               `json:"status"`
	ErrorCode          string            `json:"error_code,omitempty"`
	RateHeaders        map[string]string `json:"-"`
	PII                []string          `json:"pii,omitempty"`
}

// Headers renders Meta as response headers [API-09].
func (m *Meta) Headers(h http.Header) {
	set := func(k, v string) {
		if v != "" {
			h.Set(k, v)
		}
	}
	set("X-Request-Id", m.RequestID)
	set("X-Gw-Served-Model", m.Deployment)
	set("X-Gw-Provider", m.Provider)
	set("X-Gw-Policy", m.Policy)
	if m.PolicyVersion > 0 {
		set("X-Gw-Policy-Version", strconv.Itoa(m.PolicyVersion))
	}
	set("X-Gw-Route-Reason", m.RouteReason)
	set("X-Gw-Cache", m.CacheStatus)
	set("X-Gw-Fallback-Attempts", strconv.Itoa(m.FallbackAttempts))
	for k, v := range m.RateHeaders {
		set(k, v)
	}
}

// CostHeaders renders the fields only known once the response is complete.
func (m *Meta) CostHeaders(h http.Header) {
	h.Set("X-Gw-Input-Tokens", strconv.Itoa(m.Usage.TotalInput()))
	h.Set("X-Gw-Output-Tokens", strconv.Itoa(m.Usage.TotalOutput()))
	h.Set("X-Gw-Cost-Usd", formatUSD(m.CostUSD))
	h.Set("X-Gw-Baseline-Cost-Usd", formatUSD(m.BaselineUSD))
	h.Set("X-Gw-Usage-Estimated", strconv.FormatBool(m.Usage.Estimated))
	if m.BudgetRemainingUSD != nil {
		h.Set("X-Gw-Budget-Remaining-Usd", formatUSD(*m.BudgetRemainingUSD))
	}
}

// CostTrailerNames are announced as HTTP trailers on streaming responses.
var CostTrailerNames = []string{"X-Gw-Input-Tokens", "X-Gw-Output-Tokens", "X-Gw-Cost-Usd", "X-Gw-Baseline-Cost-Usd", "X-Gw-Usage-Estimated", "X-Gw-Budget-Remaining-Usd"}

// Summary is the gateway metadata attached to the final stream chunk.
func (m *Meta) Summary() map[string]any {
	out := map[string]any{
		"request_id": m.RequestID, "served_model": m.Deployment, "provider": m.Provider,
		"policy": m.Policy, "policy_version": m.PolicyVersion, "route_reason": m.RouteReason,
		"cache_status": m.CacheStatus, "input_tokens": m.Usage.TotalInput(), "output_tokens": m.Usage.TotalOutput(),
		"cost_usd": m.CostUSD, "usage_estimated": m.Usage.Estimated, "fallback_attempts": m.FallbackAttempts,
	}
	if m.BudgetRemainingUSD != nil {
		out["budget_remaining_usd"] = *m.BudgetRemainingUSD
	}
	return out
}

func formatUSD(v float64) string { return strconv.FormatFloat(v, 'f', 10, 64) }

// DecodePolicy parses a stored policy body (JSON or YAML).
func DecodePolicy(b []byte) (*config.Policy, error) {
	var p config.Policy
	if err := yaml.Unmarshal(b, &p); err != nil {
		if err2 := json.Unmarshal(b, &p); err2 != nil {
			return nil, fmt.Errorf("policy body: %w", err)
		}
	}
	return &p, nil
}

func (rc *RequestContext) admissionHeaders() map[string]string {
	if rc.admission == nil {
		return nil
	}
	return rc.admission.Headers
}

// anonymous is the principal for the gateway's internal calls.
var anonymous = &governance.Principal{Anonymous: true, KeyPrefix: "internal"}
