// Package observe emits per-request events, metrics and traces [OBS-01..03].
package observe

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Attempt describes one upstream attempt (primary, fallback or cascade).
type Attempt struct {
	Deployment string  `json:"deployment"`
	Provider   string  `json:"provider"`
	Kind       string  `json:"kind"` // primary | fallback | cascade | classifier | judge
	Status     string  `json:"status"`
	Error      string  `json:"error,omitempty"`
	CostUSD    float64 `json:"cost_usd"`
	LatencyMs  float64 `json:"latency_ms"`
	TTFTMs     float64 `json:"ttft_ms,omitempty"`
}

// Event is the one structured record emitted per request [OBS-01]. It never carries
// prompt or response content, provider keys or full virtual keys [NFR-16].
type Event struct {
	Time           time.Time `json:"ts"`
	RequestID      string    `json:"request_id"`
	Endpoint       string    `json:"endpoint"`
	Format         string    `json:"format"`
	Tenant         string    `json:"tenant"`
	Project        string    `json:"project"`
	KeyPrefix      string    `json:"key_prefix"`
	EndUser        string    `json:"end_user,omitempty"`
	Tags           []string  `json:"tags,omitempty"`
	RequestedModel string    `json:"requested_model"`
	ServedModel    string    `json:"served_model,omitempty"`
	Provider       string    `json:"provider,omitempty"`
	Policy         string    `json:"policy,omitempty"`
	PolicyVersion  int       `json:"policy_version,omitempty"`
	RouteReason    string    `json:"route_reason,omitempty"`
	Candidates     int       `json:"candidate_count"`
	Difficulty     float64   `json:"difficulty_score"`
	CacheStatus    string    `json:"cache_status"`
	Attempts       []Attempt `json:"attempts,omitempty"`
	InputTokens    int       `json:"input_tokens"`
	CachedTokens   int       `json:"cached_input_tokens"`
	OutputTokens   int       `json:"output_tokens"`
	ReasoningToks  int       `json:"reasoning_tokens"`
	UsageEstimated bool      `json:"usage_estimated"`
	CostUSD        float64   `json:"cost_usd"`
	BaselineUSD    float64   `json:"baseline_cost_usd"`
	CacheSavedUSD  float64   `json:"cache_saved_usd"`
	TTFTMs         float64   `json:"ttft_ms,omitempty"`
	LatencyMs      float64   `json:"latency_ms"`
	OverheadMs     float64   `json:"gateway_overhead_ms"`
	TokensPerSec   float64   `json:"tokens_per_sec,omitempty"`
	Stream         bool      `json:"stream"`
	Status         int       `json:"status"`
	ErrorCode      string    `json:"error_code,omitempty"`
	Shadow         bool      `json:"shadow,omitempty"`
	TraceID        string    `json:"trace_id,omitempty"`
	PII            []string  `json:"pii_detected,omitempty"`
}

// EventSink receives request events.
type EventSink interface{ Emit(Event) }

// JSONLogger writes one JSON line per event.
type JSONLogger struct {
	mu sync.Mutex
	w  io.Writer
}

func NewJSONLogger(w io.Writer) *JSONLogger { return &JSONLogger{w: w} }

func (l *JSONLogger) Emit(e Event) {
	b, err := json.Marshal(struct {
		Type string `json:"type"`
		Event
	}{"request", e})
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.w.Write(append(b, '\n'))
}

// Recorder keeps events in memory (tests, the request explorer's live tail).
type Recorder struct {
	mu     sync.Mutex
	events []Event
	max    int
}

func NewRecorder(max int) *Recorder { return &Recorder{max: max} }

func (r *Recorder) Emit(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	if r.max > 0 && len(r.events) > r.max {
		r.events = r.events[len(r.events)-r.max:]
	}
}

func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

func (r *Recorder) Last() (Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return Event{}, false
	}
	return r.events[len(r.events)-1], true
}

// Multi fans events out to several sinks.
type Multi []EventSink

func (m Multi) Emit(e Event) {
	for _, s := range m {
		if s != nil {
			s.Emit(e)
		}
	}
}
