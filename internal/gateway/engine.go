package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/cache"
	"github.com/SureshKhemka/llm-gateway/internal/catalog"
	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/cost"
	"github.com/SureshKhemka/llm-gateway/internal/governance"
	"github.com/SureshKhemka/llm-gateway/internal/observe"
	"github.com/SureshKhemka/llm-gateway/internal/provider"
	"github.com/SureshKhemka/llm-gateway/internal/resilience"
	"github.com/SureshKhemka/llm-gateway/internal/store"
	"github.com/SureshKhemka/llm-gateway/internal/tokens"
)

// EstimatorFunc is a pluggable difficulty estimator (similarity, classifier). It returns
// ok=false to fall back to the rule-based estimator.
type EstimatorFunc func(s *Snapshot, rc *RequestContext, req *api.ChatRequest, pol *config.Policy, dec *Decision) (Difficulty, bool)

// Engine executes requests against the current snapshot.
type Engine struct {
	snap       atomic.Pointer[Snapshot]
	Store      store.Store
	Writer     *store.Writer
	Events     observe.EventSink
	Log        *slog.Logger
	Now        func() time.Time
	Latency    *LatencyTracker
	Health     *resilience.Tracker
	Limiter    *resilience.Limiter
	Pins       *Pins
	Gov        *governance.Governor
	Discovery  *Discovery
	Factory    ProviderFactory
	Hooks      RouteHooks
	Estimators map[string]EstimatorFunc
	Cache      cache.Store
	shadows    sync.WaitGroup
}

type EngineOptions struct {
	Store  store.Store
	Events observe.EventSink
	Log    *slog.Logger
	Now    func() time.Time
	WALDir string
	// Counters back budgets and rate limits (Redis in production); nil = in-memory.
	Counters governance.Counters
	Factory  ProviderFactory
	Cache    cache.Store
}

func NewEngine(s *Snapshot, o EngineOptions) *Engine {
	if o.Store == nil {
		o.Store = store.NewMemory()
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Events == nil {
		o.Events = observe.Multi{}
	}
	e := &Engine{Store: o.Store, Events: o.Events, Log: o.Log, Now: o.Now,
		Writer:  store.NewWriter(o.Store, o.WALDir, time.Second, o.Log),
		Latency: NewLatencyTracker(s.Config.Resilience.LatencyAlpha),
		Health:  resilience.NewTracker(BreakerConfig(s)),
		Limiter: resilience.NewLimiter(),
		Pins:    NewPins(30 * time.Minute)}
	if o.Counters == nil {
		o.Counters = governance.NewMemCounters()
	}
	e.Gov = governance.NewGovernor(o.Counters, o.Log)
	e.Discovery = NewDiscovery()
	e.Factory = o.Factory
	e.Cache = o.Cache
	if e.Cache == nil {
		e.Cache = cache.NewMemory()
	}
	e.Estimators = map[string]EstimatorFunc{"classifier": e.classifierEstimator, "similarity": e.similarityEstimator}
	e.Gov.FailOpen = s.Config.Storage.BudgetsFailOpen
	e.Gov.Alerts.SetSinks(s.Config.Alerts)
	e.Hooks = RouteHooks{
		Health: func(dep string) (string, bool) {
			h, ok := e.Health.Available(dep)
			return string(h), ok
		},
		InFlight: e.Limiter.InFlight,
		Pinned:   e.Pins.Get,
		Cold:     e.Health.Cold,
		BudgetUtilisation: func(rc *RequestContext) float64 {
			return e.Gov.Utilisation(context.Background(), rc.budgets)
		},
	}
	e.snap.Store(s)
	return e
}

// BreakerConfig derives breaker thresholds from a snapshot's configuration.
func BreakerConfig(s *Snapshot) resilience.BreakerConfig {
	b := s.Config.Resilience.Breaker
	return resilience.BreakerConfig{Window: b.Window.D(), MinRequests: b.MinRequests, ErrorRate: b.ErrorRate,
		LatencyP95Ms: b.LatencyP95Ms, CoolDown: b.CoolDown.D(), Consecutive: b.Consecutive}
}

func (e *Engine) Snapshot() *Snapshot { return e.snap.Load() }
func (e *Engine) Swap(s *Snapshot) {
	e.snap.Store(s)
	e.Health.SetConfig(BreakerConfig(s))
	e.Gov.FailOpen = s.Config.Storage.BudgetsFailOpen
	e.Gov.Alerts.SetSinks(s.Config.Alerts)
}

// govern applies rate limits and reserves budget before any upstream call [BUD-03,
// BUD-04]. It must run after plan() so the reservation can price the candidates.
func (e *Engine) admit(ctx context.Context, s *Snapshot, rc *RequestContext, req *api.ChatRequest, promptTokens int) *api.Error {
	if rc.Explain {
		return nil
	}
	pr := rc.Principal
	ad, aerr := e.Gov.Admit(ctx, rc.ID, governance.RateScopes(pr), promptTokens)
	if aerr != nil {
		return aerr
	}
	rc.admission = ad
	rc.budgets = e.Gov.Applicable(s.Config.Budgets, s.Grants, governance.Scope{
		Tenant: pr.TenantID(), Project: pr.ProjectID(), Key: pr.KeyID, EndUser: rc.EndUser(),
		Eval: rc.EvalRun != "", Shadow: rc.Shadow})
	if len(rc.budgets) > 0 && req != nil && req.MaxOutput(0) == 0 {
		// Under a hard budget the output cap must be explicit so the reservation is a
		// true worst case.
		cap := s.Config.Defaults.MaxOutputTokens
		req.MaxTokens = &cap
		rc.maxOutCap = cap
	}
	res, berr := e.Gov.Reserve(ctx, rc.budgets, 0)
	if berr != nil {
		return berr
	}
	rc.reservation = res
	return nil
}
func (e *Engine) Close() { e.Writer.Close() }

// Plan is the ordered list of deployments to try plus the decision that produced it.
type Plan struct {
	Requested     string
	Policy        *config.Policy
	PolicyName    string
	PolicyVersion int
	Attempts      []*catalog.Deployment
	Reason        string
	Decision      *Decision
	PromptTokens  int
	MaxOutput     int
	// Redacted is sent to non-local deployments when PII redaction applies [GRD-04].
	Redacted *api.ChatRequest
}

// plan resolves the requested model: virtual models go through the router; concrete
// deployments are used as-is [API-07].
func (e *Engine) plan(s *Snapshot, rc *RequestContext, req *api.ChatRequest) (*Plan, error) {
	p := &Plan{Requested: req.Model, PromptTokens: tokens.Prompt(req)}
	p.MaxOutput = req.MaxOutput(s.Config.Defaults.MaxOutputTokens)
	if req.Model == "" {
		return nil, api.NewError(api.CodeInvalidRequest, "model is required")
	}
	if !rc.Principal.ModelAllowed(req.Model) {
		return nil, api.NewError(api.CodeForbidden, "this key may not use model %q", req.Model)
	}
	if vm, ok := s.Catalog.VirtualModels[req.Model]; ok {
		if err := e.route(s, rc, req, vm, p); err != nil {
			return p, err
		}
		return p, nil
	}
	d := s.Catalog.Lookup(req.Model)
	if d == nil || !d.Approved {
		return nil, api.NewError(api.CodeNotFound, "unknown model %q", req.Model)
	}
	if !d.Enabled {
		return nil, api.NewError(api.CodeNoEligibleModel, "model %q is disabled", req.Model)
	}
	if !rc.Principal.ProviderAllowed(d.Provider) {
		return nil, api.NewError(api.CodeForbidden, "this key may not use provider %q", d.Provider)
	}
	if p.PromptTokens > d.ContextWindow {
		return nil, api.NewError(api.CodeContextTooLong, "prompt of ~%d tokens exceeds %s's context window of %d", p.PromptTokens, d.ID, d.ContextWindow)
	}
	pol, src := resolvePolicy(s, rc, nil)
	p.Policy, p.PolicyName, p.PolicyVersion = pol, pol.Name, pol.Version
	estCost, estLat := e.estimate(d, rc.Start, p.PromptTokens, min(p.MaxOutput, pol.ExpectedOutput))
	p.Attempts = []*catalog.Deployment{d}
	p.Reason = "explicit_model"
	p.Decision = &Decision{RequestID: rc.ID, RequestedModel: req.Model, Policy: pol.Name, PolicyVersion: pol.Version,
		PolicySource: src, Objective: "explicit", PromptTokens: p.PromptTokens, MaxOutputTokens: p.MaxOutput,
		MinTier: d.Tier, MaxTier: d.Tier, Reason: "explicit_model", Ordered: []string{d.ID}, EstCostUSD: estCost,
		Candidates: []CandidateInfo{{Deployment: d.ID, Provider: d.Provider, Tier: d.Tier, EstCostUSD: estCost, EstLatencyMs: estLat}}}
	return p, nil
}

// Explain plans a request without calling any model and at no cost [RTE-15].
func (e *Engine) Explain(rc *RequestContext, req *api.ChatRequest) (*Decision, error) {
	s := e.Snapshot()
	rc.Explain = true
	p, err := e.plan(s, rc, req)
	if p != nil && p.Decision != nil {
		if len(p.Decision.Ordered) > 0 {
			p.Decision.Chosen = p.Decision.Ordered[0]
		}
		return p.Decision, err
	}
	return nil, err
}

func (e *Engine) timeouts(s *Snapshot, rc *RequestContext, d *catalog.Deployment) catalog.Timeouts {
	prov := s.Catalog.Providers[d.Provider]
	def := catalog.Timeouts{
		Connect: s.Config.Resilience.Timeouts.Connect.D(), TTFT: s.Config.Resilience.Timeouts.TTFT.D(),
		Idle: s.Config.Resilience.Timeouts.Idle.D(), Total: s.Config.Resilience.Timeouts.Total.D(),
	}
	var req catalog.Timeouts
	max := s.Config.Resilience.MaxTimeouts
	if rc.Hints.TimeoutMs != nil {
		req.Total = clampDur(time.Duration(*rc.Hints.TimeoutMs)*time.Millisecond, max.Total.D())
	}
	if rc.Hints.TTFTTimeoutMs != nil {
		req.TTFT = clampDur(time.Duration(*rc.Hints.TTFTTimeoutMs)*time.Millisecond, max.TTFT.D())
	}
	var pt catalog.Timeouts
	if prov != nil {
		pt = prov.Timeouts
	}
	return provider.MergeTimeouts(req, d.Timeouts, pt, def)
}

func clampDur(v, max time.Duration) time.Duration {
	if max > 0 && v > max {
		return max
	}
	return v
}

// usageFrom converts reported usage, or estimates it, for one completed attempt.
func usageFrom(req *api.ChatRequest, u *api.Usage, text string, calls []api.ToolCall) cost.Usage {
	if u != nil && (u.PromptTokens > 0 || u.CompletionTokens > 0) {
		return provider.UsageOf(u)
	}
	return cost.Usage{Input: tokens.Prompt(req), Output: capOutput(req, tokens.Completion(text, calls)), Estimated: true}
}

// capOutput bounds an estimated output count by the request's output cap: the model
// cannot have produced more, and budgets reserved exactly that much [BUD-03].
func capOutput(req *api.ChatRequest, n int) int {
	if c := req.MaxOutput(0); c > 0 && n > c {
		return c
	}
	return n
}

func responseText(r *api.ChatResponse) (string, []api.ToolCall) {
	if len(r.Choices) == 0 {
		return "", nil
	}
	return r.Choices[0].Message.Content.PlainText(), r.Choices[0].Message.ToolCalls
}

func (e *Engine) newMeta(rc *RequestContext, requested string) *Meta {
	return &Meta{RequestID: rc.ID, RequestedModel: requested, CacheStatus: "miss"}
}

func applyPlan(m *Meta, p *Plan) {
	m.Policy, m.PolicyVersion, m.RouteReason = p.PolicyName, p.PolicyVersion, p.Reason
	if p.Decision != nil {
		m.Difficulty = p.Decision.Difficulty
		m.Candidates = len(p.Decision.Candidates)
	} else {
		m.Candidates = len(p.Attempts)
	}
}

// policyFor resolves the policy a request will run under (cache keys need it before
// routing).
func (e *Engine) policyFor(s *Snapshot, rc *RequestContext, model string) *config.Policy {
	pol, _ := resolvePolicy(s, rc, s.Catalog.VirtualModels[model])
	return pol
}

// Chat serves a non-streaming chat completion.
func (e *Engine) Chat(ctx context.Context, rc *RequestContext, req *api.ChatRequest) (*api.ChatResponse, *Meta, error) {
	s := e.Snapshot()
	meta := e.newMeta(rc, req.Model)
	req.Model = resolveAlias(s, req.Model)
	if gerr := e.guardRequest(ctx, s, rc, req); gerr != nil {
		return nil, e.failPlan(s, rc, meta, nil, gerr), gerr
	}
	if aerr := e.admit(ctx, s, rc, req, tokens.Prompt(req)); aerr != nil {
		return nil, e.failPlan(s, rc, meta, nil, aerr), aerr
	}
	pol := e.policyFor(s, rc, req.Model)
	ent, status, _, lk := e.lookupCache(ctx, s, rc, req, pol)
	meta.CacheStatus = status
	if ent != nil {
		// The lookup embedding is absorbed by the gateway: hits bill exactly the cache price.
		if resp, err := e.serveHit(s, rc, ent, status, meta); err == nil {
			e.finish(s, rc, meta, nil, 200, "")
			return resp, meta, nil
		}
	}
	rc.overheadUSD += lk.overheadUSD
	p, err := e.plan(s, rc, req)
	if err != nil {
		return nil, e.failPlan(s, rc, meta, p, err), err
	}
	applyPlan(meta, p)
	if perr := e.applyPII(s, rc, req, p, meta); perr != nil {
		return nil, e.failPlan(s, rc, meta, p, perr), perr
	}
	resp, valid, err := e.runChat(ctx, s, rc, req, p, meta)
	if err != nil {
		gerr := upstreamToAPI(err)
		return nil, e.failPlan(s, rc, meta, p, gerr), gerr
	}
	if post := s.Config.Guardrails.PostWebhook; post != "" {
		text, _ := responseText(resp)
		if werr := e.callWebhook(ctx, s, post, map[string]any{"stage": "post", "request_id": rc.ID, "tenant": rc.Principal.TenantID(),
			"model": meta.Deployment, "response": text}); werr != nil {
			return nil, e.failPlan(s, rc, meta, p, werr), werr
		}
	}
	if valid {
		e.storeCache(ctx, s, rc, lk, resp, meta)
	}
	e.logBody(ctx, s, rc, req, resp)
	e.finish(s, rc, meta, p, 200, "")
	e.maybeShadow(rc, req, p.Policy)
	e.maybeGradeOnline(s, rc, req, resp, meta)
	return resp, meta, nil
}

// runChat executes the plan and, under a cascade policy, validates each answer and
// escalates to a stronger tier on failure [RTE-06]. Every attempt's cost is billed:
// failed cascade attempts are overhead on the same request [CST-01].
func (e *Engine) runChat(ctx context.Context, s *Snapshot, rc *RequestContext, req *api.ChatRequest, p *Plan, meta *Meta) (*api.ChatResponse, bool, error) {
	cascade := p.Decision != nil && p.Decision.Cascade && p.Policy != nil
	attempts := p.Attempts
	escalations := 0
	for {
		var resp *api.ChatResponse
		sub := *p
		sub.Attempts = attempts
		h, err := e.execute(ctx, s, rc, &sub, meta, func(ctx context.Context, d *catalog.Deployment) error {
			var err error
			resp, err = s.Providers.Get(d.Provider).Chat(ctx, d, requestFor(p, d, req), e.timeouts(s, rc, d))
			return err
		})
		if err != nil {
			return nil, false, err
		}
		h.release()
		d := h.d
		elapsed := msSince(e.Now, h.start)
		e.Health.Record(d.ID, true, elapsed, "")
		e.Latency.Observe(d.ID, elapsed, 0, 0)
		text, calls := responseText(resp)
		u := usageFrom(req, resp.Usage, text, calls)
		bd := cost.Of(d, rc.Start, u)
		last := &meta.Attempts[len(meta.Attempts)-1]
		last.CostUSD, last.LatencyMs = bd.Total(), elapsed
		accept := func(valid bool) (*api.ChatResponse, bool, error) {
			e.Pins.Set(rc, d.ID)
			meta.Deployment, meta.Provider = d.ID, d.Provider
			meta.Usage, meta.Cost = u, bd
			meta.UpstreamMs, meta.TTFTMs = elapsed, elapsed
			resp.Model = d.ID
			return resp, valid, nil
		}
		if !cascade {
			return accept(true)
		}
		var judgeCost float64
		reason := e.validate(ctx, s, rc, p.Policy, req, resp, &judgeCost)
		rc.overheadUSD += judgeCost
		if reason == "" {
			return accept(true)
		}
		last.Kind, last.Status, last.Error = "cascade", "invalid", "validation:"+reason
		var next []*catalog.Deployment
		for _, a := range attempts {
			if a.Tier > d.Tier {
				next = append(next, a)
			}
		}
		if escalations >= p.Policy.Cascade.MaxEscalations || len(next) == 0 {
			meta.RouteReason = "cascade_exhausted"
			return accept(false)
		}
		escalations++
		rc.overheadUSD += bd.Total()
		meta.RouteReason = "cascade_escalated"
		p.Reason = meta.RouteReason
		if p.Decision != nil {
			p.Decision.Reason = meta.RouteReason
		}
		attempts = next
	}
}

// upstreamToAPI maps the final upstream failure to a client error.
func upstreamToAPI(err error) *api.Error {
	if err == nil {
		return api.NewError(api.CodeUpstreamUnavailable, "no deployment could serve the request")
	}
	var ge *api.Error
	if errors.As(err, &ge) {
		return ge
	}
	ue := provider.AsUpstream(err)
	switch ue.Kind {
	case provider.KindContextLength:
		return api.NewError(api.CodeContextTooLong, "%s", ue.Message)
	case provider.KindClient:
		return api.NewError(api.CodeInvalidRequest, "upstream rejected the request: %s", ue.Message)
	case provider.KindCanceled:
		e := api.NewError(api.CodeInvalidRequest, "request canceled by client")
		e.Status = 499
		return e
	}
	e := api.NewError(api.CodeUpstreamUnavailable, "upstream failed: %s", ue.Message)
	if ue.RetryAfter > 0 {
		e.RetryAfter = ue.RetryAfter
	}
	return e
}

func msSince(now func() time.Time, t time.Time) float64 {
	return float64(now().Sub(t).Microseconds()) / 1000
}

// fail records a failed request and returns the meta to attach to the error response.
func (e *Engine) fail(s *Snapshot, rc *RequestContext, meta *Meta, err error) *Meta {
	return e.failPlan(s, rc, meta, nil, err)
}

// failPlan is fail with the (possibly partial) plan, so the decision record — including
// exclusion reasons — is stored for failed routes too [RTE-02, RTE-14].
func (e *Engine) failPlan(s *Snapshot, rc *RequestContext, meta *Meta, p *Plan, err error) *Meta {
	if p != nil && p.Decision != nil {
		applyPlan(meta, p)
	}
	var ge *api.Error
	if !errors.As(err, &ge) {
		ge = api.NewError(api.CodeInternal, "%v", err)
	}
	status := ge.Status
	if status == 0 {
		status = api.StatusFor(ge.Code)
	}
	meta.ErrorCode = ge.Code
	e.finish(s, rc, meta, p, status, ge.Code)
	return meta
}

// baseline returns the deployment this request is compared against for savings [CST-03].
func (e *Engine) baseline(s *Snapshot, rc *RequestContext, p *Plan) *catalog.Deployment {
	candidates := []string{rc.Principal.Scopes.Baseline}
	if rc.Principal.Project != nil {
		candidates = append(candidates, rc.Principal.Project.Baseline)
	}
	if rc.Principal.Tenant != nil {
		candidates = append(candidates, rc.Principal.Tenant.Baseline)
	}
	if p != nil && p.Policy != nil {
		candidates = append(candidates, p.Policy.Baseline)
	}
	candidates = append(candidates, s.Config.Defaults.Baseline)
	for _, id := range candidates {
		if id != "" {
			if d := s.Catalog.Deployments[id]; d != nil {
				return d
			}
		}
	}
	return nil
}

// finish writes the cost record, decision record and request event.
func (e *Engine) finish(s *Snapshot, rc *RequestContext, meta *Meta, p *Plan, status int, code string) {
	meta.Status = status
	meta.ErrorCode = code
	meta.LatencyMs = msSince(e.Now, rc.Start)
	if bd := e.baseline(s, rc, p); bd != nil && status < 400 {
		// The baseline sees the same token counts the routed model used.
		meta.BaselineUSD = cost.Of(bd, rc.Start, meta.Usage).Total()
		if meta.CacheStatus == "exact_hit" || meta.CacheStatus == "semantic_hit" {
			meta.BaselineUSD = meta.CacheSavedUSD + meta.CostUSD
		}
	} else if status < 400 {
		meta.BaselineUSD = meta.CostUSD + meta.CacheSavedUSD
	}
	if rc.Explain {
		return
	}
	ctx := context.Background()
	// CostUSD is the request's total: the served attempt plus overhead (failed cascade
	// attempts, classifier, judge, embeddings) [CST-01]. Cache hits bill the cache price.
	if meta.CacheStatus != CacheExactHit && meta.CacheStatus != CacheSemanticHit {
		meta.OverheadUSD = rc.overheadUSD
		meta.CostUSD = meta.Cost.Total() + meta.OverheadUSD
	}
	rc.reservation.Settle(ctx, meta.CostUSD)
	rc.admission.Release(ctx, meta.Usage.TotalInput(), meta.Usage.TotalOutput())
	if len(rc.budgets) > 0 {
		meta.BudgetRemainingUSD = e.Gov.Remaining(ctx, rc.budgets)
	}
	for k, v := range rc.admissionHeaders() {
		if meta.RateHeaders == nil {
			meta.RateHeaders = map[string]string{}
		}
		meta.RateHeaders[k] = v
	}
	pr := rc.Principal
	rec := store.CostRecord{
		RequestID: rc.ID, Time: rc.Start.UTC(), Tenant: pr.TenantID(), Project: pr.ProjectID(), KeyID: pr.KeyID,
		EndUser: rc.EndUser(), Tags: rc.Tags(), Endpoint: rc.Endpoint, RequestedModel: meta.RequestedModel,
		Deployment: meta.Deployment, Provider: meta.Provider, Policy: meta.Policy, PolicyVersion: meta.PolicyVersion,
		RouteReason: meta.RouteReason, CacheStatus: meta.CacheStatus,
		InputTokens: meta.Usage.Input, CachedTokens: meta.Usage.CachedInput, OutputTokens: meta.Usage.Output, ReasoningToks: meta.Usage.Reasoning,
		CostUSD: meta.CostUSD, InputUSD: meta.Cost.Input, CachedInputUSD: meta.Cost.CachedInput, OutputUSD: meta.Cost.Output, ReasoningUSD: meta.Cost.Reasoning,
		OverheadUSD: meta.OverheadUSD, BaselineUSD: meta.BaselineUSD, CacheSavedUSD: meta.CacheSavedUSD,
		Estimated: meta.Usage.Estimated, Attempts: len(meta.Attempts), Status: status, ErrorCode: code,
		LatencyMs: meta.LatencyMs, TTFTMs: meta.TTFTMs, Shadow: rc.Shadow, EvalRun: rc.EvalRun,
	}
	e.Writer.Cost(rec)
	if p != nil && p.Decision != nil {
		p.Decision.Attempts = meta.Attempts
		p.Decision.Chosen = meta.Deployment
		e.Writer.Decision(store.DecisionRecord{RequestID: rc.ID, Time: rc.Start.UTC(), Tenant: pr.TenantID(), Body: p.Decision.JSON()})
	}
	overhead := meta.LatencyMs - meta.UpstreamMs
	if overhead < 0 {
		overhead = 0
	}
	ev := observe.Event{
		Time: rc.Start.UTC(), RequestID: rc.ID, Endpoint: rc.Endpoint, Format: formatName(rc.Format),
		Tenant: pr.TenantID(), Project: pr.ProjectID(), KeyPrefix: pr.KeyPrefix, EndUser: rc.EndUser(), Tags: rc.Tags(),
		RequestedModel: meta.RequestedModel, ServedModel: meta.Deployment, Provider: meta.Provider,
		Policy: meta.Policy, PolicyVersion: meta.PolicyVersion, RouteReason: meta.RouteReason,
		Candidates: meta.Candidates, Difficulty: meta.Difficulty, CacheStatus: meta.CacheStatus, Attempts: meta.Attempts,
		InputTokens: meta.Usage.TotalInput(), CachedTokens: meta.Usage.CachedInput, OutputTokens: meta.Usage.TotalOutput(),
		ReasoningToks: meta.Usage.Reasoning, UsageEstimated: meta.Usage.Estimated, CostUSD: meta.CostUSD,
		BaselineUSD: meta.BaselineUSD, CacheSavedUSD: meta.CacheSavedUSD, TTFTMs: meta.TTFTMs, LatencyMs: meta.LatencyMs,
		OverheadMs: overhead, Status: status, ErrorCode: code, Shadow: rc.Shadow, TraceID: rc.TraceID, PII: meta.PII,
		Stream: rc.Endpoint != "embeddings" && meta.TTFTMs > 0 && meta.TTFTMs < meta.UpstreamMs,
	}
	if meta.UpstreamMs > meta.TTFTMs && meta.Usage.TotalOutput() > 0 {
		ev.TokensPerSec = float64(meta.Usage.TotalOutput()) / ((meta.UpstreamMs - meta.TTFTMs) / 1000)
	}
	e.Events.Emit(ev)
}

func formatName(f api.Format) string {
	if f == api.FormatAnthropic {
		return "anthropic"
	}
	return "openai"
}

// ---------- streaming ----------

// ChatStream is an open streaming response. The engine has already received the first
// content-bearing chunk, so fallback is no longer possible once the caller has it
// [RES-04].
type ChatStream struct {
	e        *Engine
	s        *Snapshot
	rc       *RequestContext
	req      *api.ChatRequest
	plan     *Plan
	dep      *catalog.Deployment
	up       provider.Stream
	Meta     *Meta
	start    time.Time
	buf      []*api.ChatChunk
	text     strings.Builder
	calls    map[int]*api.ToolCall
	usage    *api.Usage
	finish   string
	done     bool
	closed   bool
	FailErr  error
	release  func()
	lookup   *cacheLookup
	replayed bool
}

// FinishReason returns the upstream finish reason seen so far.
func (cs *ChatStream) FinishReason() string { return cs.finish }

func isContent(ch *api.ChatChunk) bool {
	for _, c := range ch.Choices {
		if (c.Delta.Content != nil && *c.Delta.Content != "") || len(c.Delta.ToolCalls) > 0 || c.FinishReason != nil ||
			(c.Delta.ReasoningContent != nil && *c.Delta.ReasoningContent != "") || (c.Delta.Refusal != nil && *c.Delta.Refusal != "") {
			return true
		}
	}
	return false
}

// open starts the upstream stream on d and buffers chunks up to the first content.
func (cs *ChatStream) open(ctx context.Context, d *catalog.Deployment) error {
	up, err := cs.s.Providers.Get(d.Provider).ChatStream(ctx, d, requestFor(cs.plan, d, cs.req), cs.e.timeouts(cs.s, cs.rc, d))
	if err != nil {
		return err
	}
	cs.up, cs.dep = up, d
	cs.buf, cs.text, cs.calls, cs.usage, cs.finish = nil, strings.Builder{}, map[int]*api.ToolCall{}, nil, ""
	for {
		ch, err := up.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			up.Close()
			return err
		}
		if cs.absorb(ch) {
			cs.buf = append(cs.buf, ch)
		}
		if isContent(ch) {
			cs.Meta.TTFTMs = msSince(cs.e.Now, cs.start)
			return nil
		}
	}
}

// absorb tracks text, tool calls and usage; it returns false for usage-only chunks,
// which the edge re-emits itself [API-13].
func (cs *ChatStream) absorb(ch *api.ChatChunk) bool {
	if ch.Usage != nil {
		cs.usage = ch.Usage
	}
	if len(ch.Choices) == 0 {
		return false
	}
	ch.Model = cs.dep.ID
	for _, c := range ch.Choices {
		if c.Delta.Content != nil {
			cs.text.WriteString(*c.Delta.Content)
		}
		for _, tc := range c.Delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			cur, ok := cs.calls[idx]
			if !ok {
				cur = &api.ToolCall{}
				cs.calls[idx] = cur
			}
			if tc.Function.Name != "" {
				cur.Function.Name = tc.Function.Name
			}
			cur.Function.Arguments += tc.Function.Arguments
		}
		if c.FinishReason != nil {
			cs.finish = *c.FinishReason
		}
	}
	return true
}

// ChatStream opens a streaming chat completion, falling back across the plan until a
// deployment produces its first token. Cache hits and buffered cascades are replayed as
// a stream [CCH-05, RTE-06].
func (e *Engine) ChatStream(ctx context.Context, rc *RequestContext, req *api.ChatRequest) (*ChatStream, error) {
	s := e.Snapshot()
	meta := e.newMeta(rc, req.Model)
	req.Model = resolveAlias(s, req.Model)
	if gerr := e.guardRequest(ctx, s, rc, req); gerr != nil {
		e.failPlan(s, rc, meta, nil, gerr)
		return &ChatStream{Meta: meta}, gerr
	}
	if aerr := e.admit(ctx, s, rc, req, tokens.Prompt(req)); aerr != nil {
		e.failPlan(s, rc, meta, nil, aerr)
		return &ChatStream{Meta: meta}, aerr
	}
	pol := e.policyFor(s, rc, req.Model)
	ent, status, _, lk := e.lookupCache(ctx, s, rc, req, pol)
	meta.CacheStatus = status
	cs := &ChatStream{e: e, s: s, rc: rc, req: req, Meta: meta, lookup: lk}
	if ent != nil {
		if resp, err := e.serveHit(s, rc, ent, status, meta); err == nil {
			cs.replay(resp, ent.Deployment, nil)
			return cs, nil
		}
	}
	rc.overheadUSD += lk.overheadUSD
	p, err := e.plan(s, rc, req)
	if err != nil {
		e.failPlan(s, rc, meta, p, err)
		return &ChatStream{Meta: meta}, err
	}
	applyPlan(meta, p)
	cs.plan = p
	if perr := e.applyPII(s, rc, req, p, meta); perr != nil {
		e.failPlan(s, rc, meta, p, perr)
		return &ChatStream{Meta: meta}, perr
	}
	if p.Decision != nil && p.Decision.Cascade {
		// Buffered cascade: produce and validate the whole answer, then stream it.
		resp, valid, err := e.runChat(ctx, s, rc, req, p, meta)
		if err != nil {
			gerr := upstreamToAPI(err)
			e.failPlan(s, rc, meta, p, gerr)
			return &ChatStream{Meta: meta}, gerr
		}
		if valid {
			e.storeCache(ctx, s, rc, lk, resp, meta)
		}
		cs.replay(resp, meta.Deployment, p)
		return cs, nil
	}
	h, err := e.execute(ctx, s, rc, p, meta, func(ctx context.Context, d *catalog.Deployment) error {
		cs.start = e.Now()
		return cs.open(ctx, d)
	})
	if err != nil {
		gerr := upstreamToAPI(err)
		e.failPlan(s, rc, meta, p, gerr)
		return &ChatStream{Meta: meta}, gerr
	}
	cs.release = h.release
	meta.Deployment, meta.Provider = h.d.ID, h.d.Provider
	return cs, nil
}

// replay turns a complete response into the stream's buffer; billing is already in Meta.
func (cs *ChatStream) replay(resp *api.ChatResponse, model string, p *Plan) {
	cs.buf = replayChunks(resp, model)
	cs.replayed = true
	cs.plan = p
}

// Recv returns the next chunk for the client; io.EOF at the end. After an upstream
// failure it returns the error once; Close then bills what was delivered.
func (cs *ChatStream) Recv() (*api.ChatChunk, error) {
	if len(cs.buf) > 0 {
		ch := cs.buf[0]
		cs.buf = cs.buf[1:]
		return ch, nil
	}
	if cs.done || cs.up == nil {
		return nil, io.EOF
	}
	for {
		ch, err := cs.up.Recv()
		if err == io.EOF {
			cs.done = true
			return nil, io.EOF
		}
		if err != nil {
			cs.done = true
			cs.FailErr = err
			return nil, err
		}
		if cs.absorb(ch) {
			return ch, nil
		}
	}
}

// Usage returns the usage to report for the stream: upstream-reported when present,
// otherwise counted locally from what was delivered and flagged estimated [API-11, API-13].
func (cs *ChatStream) Usage() cost.Usage {
	calls := make([]api.ToolCall, 0, len(cs.calls))
	for _, c := range cs.calls {
		calls = append(calls, *c)
	}
	if cs.FailErr == nil && cs.usage != nil {
		return provider.UsageOf(cs.usage)
	}
	u := cost.Usage{Input: tokens.Prompt(cs.req), Output: capOutput(cs.req, tokens.Completion(cs.text.String(), calls)), Estimated: true}
	if cs.usage != nil && cs.usage.PromptTokens > 0 {
		u.Input = cs.usage.PromptTokens - cs.usage.Cached()
		u.CachedInput = cs.usage.Cached()
	}
	return u
}

// Close finishes billing and records the request. clientErr is non-nil when the client
// went away before the end.
func (cs *ChatStream) Close(clientErr error) *Meta {
	if cs.closed {
		return cs.Meta
	}
	if cs.replayed {
		cs.closed = true
		status, code := 200, ""
		if clientErr != nil {
			status, code = 499, "client_closed"
		}
		cs.e.finish(cs.s, cs.rc, cs.Meta, cs.plan, status, code)
		return cs.Meta
	}
	if cs.up == nil {
		return cs.Meta
	}
	cs.closed = true
	cs.up.Close()
	if cs.release != nil {
		cs.release()
	}
	m := cs.Meta
	m.Usage = cs.Usage()
	m.Cost = cost.Of(cs.dep, cs.rc.Start, m.Usage)
	m.UpstreamMs = msSince(cs.e.Now, cs.start)
	switch {
	case clientErr == nil && cs.FailErr == nil:
		cs.e.Latency.Observe(cs.dep.ID, m.TTFTMs, m.Usage.TotalOutput(), m.UpstreamMs-m.TTFTMs)
		cs.e.Health.Record(cs.dep.ID, true, m.TTFTMs, "")
		cs.e.Pins.Set(cs.rc, cs.dep.ID)
	case cs.FailErr != nil:
		ue := provider.AsUpstream(cs.FailErr)
		cs.e.Health.Record(cs.dep.ID, !ue.CountsAgainstHealth(), m.UpstreamMs, "mid-stream: "+ue.Message)
	default:
		cs.e.Health.Release(cs.dep.ID)
	}
	last := &m.Attempts[len(m.Attempts)-1]
	last.CostUSD, last.LatencyMs, last.TTFTMs = m.Cost.Total(), m.UpstreamMs, m.TTFTMs
	if clientErr == nil && cs.FailErr == nil {
		asm := cs.assembled()
		if cs.finish != "length" {
			cs.e.storeCache(context.Background(), cs.s, cs.rc, cs.lookup, asm, m)
		}
		cs.e.logBody(context.Background(), cs.s, cs.rc, cs.req, asm)
		if cs.plan != nil {
			defer cs.e.maybeShadow(cs.rc, cs.req, cs.plan.Policy)
		}
	}
	status, code := 200, ""
	switch {
	case clientErr != nil:
		status, code = 499, "client_closed"
		last.Status, last.Error = "client_closed", "client disconnected"
	case cs.FailErr != nil:
		status, code = 502, api.CodeUpstreamUnavailable
		last.Status, last.Error = "error", string(provider.AsUpstream(cs.FailErr).Kind)
	}
	cs.e.finish(cs.s, cs.rc, m, cs.plan, status, code)
	return m
}

// assembled rebuilds a complete response from a finished stream (for the cache).
func (cs *ChatStream) assembled() *api.ChatResponse {
	msg := api.Message{Role: "assistant", Content: api.TextContent(cs.text.String())}
	for i := 0; i < len(cs.calls); i++ {
		if c, ok := cs.calls[i]; ok {
			msg.ToolCalls = append(msg.ToolCalls, *c)
		}
	}
	u := cs.Usage()
	return &api.ChatResponse{ID: cs.rc.ID, Object: "chat.completion", Created: cs.rc.Start.Unix(), Model: cs.dep.ID,
		Choices: []api.Choice{{Message: msg, FinishReason: cs.finish}},
		Usage:   &api.Usage{PromptTokens: u.TotalInput(), CompletionTokens: u.TotalOutput(), TotalTokens: u.TotalInput() + u.TotalOutput()}}
}

// ---------- embeddings ----------

func (e *Engine) Embeddings(ctx context.Context, rc *RequestContext, req *api.EmbeddingsRequest) (*api.EmbeddingsResponse, *Meta, error) {
	s := e.Snapshot()
	meta := e.newMeta(rc, req.Model)
	meta.CacheStatus = "bypass"
	d, perr := e.planEmbeddings(s, rc, req)
	if perr != nil {
		return nil, e.fail(s, rc, meta, perr), perr
	}
	inputs, _ := req.Inputs()
	est := 0
	for _, in := range inputs {
		est += tokens.Count(in)
	}
	if aerr := e.admit(ctx, s, rc, nil, est); aerr != nil {
		return nil, e.fail(s, rc, meta, aerr), aerr
	}
	if berr := rc.reservation.Grow(ctx, cost.WorstCase(d, rc.Start, int(float64(est)*e.Gov.InputSafety), 0)); berr != nil {
		return nil, e.fail(s, rc, meta, berr), berr
	}
	meta.RouteReason = "explicit_model"
	if _, vm := s.Catalog.VirtualModels[req.Model]; vm {
		meta.RouteReason = "cheapest_embeddings"
	}
	start := e.Now()
	resp, err := s.Providers.Get(d.Provider).Embeddings(ctx, d, req, e.timeouts(s, rc, d))
	meta.UpstreamMs = msSince(e.Now, start)
	if err != nil {
		ue := provider.AsUpstream(err)
		meta.Attempts = append(meta.Attempts, observe.Attempt{Deployment: d.ID, Provider: d.Provider, Kind: "primary", Status: "error", Error: string(ue.Kind)})
		gerr := upstreamToAPI(err)
		return nil, e.fail(s, rc, meta, gerr), gerr
	}
	var u cost.Usage
	if resp.Usage != nil && resp.Usage.PromptTokens > 0 {
		u.Input = resp.Usage.PromptTokens
	} else {
		inputs, _ := req.Inputs()
		for _, in := range inputs {
			u.Input += tokens.Count(in)
		}
		u.Estimated = true
	}
	meta.Deployment, meta.Provider, meta.Usage = d.ID, d.Provider, u
	meta.Cost = cost.Of(d, rc.Start, u)
	meta.CostUSD = meta.Cost.Total()
	meta.Attempts = append(meta.Attempts, observe.Attempt{Deployment: d.ID, Provider: d.Provider, Kind: "primary", Status: "ok", CostUSD: meta.CostUSD, LatencyMs: meta.UpstreamMs})
	resp.Model = d.ID
	e.finish(s, rc, meta, nil, 200, "")
	return resp, meta, nil
}

func (e *Engine) planEmbeddings(s *Snapshot, rc *RequestContext, req *api.EmbeddingsRequest) (*catalog.Deployment, error) {
	if req.Model == "" {
		return nil, api.NewError(api.CodeInvalidRequest, "model is required")
	}
	if !rc.Principal.ModelAllowed(req.Model) {
		return nil, api.NewError(api.CodeForbidden, "this key may not use model %q", req.Model)
	}
	if _, ok := s.Catalog.VirtualModels[req.Model]; ok {
		var best *catalog.Deployment
		for _, d := range s.Catalog.Usable() {
			if d.Has(catalog.CapEmbeddings) && rc.Principal.ProviderAllowed(d.Provider) &&
				(best == nil || d.PriceAt(rc.Start).Input < best.PriceAt(rc.Start).Input) {
				best = d
			}
		}
		if best == nil {
			return nil, api.NewError(api.CodeNoEligibleModel, "no embeddings deployment is available")
		}
		return best, nil
	}
	d := s.Catalog.Lookup(req.Model)
	if d == nil || !d.Approved {
		return nil, api.NewError(api.CodeNotFound, "unknown model %q", req.Model)
	}
	if !d.Has(catalog.CapEmbeddings) {
		return nil, api.NewError(api.CodeInvalidRequest, "model %q does not support embeddings", req.Model)
	}
	if !d.Enabled {
		return nil, api.NewError(api.CodeNoEligibleModel, "model %q is disabled", req.Model)
	}
	return d, nil
}

// Models lists the virtual and concrete models the principal may use [API-04].
func (e *Engine) Models(rc *RequestContext) *api.ModelList {
	s := e.Snapshot()
	list := &api.ModelList{Object: "list"}
	created := s.Loaded.Unix()
	var names []string
	for name := range s.Catalog.VirtualModels {
		names = append(names, name)
	}
	sortStrings(names)
	for _, n := range names {
		if rc.Principal.ModelAllowed(n) {
			list.Data = append(list.Data, api.ModelObject{ID: n, Object: "model", Created: created, OwnedBy: "gateway"})
		}
	}
	for _, d := range s.Catalog.Usable() {
		if rc.Principal.ModelAllowed(d.ID) && rc.Principal.ProviderAllowed(d.Provider) {
			list.Data = append(list.Data, api.ModelObject{ID: d.ID, Object: "model", Created: created, OwnedBy: d.Provider})
		}
	}
	return list
}
