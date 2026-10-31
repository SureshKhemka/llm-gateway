package config

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Issue is one validation problem, located by file and path [CFG-02].
type Issue struct {
	File   string `json:"file"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

func (i Issue) String() string {
	if i.Path == "" {
		return fmt.Sprintf("%s: %s", i.File, i.Reason)
	}
	return fmt.Sprintf("%s: %s: %s", i.File, i.Path, i.Reason)
}

type ValidationError struct{ Issues []Issue }

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Issues))
	for i, is := range e.Issues {
		parts[i] = is.String()
	}
	return "invalid configuration:\n  " + strings.Join(parts, "\n  ")
}

var (
	KnownCapabilities = map[string]bool{"chat": true, "tools": true, "vision": true, "json_schema": true, "embeddings": true, "reasoning": true}
	knownObjectives   = map[string]bool{"min_cost": true, "min_latency": true, "max_quality": true, "weighted": true}
	knownEstimators   = map[string]bool{"rules": true, "similarity": true, "classifier": true}
	knownValidators   = map[string]bool{"json_schema": true, "non_empty": true, "not_truncated": true, "no_refusal": true, "judge": true}
	knownRoles        = map[string]bool{"org_admin": true, "tenant_admin": true, "finops_viewer": true, "auditor": true}
	knownPeriods      = map[string]bool{"daily": true, "weekly": true, "monthly": true, "custom": true}
	knownScopes       = map[string]bool{"tenant": true, "project": true, "key": true, "end_user": true, "eval": true, "shadow": true}
	knownPII          = map[string]bool{"off": true, "log": true, "redact": true, "block": true}
)

// Validate checks the whole configuration and returns every problem found.
func Validate(c *Config) error {
	v := &validator{c: c}
	v.run()
	if len(v.issues) > 0 {
		return &ValidationError{Issues: v.issues}
	}
	return nil
}

type validator struct {
	c      *Config
	issues []Issue
}

func (v *validator) add(kind, id, path, format string, args ...any) {
	v.issues = append(v.issues, Issue{File: v.c.FileOf(kind, id), Path: path, Reason: fmt.Sprintf(format, args...)})
}

func (v *validator) secret(kind, id, path string, s SecretRef) {
	if !s.valid() {
		v.add(kind, id, path, "secrets must be references (env:NAME or file:/path), not inline values")
	}
}

func (v *validator) run() {
	c := v.c
	v.secret("storage", "", "storage.redis_url", c.Storage.RedisURL)
	v.secret("storage", "", "storage.postgres_dsn", c.Storage.PostgresDSN)

	providers := map[string]bool{}
	for i, p := range c.Providers {
		path := fmt.Sprintf("providers[%d]", i)
		switch {
		case p.ID == "":
			v.add("providers", p.ID, path+".id", "is required")
		case providers[p.ID]:
			v.add("providers", p.ID, path+".id", "duplicate provider id %q", p.ID)
		}
		providers[p.ID] = true
		if p.Type != "openai" && p.Type != "anthropic" {
			v.add("providers", p.ID, path+".type", "must be openai or anthropic, got %q", p.Type)
		}
		if p.BaseURL == "" {
			if p.Type != "anthropic" {
				v.add("providers", p.ID, path+".base_url", "is required")
			}
		} else if u, err := url.Parse(p.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			v.add("providers", p.ID, path+".base_url", "must be an http(s) URL")
		} else if p.AllowPrivateNetwork != nil && !*p.AllowPrivateNetwork && isPrivateHost(u.Hostname()) {
			v.add("providers", p.ID, path+".base_url", "points at a private address but allow_private_network is false")
		}
		v.secret("providers", p.ID, path+".api_key", p.APIKey)
		for k := range p.Headers {
			if strings.EqualFold(k, "authorization") || strings.EqualFold(k, "x-api-key") {
				v.add("providers", p.ID, path+".headers."+k, "credentials must go in api_key as a secret reference")
			}
		}
	}

	deps := map[string]Deployment{}
	for i, d := range c.Deployments {
		path := fmt.Sprintf("deployments[%d]", i)
		if d.ID == "" {
			v.add("deployments", d.ID, path+".id", "is required")
		} else if _, dup := deps[d.ID]; dup {
			v.add("deployments", d.ID, path+".id", "duplicate deployment id %q", d.ID)
		}
		deps[d.ID] = d
		if !providers[d.Provider] {
			v.add("deployments", d.ID, path+".provider", "unknown provider %q", d.Provider)
		}
		if d.Model == "" {
			v.add("deployments", d.ID, path+".model", "is required")
		}
		for j, cp := range d.Capabilities {
			if !KnownCapabilities[cp] {
				v.add("deployments", d.ID, fmt.Sprintf("%s.capabilities[%d]", path, j), "unknown capability %q", cp)
			}
		}
		if d.Tier < 0 || d.Tier > 3 {
			v.add("deployments", d.ID, path+".tier", "must be between 0 and 3")
		}
		if d.ContextWindow < 0 || d.MaxOutputTokens < 0 || d.Concurrency < 0 {
			v.add("deployments", d.ID, path, "context_window, max_output_tokens and concurrency must be ≥ 0")
		}
		if len(d.Prices) == 0 {
			v.add("deployments", d.ID, path+".prices", "at least one price card is required (use 0 for free local models)")
		}
		seen := map[time.Time]bool{}
		for j, pc := range d.Prices {
			pp := fmt.Sprintf("%s.prices[%d]", path, j)
			if pc.Input < 0 || pc.Output < 0 || (pc.CachedInput != nil && *pc.CachedInput < 0) || (pc.Reasoning != nil && *pc.Reasoning < 0) {
				v.add("deployments", d.ID, pp, "prices must be ≥ 0")
			}
			if seen[pc.EffectiveFrom] {
				v.add("deployments", d.ID, pp+".effective_from", "duplicate effective date")
			}
			seen[pc.EffectiveFrom] = true
		}
	}

	policies := map[string]bool{}
	for _, p := range BuiltinPolicies() {
		policies[p.Name] = true
	}
	userPolicies := map[string]map[int]bool{}
	for i, p := range c.Policies {
		path := fmt.Sprintf("policies[%d]", i)
		if p.Name == "" {
			v.add("policies", p.Name, path+".name", "is required")
		}
		if userPolicies[p.Name] == nil {
			userPolicies[p.Name] = map[int]bool{}
		}
		if userPolicies[p.Name][p.Version] {
			v.add("policies", p.Name, path+".version", "duplicate version %d of policy %q", p.Version, p.Name)
		}
		userPolicies[p.Name][p.Version] = true
		policies[p.Name] = true
		if !knownObjectives[p.Objective] {
			v.add("policies", p.Name, path+".objective", "unknown objective %q", p.Objective)
		}
		if !knownEstimators[p.Difficulty.Estimator] {
			v.add("policies", p.Name, path+".difficulty.estimator", "unknown estimator %q", p.Difficulty.Estimator)
		}
		if !sort.Float64sAreSorted(p.Difficulty.Thresholds) {
			v.add("policies", p.Name, path+".difficulty.thresholds", "must be ascending")
		}
		if p.Difficulty.Estimator == "classifier" && p.Difficulty.Classifier == "" {
			v.add("policies", p.Name, path+".difficulty.classifier", "is required for the classifier estimator")
		}
		for _, ref := range []struct{ field, id string }{
			{"difficulty.classifier", p.Difficulty.Classifier},
			{"difficulty.embedder", p.Difficulty.Embedder},
			{"cascade.judge", p.Cascade.Judge},
			{"baseline", p.Baseline},
		} {
			if ref.id != "" {
				if _, ok := deps[ref.id]; !ok {
					v.add("policies", p.Name, path+"."+ref.field, "unknown deployment %q", ref.id)
				}
			}
		}
		for j, val := range p.Cascade.Validators {
			if !knownValidators[val] {
				v.add("policies", p.Name, fmt.Sprintf("%s.cascade.validators[%d]", path, j), "unknown validator %q", val)
			}
			if val == "judge" && p.Cascade.Judge == "" {
				v.add("policies", p.Name, path+".cascade.judge", "is required when the judge validator is enabled")
			}
		}
		for j, s := range p.Degrade {
			if s.AtPct <= 0 || s.AtPct > 100 {
				v.add("policies", p.Name, fmt.Sprintf("%s.degrade[%d].at_pct", path, j), "must be in (0, 100]")
			}
		}
	}
	for i, p := range c.Policies {
		for j, s := range p.Split {
			if !policies[s.Policy] {
				v.add("policies", p.Name, fmt.Sprintf("policies[%d].split[%d].policy", i, j), "unknown policy %q", s.Policy)
			}
		}
		if p.Shadow != nil {
			if p.Shadow.Policy != "" && !policies[p.Shadow.Policy] {
				v.add("policies", p.Name, fmt.Sprintf("policies[%d].shadow.policy", i), "unknown policy %q", p.Shadow.Policy)
			}
			if p.Shadow.Deployment != "" {
				if _, ok := deps[p.Shadow.Deployment]; !ok {
					v.add("policies", p.Name, fmt.Sprintf("policies[%d].shadow.deployment", i), "unknown deployment %q", p.Shadow.Deployment)
				}
			}
		}
	}

	vms := map[string]bool{}
	for i, vm := range c.VirtualModels {
		path := fmt.Sprintf("virtual_models[%d]", i)
		if vm.Name == "" {
			v.add("virtual_models", vm.Name, path+".name", "is required")
		}
		if vms[vm.Name] {
			v.add("virtual_models", vm.Name, path+".name", "duplicate virtual model %q", vm.Name)
		}
		if _, clash := deps[vm.Name]; clash {
			v.add("virtual_models", vm.Name, path+".name", "clashes with a deployment id")
		}
		vms[vm.Name] = true
		if vm.Policy != "" && !policies[vm.Policy] {
			v.add("virtual_models", vm.Name, path+".policy", "unknown policy %q", vm.Policy)
		}
		for j, d := range vm.Deployments {
			if _, ok := deps[d]; !ok {
				v.add("virtual_models", vm.Name, fmt.Sprintf("%s.deployments[%d]", path, j), "unknown deployment %q", d)
			}
		}
	}
	if c.Defaults.Policy != "" && !policies[c.Defaults.Policy] {
		v.add("defaults", "", "defaults.policy", "unknown policy %q", c.Defaults.Policy)
	}
	if c.Defaults.Baseline != "" {
		if _, ok := deps[c.Defaults.Baseline]; !ok {
			v.add("defaults", "", "defaults.baseline", "unknown deployment %q", c.Defaults.Baseline)
		}
	}

	tenants := map[string]bool{}
	projects := map[string]bool{}
	keys := map[string]bool{}
	for i, t := range c.Tenants {
		path := fmt.Sprintf("tenants[%d]", i)
		if t.ID == "" {
			v.add("tenants", t.ID, path+".id", "is required")
		}
		if tenants[t.ID] {
			v.add("tenants", t.ID, path+".id", "duplicate tenant id %q", t.ID)
		}
		tenants[t.ID] = true
		if t.Policy != "" && !policies[t.Policy] {
			v.add("tenants", t.ID, path+".policy", "unknown policy %q", t.Policy)
		}
		if t.PII != "" && !knownPII[t.PII] {
			v.add("tenants", t.ID, path+".pii", "must be off, log, redact or block")
		}
		for j, p := range t.Projects {
			pp := fmt.Sprintf("%s.projects[%d]", path, j)
			if p.ID == "" || projects[p.ID] {
				v.add("tenants", t.ID, pp+".id", "project id must be set and unique")
			}
			projects[p.ID] = true
			if p.Policy != "" && !policies[p.Policy] {
				v.add("tenants", t.ID, pp+".policy", "unknown policy %q", p.Policy)
			}
			for k, key := range p.Keys {
				kp := fmt.Sprintf("%s.keys[%d]", pp, k)
				if key.ID == "" || keys[key.ID] {
					v.add("tenants", t.ID, kp+".id", "key id must be set and unique")
				}
				keys[key.ID] = true
				if key.Secret.IsZero() {
					v.add("tenants", t.ID, kp+".secret", "bootstrap keys need a secret reference")
				}
				v.secret("tenants", t.ID, kp+".secret", key.Secret)
				if key.Scopes.Policy != "" && !policies[key.Scopes.Policy] {
					v.add("tenants", t.ID, kp+".scopes.policy", "unknown policy %q", key.Scopes.Policy)
				}
			}
		}
	}

	for i, a := range c.Admins {
		path := fmt.Sprintf("admins[%d]", i)
		if !knownRoles[a.Role] {
			v.add("admins", a.ID, path+".role", "unknown role %q", a.Role)
		}
		if a.Role == "tenant_admin" && !tenants[a.Tenant] {
			v.add("admins", a.ID, path+".tenant", "tenant_admin needs an existing tenant")
		}
		if a.Secret.IsZero() {
			v.add("admins", a.ID, path+".secret", "is required")
		}
		v.secret("admins", a.ID, path+".secret", a.Secret)
	}

	sinks := map[string]bool{}
	for i, a := range c.Alerts {
		path := fmt.Sprintf("alerts[%d]", i)
		sinks[a.ID] = true
		if a.Type != "webhook" && a.Type != "slack" {
			v.add("alerts", a.ID, path+".type", "must be webhook or slack")
		}
		v.secret("alerts", a.ID, path+".url", a.URL)
	}

	budgetIDs := map[string]bool{}
	for i, b := range c.Budgets {
		path := fmt.Sprintf("budgets[%d]", i)
		if b.ID == "" || budgetIDs[b.ID] {
			v.add("budgets", b.ID, path+".id", "budget id must be set and unique")
		}
		budgetIDs[b.ID] = true
		if !knownScopes[b.Scope] {
			v.add("budgets", b.ID, path+".scope", "unknown scope %q", b.Scope)
		}
		switch b.Scope {
		case "tenant":
			if !tenants[b.ScopeID] {
				v.add("budgets", b.ID, path+".scope_id", "unknown tenant %q", b.ScopeID)
			}
		case "project":
			if !projects[b.ScopeID] {
				v.add("budgets", b.ID, path+".scope_id", "unknown project %q", b.ScopeID)
			}
		case "key":
			if !keys[b.ScopeID] {
				v.add("budgets", b.ID, path+".scope_id", "unknown key %q", b.ScopeID)
			}
		}
		if !knownPeriods[b.Period] {
			v.add("budgets", b.ID, path+".period", "unknown period %q", b.Period)
		}
		if b.Period == "custom" && b.Custom <= 0 {
			v.add("budgets", b.ID, path+".custom_period", "is required for custom periods")
		}
		if _, err := time.LoadLocation(b.Timezone); err != nil {
			v.add("budgets", b.ID, path+".timezone", "unknown time zone %q", b.Timezone)
		}
		if b.HardUSD <= 0 {
			v.add("budgets", b.ID, path+".hard_usd", "must be > 0")
		}
		for j, s := range b.SoftPct {
			if s <= 0 || s >= 100 {
				v.add("budgets", b.ID, fmt.Sprintf("%s.soft_pct[%d]", path, j), "must be in (0, 100)")
			}
		}
		for j, s := range b.AlertSink {
			if !sinks[s] {
				v.add("budgets", b.ID, fmt.Sprintf("%s.alerts[%d]", path, j), "unknown alert sink %q", s)
			}
		}
	}

	v.secret("storage", "", "storage.body_key", c.Storage.BodyKey)
	for _, t := range c.Tenants {
		if t.BodyLogging.Enabled && c.Storage.BodyKey.IsZero() {
			v.add("tenants", t.ID, "tenants["+t.ID+"].body_logging", "body logging needs storage.body_key so stored bodies are encrypted")
		}
	}
	for i, a := range c.ModelAliases {
		if _, isDep := deps[a.Model]; !isDep && !vms[a.Model] && a.Model != "auto" && a.Model != "auto-fast" && a.Model != "auto-cheap" {
			v.add("model_aliases", "", fmt.Sprintf("model_aliases[%d].model", i), "unknown model %q", a.Model)
		}
	}
	if !knownPII[c.Guardrails.PII] {
		v.add("guardrails", "", "guardrails.pii", "must be off, log, redact or block")
	}
	if c.Cache.Embedder != "" {
		if _, ok := deps[c.Cache.Embedder]; !ok {
			v.add("cache", "", "cache.embedder", "unknown deployment %q", c.Cache.Embedder)
		}
	}
	if c.Cache.Semantic && c.Cache.Embedder == "" {
		v.add("cache", "", "cache.embedder", "is required when semantic caching is on")
	}
	if c.Cache.SemanticThreshold <= 0 || c.Cache.SemanticThreshold > 1 {
		v.add("cache", "", "cache.semantic_threshold", "must be in (0, 1]")
	}
}

func isPrivateHost(host string) bool {
	if host == "localhost" || host == "host.docker.internal" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified())
}
