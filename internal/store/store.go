// Package store persists durable records: cost records, route decisions, audit log,
// keys, policy versions and eval data. `memory` backs tests and single-node dev;
// `postgres` backs production.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var ErrNotFound = errors.New("not found")

// CostRecord is the per-request cost and attribution record [CST-01..04].
type CostRecord struct {
	RequestID      string          `json:"request_id"`
	Time           time.Time       `json:"ts"`
	Tenant         string          `json:"tenant"`
	Project        string          `json:"project"`
	KeyID          string          `json:"key_id"`
	EndUser        string          `json:"end_user,omitempty"`
	Tags           []string        `json:"tags,omitempty"`
	Endpoint       string          `json:"endpoint"`
	RequestedModel string          `json:"requested_model"`
	Deployment     string          `json:"deployment"`
	Provider       string          `json:"provider"`
	Policy         string          `json:"policy"`
	PolicyVersion  int             `json:"policy_version"`
	RouteReason    string          `json:"route_reason"`
	CacheStatus    string          `json:"cache_status"`
	InputTokens    int             `json:"input_tokens"`
	CachedTokens   int             `json:"cached_input_tokens"`
	OutputTokens   int             `json:"output_tokens"`
	ReasoningToks  int             `json:"reasoning_tokens"`
	CostUSD        float64         `json:"cost_usd"`
	InputUSD       float64         `json:"input_usd"`
	CachedInputUSD float64         `json:"cached_input_usd"`
	OutputUSD      float64         `json:"output_usd"`
	ReasoningUSD   float64         `json:"reasoning_usd"`
	OverheadUSD    float64         `json:"overhead_usd"`
	BaselineUSD    float64         `json:"baseline_cost_usd"`
	CacheSavedUSD  float64         `json:"cache_saved_usd"`
	Estimated      bool            `json:"estimated"`
	Attempts       int             `json:"attempts"`
	Status         int             `json:"status"`
	ErrorCode      string          `json:"error_code,omitempty"`
	LatencyMs      float64         `json:"latency_ms"`
	TTFTMs         float64         `json:"ttft_ms"`
	Shadow         bool            `json:"shadow"`
	EvalRun        string          `json:"eval_run,omitempty"`
	Extra          json.RawMessage `json:"extra,omitempty"`
}

// RoutingSavings is what routing saved against the baseline (cache savings separate) [CST-03].
func (r CostRecord) RoutingSavings() float64 {
	if r.CacheStatus == "exact_hit" || r.CacheStatus == "semantic_hit" {
		return 0
	}
	return r.BaselineUSD - r.CostUSD
}

// CostFilter selects cost records. Zero fields match everything.
type CostFilter struct {
	Tenant, Project, KeyID, EndUser, Deployment, Provider, Policy, Tag, EvalRun string
	From, To                                                                    time.Time
	IncludeShadow                                                               bool
}

// CostRow is one aggregate bucket [CST-04].
type CostRow struct {
	Group          map[string]string `json:"group"`
	Bucket         time.Time         `json:"bucket,omitempty"`
	Requests       int64             `json:"requests"`
	InputTokens    int64             `json:"input_tokens"`
	CachedTokens   int64             `json:"cached_input_tokens"`
	OutputTokens   int64             `json:"output_tokens"`
	ReasoningToks  int64             `json:"reasoning_tokens"`
	CostUSD        float64           `json:"cost_usd"`
	BaselineUSD    float64           `json:"baseline_cost_usd"`
	RoutingSavings float64           `json:"routing_savings_usd"`
	CacheSavings   float64           `json:"cache_savings_usd"`
	CacheHits      int64             `json:"cache_hits"`
	Errors         int64             `json:"errors"`
}

type CostQuery struct {
	Filter  CostFilter
	GroupBy []string      // tenant, project, key, end_user, model, provider, policy, tag
	Bucket  time.Duration // 0 = no time bucketing
}

// DecisionRecord is the stored routing explanation for one request [RTE-14].
type DecisionRecord struct {
	RequestID string          `json:"request_id"`
	Time      time.Time       `json:"ts"`
	Tenant    string          `json:"tenant"`
	Body      json.RawMessage `json:"decision"`
}

// AuditEntry is one append-only admin/config change record [OBS-06].
type AuditEntry struct {
	ID         int64           `json:"id"`
	Time       time.Time       `json:"ts"`
	Actor      string          `json:"actor"`
	Role       string          `json:"role"`
	Tenant     string          `json:"tenant,omitempty"`
	Action     string          `json:"action"`
	ObjectKind string          `json:"object_kind"`
	ObjectID   string          `json:"object_id"`
	Before     json.RawMessage `json:"before,omitempty"`
	After      json.RawMessage `json:"after,omitempty"`
	Reason     string          `json:"reason,omitempty"`
}

type AuditFilter struct {
	Tenant     string
	ObjectKind string
	Since      time.Time
	Limit      int
}

// KeyRecord is a virtual API key as stored: hash only, never the key [TEN-02].
type KeyRecord struct {
	ID           string          `json:"id"`
	Prefix       string          `json:"prefix"`
	Hash         []byte          `json:"-"`
	Tenant       string          `json:"tenant"`
	Project      string          `json:"project"`
	Name         string          `json:"name"`
	Scopes       json.RawMessage `json:"scopes"`
	CreatedAt    time.Time       `json:"created_at"`
	ExpiresAt    *time.Time      `json:"expires_at,omitempty"`
	RevokedAt    *time.Time      `json:"revoked_at,omitempty"`
	RotatedFrom  string          `json:"rotated_from,omitempty"`
	OverlapUntil *time.Time      `json:"overlap_until,omitempty"`
	Managed      string          `json:"managed"` // file | api
}

// PolicyVersion is one retained version of a routing policy [CFG-06].
type PolicyVersion struct {
	Name           string          `json:"name"`
	Version        int             `json:"version"`
	CreatedAt      time.Time       `json:"created_at"`
	Actor          string          `json:"actor"`
	Body           json.RawMessage `json:"body"`
	Active         bool            `json:"active"`
	EvalRun        string          `json:"eval_run,omitempty"`
	OverrideReason string          `json:"override_reason,omitempty"`
}

// Generic is a small document store for objects that only need get/put/list by kind:
// tenants and budgets created through the admin API, eval suites and runs, feedback.
type Doc struct {
	Kind    string          `json:"kind"`
	ID      string          `json:"id"`
	Tenant  string          `json:"tenant,omitempty"`
	Updated time.Time       `json:"updated"`
	Body    json.RawMessage `json:"body"`
}

type Store interface {
	InsertCostRecords(ctx context.Context, recs []CostRecord) error
	QueryCost(ctx context.Context, q CostQuery) ([]CostRow, error)
	ListCostRecords(ctx context.Context, f CostFilter, limit int) ([]CostRecord, error)

	PutDecision(ctx context.Context, d DecisionRecord) error
	GetDecision(ctx context.Context, requestID string) (*DecisionRecord, error)

	AppendAudit(ctx context.Context, e AuditEntry) (int64, error)
	ListAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, error)

	PutKey(ctx context.Context, k KeyRecord) error
	GetKeyByHash(ctx context.Context, hash []byte) (*KeyRecord, error)
	GetKey(ctx context.Context, id string) (*KeyRecord, error)
	ListKeys(ctx context.Context, tenant string) ([]KeyRecord, error)

	PutPolicyVersion(ctx context.Context, p PolicyVersion) error
	ListPolicyVersions(ctx context.Context, name string) ([]PolicyVersion, error)
	ActivatePolicyVersion(ctx context.Context, name string, version int) error

	PutDoc(ctx context.Context, d Doc) error
	GetDoc(ctx context.Context, kind, id string) (*Doc, error)
	ListDocs(ctx context.Context, kind, tenant string) ([]Doc, error)
	DeleteDoc(ctx context.Context, kind, id string) error

	Ping(ctx context.Context) error
	Close() error
}
