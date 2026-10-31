// Package resilience tracks deployment health, runs circuit breakers, limits
// concurrency and computes retry backoff [RES-01, RES-02, RES-05, RES-07].
package resilience

import (
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

type BreakerState string

const (
	Closed   BreakerState = "closed"
	Open     BreakerState = "open"
	HalfOpen BreakerState = "half_open"
)

type Health string

const (
	Healthy   Health = "healthy"
	Degraded  Health = "degraded"
	Unhealthy Health = "unhealthy"
)

// BreakerConfig sets error-rate and latency thresholds over a rolling window.
type BreakerConfig struct {
	Window       time.Duration
	MinRequests  int
	ErrorRate    float64 // open at or above this share of failures
	LatencyP95Ms float64 // open when p95 latency exceeds this (0 = off)
	CoolDown     time.Duration
	// Consecutive opens the breaker after this many failures in a row, regardless of the
	// window's error rate (an unloaded model fails every request).
	Consecutive int
}

type outcome struct {
	at      time.Time
	ok      bool
	latency float64
}

type depState struct {
	mu        sync.Mutex
	window    []outcome
	state     BreakerState
	openedAt  time.Time
	probing   bool
	reason    string
	probeFail bool // last active probe failed (model unloaded / provider unreachable)
	consec    int
	cold      bool
	lastErr   string
	changedAt time.Time
	// Window aggregates kept incrementally: fails is exact; p95 is recomputed lazily and
	// at most every p95Every under load, since sorting the window per request dominated
	// the request path's CPU.
	fails    int
	p95      float64
	p95Dirty bool
	p95At    time.Time
	scratch  []float64
}

const p95Every = 200 * time.Millisecond

// Tracker holds health and breaker state for every deployment.
type Tracker struct {
	mu   sync.RWMutex
	cfg  BreakerConfig
	deps map[string]*depState
	Now  func() time.Time
	// OnChange is called on every breaker transition (metrics, audit, logs).
	OnChange func(dep string, from, to BreakerState, reason string)
}

func NewTracker(cfg BreakerConfig) *Tracker {
	if cfg.Window == 0 {
		cfg.Window = 30 * time.Second
	}
	if cfg.MinRequests == 0 {
		cfg.MinRequests = 5
	}
	if cfg.ErrorRate == 0 {
		cfg.ErrorRate = 0.5
	}
	if cfg.CoolDown == 0 {
		cfg.CoolDown = 10 * time.Second
	}
	if cfg.Consecutive == 0 {
		cfg.Consecutive = 5
	}
	return &Tracker{cfg: cfg, deps: map[string]*depState{}, Now: time.Now}
}

// SetConfig updates thresholds (hot reload).
func (t *Tracker) SetConfig(cfg BreakerConfig) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if cfg.Window == 0 {
		cfg.Window = t.cfg.Window
	}
	if cfg.MinRequests == 0 {
		cfg.MinRequests = t.cfg.MinRequests
	}
	if cfg.ErrorRate == 0 {
		cfg.ErrorRate = t.cfg.ErrorRate
	}
	if cfg.CoolDown == 0 {
		cfg.CoolDown = t.cfg.CoolDown
	}
	if cfg.Consecutive == 0 {
		cfg.Consecutive = t.cfg.Consecutive
	}
	t.cfg = cfg
}

func (t *Tracker) get(dep string) *depState {
	t.mu.RLock()
	s, ok := t.deps[dep]
	t.mu.RUnlock()
	if ok {
		return s
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok = t.deps[dep]; !ok {
		s = &depState{state: Closed}
		t.deps[dep] = s
	}
	return s
}

func (t *Tracker) config() BreakerConfig {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.cfg
}

// Available reports, without side effects, whether routing may consider dep: the
// breaker is closed, or open long enough to admit a half-open probe.
func (t *Tracker) Available(dep string) (Health, bool) {
	s := t.get(dep)
	cfg := t.config()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case Open:
		if t.Now().Sub(s.openedAt) >= cfg.CoolDown && !s.probing {
			return Unhealthy, true
		}
		return Unhealthy, false
	case HalfOpen:
		return Unhealthy, !s.probing
	}
	return t.healthLocked(s, cfg), true
}

// Acquire is called right before an attempt. In the half-open state it admits exactly
// one probe request; it returns false if the deployment must be skipped.
func (t *Tracker) Acquire(dep string) bool {
	s := t.get(dep)
	cfg := t.config()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case Open:
		if t.Now().Sub(s.openedAt) < cfg.CoolDown || s.probing {
			return false
		}
		t.transition(dep, s, HalfOpen, "cool-down elapsed")
		s.probing = true
		return true
	case HalfOpen:
		if s.probing {
			return false
		}
		s.probing = true
		return true
	}
	return true
}

// Record feeds one outcome. ok=false only for failures that say something about the
// deployment (not client errors).
func (t *Tracker) Record(dep string, ok bool, latencyMs float64, errMsg string) {
	s := t.get(dep)
	cfg := t.config()
	now := t.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.window = append(s.window, outcome{at: now, ok: ok, latency: latencyMs})
	s.p95Dirty = true
	t.trim(s, cfg, now)
	if ok {
		s.consec = 0
	} else {
		s.fails++
		s.lastErr = errMsg
		s.consec++
	}
	switch s.state {
	case HalfOpen:
		s.probing = false
		if ok {
			s.window, s.consec, s.fails, s.p95, s.p95Dirty = s.window[:0], 0, 0, 0, false
			t.transition(dep, s, Closed, "probe succeeded")
		} else {
			s.openedAt = now
			t.transition(dep, s, Open, "probe failed: "+errMsg)
		}
	case Closed:
		if why := t.shouldOpen(s, cfg); why != "" {
			s.openedAt = now
			t.transition(dep, s, Open, why)
		}
	case Open:
		s.probing = false
	}
}

// Release clears a half-open probe slot when an attempt ended without an outcome
// (for example the client went away).
func (t *Tracker) Release(dep string) {
	s := t.get(dep)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == HalfOpen {
		s.probing = false
	}
}

func (t *Tracker) trim(s *depState, cfg BreakerConfig, now time.Time) {
	cut := 0
	for cut < len(s.window) && now.Sub(s.window[cut].at) > cfg.Window {
		cut++
	}
	if over := len(s.window) - cut - 10000; over > 0 {
		cut += over
	}
	if cut == 0 {
		return
	}
	for _, o := range s.window[:cut] {
		if !o.ok {
			s.fails--
		}
	}
	// Reslicing (not copying) keeps trim O(cut); append reallocates once the tail is full.
	s.window = s.window[cut:]
	s.p95Dirty = true
}

// stats returns the window's size, error rate and (when needP95) p95 latency of
// successful requests.
func (t *Tracker) stats(s *depState, needP95 bool) (n int, errRate, p95 float64) {
	n = len(s.window)
	if n == 0 {
		return 0, 0, 0
	}
	errRate = float64(s.fails) / float64(n)
	if !needP95 {
		return n, errRate, s.p95
	}
	if s.p95Dirty && (n <= 256 || t.Now().Sub(s.p95At) >= p95Every) {
		lats := s.scratch[:0]
		for _, o := range s.window {
			if o.ok {
				lats = append(lats, o.latency)
			}
		}
		s.p95 = 0
		if len(lats) > 0 {
			sort.Float64s(lats)
			s.p95 = lats[min(len(lats)-1, len(lats)*95/100)]
		}
		s.scratch, s.p95Dirty, s.p95At = lats, false, t.Now()
	}
	return n, errRate, s.p95
}

func (t *Tracker) shouldOpen(s *depState, cfg BreakerConfig) string {
	if cfg.Consecutive > 0 && s.consec >= cfg.Consecutive {
		return "consecutive failures"
	}
	n, er, p95 := t.stats(s, cfg.LatencyP95Ms > 0)
	if n < cfg.MinRequests {
		return ""
	}
	if er >= cfg.ErrorRate {
		return "error rate over threshold"
	}
	if cfg.LatencyP95Ms > 0 && p95 > cfg.LatencyP95Ms {
		return "p95 latency over threshold"
	}
	return ""
}

func (t *Tracker) healthLocked(s *depState, cfg BreakerConfig) Health {
	if s.state != Closed {
		return Unhealthy
	}
	if s.probeFail {
		return Unhealthy
	}
	n, er, p95 := t.stats(s, cfg.LatencyP95Ms > 0)
	if n >= cfg.MinRequests && (er >= cfg.ErrorRate/2 || cfg.LatencyP95Ms > 0 && p95 > cfg.LatencyP95Ms*0.8) {
		return Degraded
	}
	return Healthy
}

func (t *Tracker) transition(dep string, s *depState, to BreakerState, reason string) {
	from := s.state
	if from == to {
		return
	}
	s.state, s.reason, s.changedAt = to, reason, t.Now()
	if t.OnChange != nil {
		go t.OnChange(dep, from, to, reason)
	}
}

// ProbeResult records an active health probe for a deployment [RES-01]. A failed probe
// (provider unreachable or model no longer listed) counts as a failure.
func (t *Tracker) ProbeResult(dep string, ok bool, listed bool, errMsg string) {
	s := t.get(dep)
	s.mu.Lock()
	s.probeFail = !ok || !listed
	s.cold = ok && !listed
	s.mu.Unlock()
	if !ok || !listed {
		if errMsg == "" {
			errMsg = "model not listed by provider"
		}
		t.Record(dep, false, 0, "probe: "+errMsg)
	}
}

// Cold reports whether the last probe saw the provider up but the model not loaded.
func (t *Tracker) Cold(dep string) bool {
	s := t.get(dep)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cold
}

// Status is a deployment's health for the admin API and console.
type Status struct {
	Deployment string       `json:"deployment"`
	Health     Health       `json:"health"`
	Breaker    BreakerState `json:"breaker"`
	Reason     string       `json:"reason,omitempty"`
	Requests   int          `json:"window_requests"`
	ErrorRate  float64      `json:"error_rate"`
	P95Ms      float64      `json:"p95_ms"`
	LastError  string       `json:"last_error,omitempty"`
	Cold       bool         `json:"cold"`
	Since      time.Time    `json:"since"`
}

func (t *Tracker) Status(dep string) Status {
	s := t.get(dep)
	cfg := t.config()
	s.mu.Lock()
	defer s.mu.Unlock()
	t.trim(s, cfg, t.Now())
	n, er, p95 := t.stats(s, true)
	return Status{Deployment: dep, Health: t.healthLocked(s, cfg), Breaker: s.state, Reason: s.reason, Requests: n,
		ErrorRate: er, P95Ms: p95, LastError: s.lastErr, Cold: s.cold, Since: s.changedAt}
}

func (t *Tracker) All() []Status {
	t.mu.RLock()
	ids := make([]string, 0, len(t.deps))
	for id := range t.deps {
		ids = append(ids, id)
	}
	t.mu.RUnlock()
	sort.Strings(ids)
	out := make([]Status, 0, len(ids))
	for _, id := range ids {
		out = append(out, t.Status(id))
	}
	return out
}

// Backoff returns exponential backoff with full jitter for retry n (0-based), never less
// than retryAfter [RES-05].
func Backoff(n int, base, maxDelay, retryAfter time.Duration) time.Duration {
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	d := base << uint(min(n, 16))
	if maxDelay > 0 && d > maxDelay {
		d = maxDelay
	}
	j := time.Duration(rand.Int64N(int64(d)/2+1)) + d/2
	if retryAfter > j {
		return retryAfter
	}
	return j
}
