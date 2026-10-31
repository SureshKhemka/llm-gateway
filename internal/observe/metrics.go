package observe

import (
	"net/http"
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics exports Prometheus metrics derived from request events [OBS-02]. Label values
// are bounded: tenants, models, providers and policies outside the configured sets are
// reported as "other", so a misbehaving client cannot explode cardinality.
type Metrics struct {
	reg *prometheus.Registry

	requests    *prometheus.CounterVec
	errors      *prometheus.CounterVec
	overhead    *prometheus.HistogramVec
	ttft        *prometheus.HistogramVec
	duration    *prometheus.HistogramVec
	tokens      *prometheus.CounterVec
	cost        *prometheus.CounterVec
	savings     *prometheus.CounterVec
	cache       *prometheus.CounterVec
	fallbacks   *prometheus.CounterVec
	escalations *prometheus.CounterVec

	mu     sync.RWMutex
	known  map[string]map[string]bool // dimension -> allowed values
	gauges func() []GaugeSample
}

// GaugeSample is one point-in-time value (circuit state, budget use, queue depth).
type GaugeSample struct {
	Name   string
	Help   string
	Labels map[string]string
	Value  float64
}

var latencyBuckets = []float64{.001, .0025, .005, .01, .015, .025, .04, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120}

func NewMetrics() *Metrics {
	m := &Metrics{reg: prometheus.NewRegistry(), known: map[string]map[string]bool{}}
	f := func(c prometheus.Collector) { m.reg.MustRegister(c) }
	m.requests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gw_requests_total", Help: "Requests by tenant, served model, provider, policy, status class and cache status."},
		[]string{"tenant", "model", "provider", "policy", "status", "cache"})
	m.errors = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gw_errors_total", Help: "Errors by tenant and gateway error code."}, []string{"tenant", "code"})
	m.overhead = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "gw_gateway_overhead_seconds", Help: "Time the gateway adds on top of upstream time.", Buckets: latencyBuckets}, []string{"endpoint", "stream"})
	m.ttft = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "gw_time_to_first_token_seconds", Help: "Time to first token per served model.", Buckets: latencyBuckets}, []string{"model"})
	m.duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "gw_request_duration_seconds", Help: "Total request latency per served model.", Buckets: latencyBuckets}, []string{"model"})
	m.tokens = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gw_tokens_total", Help: "Tokens by tenant, model and kind."}, []string{"tenant", "model", "kind"})
	m.cost = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gw_cost_usd_total", Help: "Cost in USD by tenant, model and policy."}, []string{"tenant", "model", "policy"})
	m.savings = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gw_savings_usd_total", Help: "Savings against the baseline in USD, by kind (routing, cache)."}, []string{"tenant", "kind"})
	m.cache = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gw_cache_requests_total", Help: "Requests by cache status (exact_hit, semantic_hit, miss, bypass)."}, []string{"tenant", "status"})
	m.fallbacks = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gw_fallbacks_total", Help: "Fallback attempts by model that failed."}, []string{"model"})
	m.escalations = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gw_cascade_escalations_total", Help: "Cascade escalations by policy."}, []string{"policy"})
	for _, c := range []prometheus.Collector{m.requests, m.errors, m.overhead, m.ttft, m.duration, m.tokens, m.cost, m.savings, m.cache, m.fallbacks, m.escalations} {
		f(c)
	}
	f(prometheus.NewGoCollector())
	f(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	f(&gaugeCollector{m})
	return m
}

// SetKnown installs the allowed label values for a dimension (tenant, model, provider,
// policy).
func (m *Metrics) SetKnown(dim string, values []string) {
	set := map[string]bool{"": true}
	for _, v := range values {
		set[v] = true
	}
	m.mu.Lock()
	m.known[dim] = set
	m.mu.Unlock()
}

// SetGauges installs the callback that reports live gauges at scrape time.
func (m *Metrics) SetGauges(fn func() []GaugeSample) {
	m.mu.Lock()
	m.gauges = fn
	m.mu.Unlock()
}

func (m *Metrics) bound(dim, v string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	set, ok := m.known[dim]
	if !ok || set[v] {
		return v
	}
	return "other"
}

func statusClass(s int) string {
	switch {
	case s == 0:
		return "0"
	case s < 300:
		return "2xx"
	case s == 499:
		return "client_closed"
	case s < 500:
		return "4xx"
	}
	return "5xx"
}

// Emit implements EventSink.
func (m *Metrics) Emit(e Event) {
	if e.Shadow {
		return
	}
	tenant := m.bound("tenant", e.Tenant)
	model := m.bound("model", e.ServedModel)
	provider := m.bound("provider", e.Provider)
	policy := m.bound("policy", e.Policy)
	m.requests.WithLabelValues(tenant, model, provider, policy, statusClass(e.Status), e.CacheStatus).Inc()
	if e.ErrorCode != "" {
		m.errors.WithLabelValues(tenant, e.ErrorCode).Inc()
	}
	m.overhead.WithLabelValues(e.Endpoint, strconv.FormatBool(e.Stream)).Observe(e.OverheadMs / 1000)
	if e.ServedModel != "" {
		m.duration.WithLabelValues(model).Observe(e.LatencyMs / 1000)
		if e.TTFTMs > 0 {
			m.ttft.WithLabelValues(model).Observe(e.TTFTMs / 1000)
		}
	}
	for kind, n := range map[string]int{"input": e.InputTokens - e.CachedTokens, "cached_input": e.CachedTokens, "output": e.OutputTokens - e.ReasoningToks, "reasoning": e.ReasoningToks} {
		if n > 0 {
			m.tokens.WithLabelValues(tenant, model, kind).Add(float64(n))
		}
	}
	if e.CostUSD > 0 {
		m.cost.WithLabelValues(tenant, model, policy).Add(e.CostUSD)
	}
	if e.CacheSavedUSD > 0 {
		m.savings.WithLabelValues(tenant, "cache").Add(e.CacheSavedUSD)
	} else if s := e.BaselineUSD - e.CostUSD; s > 0 {
		m.savings.WithLabelValues(tenant, "routing").Add(s)
	}
	if e.CacheStatus != "" {
		m.cache.WithLabelValues(tenant, e.CacheStatus).Inc()
	}
	for _, a := range e.Attempts {
		switch {
		case a.Kind == "cascade":
			m.escalations.WithLabelValues(policy).Inc()
		case a.Status == "error":
			m.fallbacks.WithLabelValues(m.bound("model", a.Deployment)).Inc()
		}
	}
}

// Handler serves /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Registry exposes the registry (tests).
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

type gaugeCollector struct{ m *Metrics }

func (g *gaugeCollector) Describe(chan<- *prometheus.Desc) {} // unchecked collector

func (g *gaugeCollector) Collect(ch chan<- prometheus.Metric) {
	g.m.mu.RLock()
	fn := g.m.gauges
	g.m.mu.RUnlock()
	if fn == nil {
		return
	}
	for _, s := range fn() {
		names := make([]string, 0, len(s.Labels))
		vals := make([]string, 0, len(s.Labels))
		for k, v := range s.Labels {
			names = append(names, k)
			vals = append(vals, v)
		}
		desc := prometheus.NewDesc(s.Name, s.Help, names, nil)
		if m, err := prometheus.NewConstMetric(desc, prometheus.GaugeValue, s.Value, vals...); err == nil {
			ch <- m
		}
	}
}
