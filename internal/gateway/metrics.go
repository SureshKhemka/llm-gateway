package gateway

import (
	"context"

	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/governance"
	"github.com/SureshKhemka/llm-gateway/internal/observe"
)

// WireMetrics connects Prometheus metrics to the engine: bounded label sets follow the
// configuration, and gauges report circuit state, in-flight requests, queue depth and
// budget utilisation at scrape time [OBS-02].
func (e *Engine) WireMetrics(m *observe.Metrics) {
	known := func(s *Snapshot) {
		var tenants, models, providers, policies []string
		for id := range s.Tenants {
			tenants = append(tenants, id)
		}
		tenants = append(tenants, "default")
		for id := range s.Catalog.Deployments {
			models = append(models, id)
		}
		for id := range s.Catalog.Providers {
			providers = append(providers, id)
		}
		for name := range s.Policies {
			policies = append(policies, name)
		}
		m.SetKnown("tenant", tenants)
		m.SetKnown("model", models)
		m.SetKnown("provider", providers)
		m.SetKnown("policy", policies)
	}
	known(e.Snapshot())
	e.OnSwap = append(e.OnSwap, known)
	m.SetGauges(func() []observe.GaugeSample {
		s := e.Snapshot()
		var out []observe.GaugeSample
		for _, id := range s.Catalog.Order {
			st := e.Health.Status(id)
			v := map[string]float64{"closed": 0, "half_open": 1, "open": 2}[string(st.Breaker)]
			out = append(out,
				observe.GaugeSample{Name: "gw_circuit_state", Help: "Circuit breaker state per deployment (0 closed, 1 half-open, 2 open).", Labels: map[string]string{"deployment": id}, Value: v},
				observe.GaugeSample{Name: "gw_deployment_in_flight", Help: "In-flight requests per deployment.", Labels: map[string]string{"deployment": id}, Value: float64(e.Limiter.InFlight(id))},
				observe.GaugeSample{Name: "gw_deployment_queue_depth", Help: "Requests waiting for a concurrency slot.", Labels: map[string]string{"deployment": id}, Value: float64(e.Limiter.Queued(id))},
			)
		}
		for i := range s.Config.Budgets {
			b := s.Config.Budgets[i]
			if b.Scope == "end_user" {
				continue // one series per end user would be unbounded
			}
			refs := e.Gov.Applicable(s.Config.Budgets[i:i+1], s.Grants, budgetScope(&b))
			if len(refs) == 0 {
				continue
			}
			u := e.Gov.Utilisation(context.Background(), refs)
			out = append(out, observe.GaugeSample{Name: "gw_budget_utilisation_ratio", Help: "Spent plus reserved over the hard limit, current period.",
				Labels: map[string]string{"budget": b.ID, "scope": b.Scope}, Value: u / 100})
		}
		return out
	})
}

// budgetScope builds a Scope that budget b applies to (for status queries).
func budgetScope(b *config.Budget) governance.Scope {
	switch b.Scope {
	case "tenant":
		return governance.Scope{Tenant: b.ScopeID}
	case "project":
		return governance.Scope{Project: b.ScopeID}
	case "key":
		return governance.Scope{Key: b.ScopeID}
	case "eval":
		return governance.Scope{Tenant: b.ScopeID, Eval: true}
	case "shadow":
		return governance.Scope{Tenant: b.ScopeID, Shadow: true}
	}
	return governance.Scope{}
}
