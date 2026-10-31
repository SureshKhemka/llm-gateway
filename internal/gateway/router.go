package gateway

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/catalog"
	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/cost"
)

// RouteHooks connect routing to live state owned by other subsystems.
type RouteHooks struct {
	// Health returns the deployment's health state and whether it may receive traffic
	// (circuit closed or half-open probe allowed) [RES-01, RES-02].
	Health func(dep string) (state string, allow bool)
	// BudgetUtilisation returns the highest utilisation (0–100) among the hard budgets
	// that apply to the request [RTE-08].
	BudgetUtilisation func(rc *RequestContext) float64
	// Pinned returns the deployment a session is pinned to [RTE-11].
	Pinned func(rc *RequestContext) string
	// InFlight returns current in-flight requests per deployment (pool balancing).
	InFlight func(dep string) int
	// Cold reports a local deployment whose model is not loaded [PRV-10].
	Cold func(dep string) bool
}

// resolvePolicy applies precedence [RTE-09]: for the generic virtual model "auto" the
// key's policy wins, then the project's, the tenant's, the virtual model's, then the
// global default. Other virtual models (auto-fast, auto-cheap, operator-defined) carry
// their own policy; scope-level hard limits still apply to them as constraints.
func resolvePolicy(s *Snapshot, rc *RequestContext, vm *catalog.VirtualModel) (*config.Policy, string) {
	type cand struct{ name, src string }
	var order []cand
	generic := vm == nil || vm.Name == "auto" || vm.Policy == ""
	if generic {
		order = append(order, cand{rc.Principal.Scopes.Policy, "key"})
		if rc.Principal.Project != nil {
			order = append(order, cand{rc.Principal.Project.Policy, "project"})
		}
		if rc.Principal.Tenant != nil {
			order = append(order, cand{rc.Principal.Tenant.Policy, "tenant"})
		}
	}
	if vm != nil {
		order = append(order, cand{vm.Policy, "virtual_model"})
	}
	order = append(order, cand{s.Config.Defaults.Policy, "default"})
	for _, c := range order {
		if c.name == "" {
			continue
		}
		if p, ok := s.Policies[c.name]; ok {
			return p, c.src
		}
	}
	return s.Policies["default"], "builtin"
}

// constraints are the effective hard limits after hints narrow the policy [RTE-10].
type constraints struct {
	maxCost       *float64
	latencyTarget *float64
	minTier       int
	maxTier       int
	allowProv     []string // nil = any
	denyProv      map[string]bool
	residency     []string // any-of; nil = any
	required      []catalog.Capability
}

func minPtr(vals ...*float64) *float64 {
	var out *float64
	for _, v := range vals {
		if v != nil && (out == nil || *v < *out) {
			x := *v
			out = &x
		}
	}
	return out
}

// intersect returns the values present in every non-nil list; nil means unrestricted.
func intersect(lists ...[]string) []string {
	var out []string
	set := false
	for _, l := range lists {
		if len(l) == 0 {
			continue
		}
		if !set {
			out = append([]string(nil), l...)
			set = true
			continue
		}
		keep := map[string]bool{}
		for _, v := range l {
			keep[v] = true
		}
		var next []string
		for _, v := range out {
			if keep[v] {
				next = append(next, v)
			}
		}
		out = next
		if out == nil {
			out = []string{} // empty but restricted: nothing allowed
		}
	}
	if set && out == nil {
		return []string{}
	}
	return out
}

func buildConstraints(s *Snapshot, rc *RequestContext, req *api.ChatRequest, p *config.Policy, residency []string) constraints {
	h := rc.Hints
	c := constraints{maxTier: 3, denyProv: map[string]bool{}}
	c.maxCost = minPtr(p.MaxCostUSD, rc.Principal.Scopes.MaxCostUSD, h.MaxCostUSD)
	c.latencyTarget = minPtr(p.LatencyTargetMs, h.LatencyTargetMs)
	if p.MinTier != nil {
		c.minTier = *p.MinTier
	}
	if h.MinTier != nil && *h.MinTier > c.minTier {
		c.minTier = *h.MinTier
	}
	if p.MaxTier != nil {
		c.maxTier = *p.MaxTier
	}
	c.allowProv = intersect(p.AllowedProviders, rc.Principal.Scopes.Providers, h.AllowProviders)
	for _, d := range append(append([]string(nil), p.DeniedProviders...), h.DenyProviders...) {
		c.denyProv[d] = true
	}
	c.residency = intersect(p.Residency, residency)
	c.required = []catalog.Capability{catalog.CapChat}
	if len(req.Tools) > 0 && string(req.ToolChoice) != `"none"` {
		c.required = append(c.required, catalog.CapTools)
	}
	if req.HasImages() {
		c.required = append(c.required, catalog.CapVision)
	}
	if req.WantsJSONSchema() {
		c.required = append(c.required, catalog.CapJSONSchema)
	}
	for _, cap := range h.Capabilities {
		if config.KnownCapabilities[cap] {
			c.required = append(c.required, catalog.Capability(cap))
		}
	}
	return c
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// estimate returns the expected cost and latency of serving the request on d,
// using observed latency when available [RTE-07].
func (e *Engine) estimate(d *catalog.Deployment, at time.Time, promptTokens, outTokens int) (float64, float64) {
	ttft, tps := d.TTFTMs, d.TokensPerSec
	if e.Latency != nil {
		if ot, otps, ok := e.Latency.Observed(d.ID); ok {
			ttft, tps = ot, otps
		}
	}
	if tps <= 0 {
		tps = 1
	}
	lat := ttft + float64(outTokens)/tps*1000
	return cost.Estimate(d, at, promptTokens, 0, outTokens), lat
}

type ranked struct {
	d    *catalog.Deployment
	info CandidateInfo
}

// route plans a virtual-model request [RTE-01..RTE-16].
func (e *Engine) route(s *Snapshot, rc *RequestContext, req *api.ChatRequest, vm *catalog.VirtualModel, p *Plan) error {
	t0 := time.Now()
	pol, src := resolvePolicy(s, rc, vm)
	p.Policy, p.PolicyName, p.PolicyVersion = pol, pol.Name, pol.Version
	dec := &Decision{RequestID: rc.ID, RequestedModel: req.Model, Policy: pol.Name, PolicyVersion: pol.Version,
		PolicySource: src, Objective: pol.Objective, PromptTokens: p.PromptTokens, MaxOutputTokens: p.MaxOutput,
		Cascade: pol.Cascade.Enabled && (!req.Stream || pol.Cascade.BufferStreams)}
	p.Decision = dec

	c := buildConstraints(s, rc, req, pol, e.residencyFor(s, rc))
	for _, cap := range c.required {
		dec.Required = append(dec.Required, string(cap))
	}

	// Difficulty → tier floor [RTE-03].
	diff := e.difficulty(s, rc, req, pol, dec)
	dec.Difficulty, dec.DifficultySignals, dec.Estimator = diff.Score, diff.Signals, pol.Difficulty.Estimator
	floor := max(diff.Tier, c.minTier)

	// Budget-driven degradation [RTE-08]: above a threshold the tier is capped. The cap
	// is a strong preference, not a hard constraint — a request only a pricier
	// deployment can serve (vision, long context) is still served.
	degradeCap := c.maxTier
	if e.Hooks.BudgetUtilisation != nil && len(pol.Degrade) > 0 {
		util := e.Hooks.BudgetUtilisation(rc)
		steps := append([]config.DegradeStep(nil), pol.Degrade...)
		sort.Slice(steps, func(i, j int) bool { return steps[i].AtPct < steps[j].AtPct })
		for _, st := range steps {
			if util >= st.AtPct && st.MaxTier < degradeCap {
				degradeCap = st.MaxTier
				dec.Degraded = fmt.Sprintf("budget %.0f%% ≥ %.0f%%: tier capped at %d", util, st.AtPct, st.MaxTier)
			}
		}
		if dec.Degraded != "" && floor > degradeCap {
			floor = max(c.minTier, degradeCap)
		}
	}
	dec.MinTier, dec.MaxTier = floor, degradeCap

	outTokens := pol.ExpectedOutput
	explicitMax := req.MaxOutput(0)
	if explicitMax > 0 && explicitMax < outTokens {
		outTokens = explicitMax
	}

	var pool []*catalog.Deployment
	if vm != nil && len(vm.Deployments) > 0 {
		for _, id := range vm.Deployments {
			if d := s.Catalog.Deployments[id]; d != nil {
				pool = append(pool, d)
			}
		}
	} else {
		for _, id := range s.Catalog.Order {
			pool = append(pool, s.Catalog.Deployments[id])
		}
	}

	var meeting, below, overCap []ranked
	for _, d := range pool {
		var reasons []string
		if !d.Enabled {
			reasons = append(reasons, "disabled")
		}
		if !d.Approved {
			reasons = append(reasons, "unregistered")
		}
		for _, cap := range c.required {
			if !d.Has(cap) {
				reasons = append(reasons, "missing_capability:"+string(cap))
			}
		}
		if d.Has(catalog.CapEmbeddings) && !d.Has(catalog.CapChat) {
			reasons = reasons[:0]
			reasons = append(reasons, "embeddings_only")
		}
		if p.PromptTokens+outTokens > d.ContextWindow {
			reasons = append(reasons, fmt.Sprintf("context_window:%d<%d", d.ContextWindow, p.PromptTokens+outTokens))
		}
		if explicitMax > 0 && explicitMax > d.MaxOutputTokens {
			reasons = append(reasons, fmt.Sprintf("max_output_tokens:%d<%d", d.MaxOutputTokens, explicitMax))
		}
		if !rc.Principal.ModelAllowed(d.ID) {
			reasons = append(reasons, "not_allowed_for_key")
		}
		if c.allowProv != nil && !contains(c.allowProv, d.Provider) {
			reasons = append(reasons, "provider_not_allowed")
		}
		if c.denyProv[d.Provider] {
			reasons = append(reasons, "provider_denied")
		}
		if c.residency != nil {
			ok := false
			for _, r := range c.residency {
				if d.HasResidency(r) {
					ok = true
				}
			}
			if !ok {
				reasons = append(reasons, "residency")
			}
		}
		if d.Tier < c.minTier {
			reasons = append(reasons, fmt.Sprintf("tier_below_min:%d<%d", d.Tier, c.minTier))
		}
		if d.Tier > c.maxTier {
			reasons = append(reasons, fmt.Sprintf("tier_above_max:%d>%d", d.Tier, c.maxTier))
		}
		estCost, estLat := e.estimate(d, rc.Start, p.PromptTokens, outTokens)
		if c.maxCost != nil && estCost > *c.maxCost {
			reasons = append(reasons, fmt.Sprintf("over_max_cost:%.6f>%.6f", estCost, *c.maxCost))
		}
		health := "healthy"
		if e.Hooks.Health != nil {
			var allow bool
			health, allow = e.Hooks.Health(d.ID)
			if !allow {
				reasons = append(reasons, "circuit_open")
			}
		}
		if len(reasons) > 0 {
			dec.Exclusions = append(dec.Exclusions, Exclusion{Deployment: d.ID, Reasons: reasons})
			continue
		}
		r := ranked{d: d, info: CandidateInfo{Deployment: d.ID, Provider: d.Provider, Tier: d.Tier, EstCostUSD: estCost, EstLatencyMs: estLat, Health: health}}
		if d.Tier > degradeCap {
			overCap = append(overCap, r)
		} else if d.Tier >= floor {
			meeting = append(meeting, r)
		} else {
			below = append(below, r)
		}
	}
	if len(meeting)+len(below)+len(overCap) == 0 {
		err := api.NewError(api.CodeNoEligibleModel, "no deployment satisfies the request's constraints")
		err.Details = dec.Exclusions
		return err
	}

	// Latency-sensitive requests never go to a cold deployment while a warm eligible one
	// exists [PRV-10].
	if c.latencyTarget != nil && e.Hooks.Cold != nil {
		isCold := func(r ranked) bool { return !r.d.Warm || e.Hooks.Cold(r.d.ID) }
		warm := 0
		for _, r := range append(append([]ranked(nil), meeting...), below...) {
			if !isCold(r) {
				warm++
			}
		}
		if warm > 0 {
			filter := func(rs []ranked) []ranked {
				var out []ranked
				for _, r := range rs {
					if isCold(r) {
						dec.Exclusions = append(dec.Exclusions, Exclusion{Deployment: r.d.ID, Reasons: []string{"cold"}})
					} else {
						out = append(out, r)
					}
				}
				return out
			}
			meeting, below = filter(meeting), filter(below)
		}
	}
	// Latency target: drop candidates that miss it, unless none would remain [RTE-05].
	if c.latencyTarget != nil {
		var ok []ranked
		for _, r := range meeting {
			if r.info.EstLatencyMs <= *c.latencyTarget {
				ok = append(ok, r)
			}
		}
		if len(ok) > 0 && len(ok) < len(meeting) {
			for _, r := range meeting {
				if r.info.EstLatencyMs > *c.latencyTarget {
					dec.Exclusions = append(dec.Exclusions, Exclusion{Deployment: r.d.ID, Reasons: []string{fmt.Sprintf("over_latency_target:%.0f>%.0f", r.info.EstLatencyMs, *c.latencyTarget)}})
				}
			}
			meeting = ok
		}
	}

	e.rankByObjective(pol, meeting)
	// Below the difficulty tier: best tier first, kept as a last resort so a request is
	// served rather than failed when nothing at its tier is available [RES-08 safe:
	// difficulty is a preference, not a hard constraint].
	sort.SliceStable(below, func(i, j int) bool {
		if below[i].d.Tier != below[j].d.Tier {
			return below[i].d.Tier > below[j].d.Tier
		}
		return below[i].info.EstCostUSD < below[j].info.EstCostUSD
	})
	sort.SliceStable(overCap, func(i, j int) bool { return overCap[i].info.EstCostUSD < overCap[j].info.EstCostUSD })
	all := append(append(meeting, below...), overCap...)
	all = e.balancePools(all)

	reason := map[string]string{"min_cost": "cheapest_meeting_tier", "min_latency": "fastest_within_cost",
		"max_quality": "highest_tier_within_cost", "weighted": "best_weighted_score"}[pol.Objective]
	if len(meeting) == 0 && len(below) > 0 {
		reason = "below_difficulty_tier"
	}
	if dec.Degraded != "" {
		reason = "budget_degraded"
	}
	// Session pinning [RTE-11].
	if e.Hooks.Pinned != nil && rc.Hints.Session != "" {
		if pin := e.Hooks.Pinned(rc); pin != "" {
			for i, r := range all {
				if r.d.ID == pin && r.d.Tier >= floor {
					all = append([]ranked{r}, append(all[:i:i], all[i+1:]...)...)
					dec.Pinned = true
					reason = "session_pinned"
					break
				}
			}
		}
	}
	for _, r := range all {
		dec.Candidates = append(dec.Candidates, r.info)
		dec.Ordered = append(dec.Ordered, r.d.ID)
		p.Attempts = append(p.Attempts, r.d)
	}
	dec.Reason = reason
	dec.EstCostUSD = all[0].info.EstCostUSD
	p.Reason = reason
	dec.RoutingMs = float64(time.Since(t0).Microseconds()) / 1000
	return nil
}

func (e *Engine) rankByObjective(pol *config.Policy, rs []ranked) {
	switch pol.Objective {
	case "min_latency":
		sort.SliceStable(rs, func(i, j int) bool {
			if rs[i].info.EstLatencyMs != rs[j].info.EstLatencyMs {
				return rs[i].info.EstLatencyMs < rs[j].info.EstLatencyMs
			}
			return rs[i].info.EstCostUSD < rs[j].info.EstCostUSD
		})
	case "max_quality":
		sort.SliceStable(rs, func(i, j int) bool {
			if rs[i].d.Tier != rs[j].d.Tier {
				return rs[i].d.Tier > rs[j].d.Tier
			}
			return rs[i].info.EstCostUSD < rs[j].info.EstCostUSD
		})
	case "weighted":
		var maxC, maxL float64
		for _, r := range rs {
			maxC = math.Max(maxC, r.info.EstCostUSD)
			maxL = math.Max(maxL, r.info.EstLatencyMs)
		}
		w := pol.Weights
		for i := range rs {
			nc, nl := 0.0, 0.0
			if maxC > 0 {
				nc = rs[i].info.EstCostUSD / maxC
			}
			if maxL > 0 {
				nl = rs[i].info.EstLatencyMs / maxL
			}
			rs[i].info.Score = w.Cost*nc + w.Latency*nl - w.Quality*float64(rs[i].d.Tier)/3
		}
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].info.Score < rs[j].info.Score })
	default: // min_cost
		sort.SliceStable(rs, func(i, j int) bool {
			if rs[i].info.EstCostUSD != rs[j].info.EstCostUSD {
				return rs[i].info.EstCostUSD < rs[j].info.EstCostUSD
			}
			if rs[i].d.Tier != rs[j].d.Tier {
				return rs[i].d.Tier > rs[j].d.Tier
			}
			return rs[i].info.EstLatencyMs < rs[j].info.EstLatencyMs
		})
	}
	for i := range rs {
		if pol.Objective != "weighted" {
			rs[i].info.Score = float64(i)
		}
	}
}

// balancePools reorders members of the same pool by least in-flight requests, then
// weight, keeping the pool's position in the ranking [PRV-09].
func (e *Engine) balancePools(rs []ranked) []ranked {
	groups := map[string][]int{}
	for i, r := range rs {
		if r.d.Pool != "" {
			groups[r.d.Pool] = append(groups[r.d.Pool], i)
		}
	}
	for _, idxs := range groups {
		if len(idxs) < 2 {
			continue
		}
		members := make([]ranked, len(idxs))
		for k, i := range idxs {
			members[k] = rs[i]
		}
		sort.SliceStable(members, func(a, b int) bool {
			fa, fb := 0, 0
			if e.Hooks.InFlight != nil {
				fa, fb = e.Hooks.InFlight(members[a].d.ID), e.Hooks.InFlight(members[b].d.ID)
			}
			// Weighted least-in-flight: compare in-flight per unit of weight.
			la := float64(fa) / float64(max(1, members[a].d.Weight))
			lb := float64(fb) / float64(max(1, members[b].d.Weight))
			if la != lb {
				return la < lb
			}
			return members[a].d.Weight > members[b].d.Weight
		})
		for k, i := range idxs {
			rs[i] = members[k]
		}
	}
	return rs
}

// difficulty runs the policy's estimator [RTE-03, RTE-04].
func (e *Engine) difficulty(s *Snapshot, rc *RequestContext, req *api.ChatRequest, pol *config.Policy, dec *Decision) Difficulty {
	if e.Estimators != nil {
		if est, ok := e.Estimators[pol.Difficulty.Estimator]; ok {
			if d, ok := est(s, rc, req, pol, dec); ok {
				return d
			}
		}
	}
	return RuleDifficulty(req, pol.Difficulty.Thresholds)
}

// residencyFor maps the request's sensitivity to allowed residency tags [GRD-02].
func (e *Engine) residencyFor(s *Snapshot, rc *RequestContext) []string {
	allowed := rc.Principal.Scopes.AllowedResidency
	if rc.Hints.Sensitivity != "" {
		if tags, ok := s.Config.Guardrails.Sensitivity[rc.Hints.Sensitivity]; ok {
			allowed = intersect(allowed, tags)
			if allowed == nil {
				allowed = tags
			}
		}
	}
	return allowed
}
