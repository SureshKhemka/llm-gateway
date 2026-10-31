package admin

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/gateway"
	"github.com/SureshKhemka/llm-gateway/internal/governance"
	"github.com/SureshKhemka/llm-gateway/internal/store"
)

func (x *API) consoleRoutes() {
	x.add("GET", "/admin/v1/overview", "Live overview: requests/s, spend and savings today, cache hit rate, errors", PermCostRead, x.overview)
	x.add("GET", "/admin/v1/catalog", "Deployments, price cards and discovered (unregistered) models", PermHealthRead, x.catalog)
	x.add("POST", "/admin/v1/catalog/{id...}", "Approve a discovered model with metadata", PermCatalogWrite, x.approve)
	x.add("POST", "/admin/v1/playground/explain", "Playground: route decision and per-candidate estimates (no model call)", PermPlayground, x.playExplain)
	x.add("POST", "/admin/v1/playground/run", "Playground: run a prompt on one or more models side by side", PermPlayground, x.playRun)
}

// overview powers the console's live dashboard [ADM-02].
func (x *API) overview(w http.ResponseWriter, r *http.Request, a *Admin) {
	x.Engine.Writer.Flush()
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	f := store.CostFilter{Tenant: a.Tenant, From: day}
	today, _ := x.Engine.Store.QueryCost(r.Context(), store.CostQuery{Filter: f})
	lastMin, _ := x.Engine.Store.QueryCost(r.Context(), store.CostQuery{Filter: store.CostFilter{Tenant: a.Tenant, From: now.Add(-time.Minute)}})
	out := map[string]any{"as_of": now}
	if len(today) == 1 {
		t := today[0]
		out["requests_today"], out["spend_today_usd"] = t.Requests, t.CostUSD
		out["routing_savings_today_usd"], out["cache_savings_today_usd"] = t.RoutingSavings, t.CacheSavings
		out["errors_today"] = t.Errors
		if t.Requests > 0 {
			out["cache_hit_rate"] = float64(t.CacheHits) / float64(t.Requests)
			out["error_rate"] = float64(t.Errors) / float64(t.Requests)
		}
	}
	if len(lastMin) == 1 {
		out["requests_per_second"] = float64(lastMin[0].Requests) / 60
	} else {
		out["requests_per_second"] = 0.0
	}
	open := 0
	for _, st := range x.Engine.Health.All() {
		if st.Breaker != "closed" {
			open++
		}
	}
	out["open_circuits"] = open
	writeJSON(w, 200, out)
}

func (x *API) catalog(w http.ResponseWriter, _ *http.Request, _ *Admin) {
	s := x.Engine.Snapshot()
	var deps []any
	for _, id := range s.Catalog.Order {
		deps = append(deps, s.Catalog.Deployments[id])
	}
	var vms []any
	names := make([]string, 0, len(s.Catalog.VirtualModels))
	for n := range s.Catalog.VirtualModels {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		vm := s.Catalog.VirtualModels[n]
		vms = append(vms, map[string]any{"name": vm.Name, "policy": vm.Policy, "deployments": vm.Deployments})
	}
	var discovered []any
	for _, d := range x.Engine.Discovery.List() {
		discovered = append(discovered, map[string]any{"id": d.ID, "provider": d.Provider, "model": d.Model, "approved": d.Approved})
	}
	writeJSON(w, 200, map[string]any{"deployments": deps, "virtual_models": vms, "discovered": discovered})
}

// approve registers a discovered model with operator-set metadata [PRV-06].
func (x *API) approve(w http.ResponseWriter, r *http.Request, a *Admin) {
	id := r.PathValue("id")
	var meta config.Deployment
	if err := decode(r, &meta); err != nil {
		api.WriteError(w, api.FormatOpenAI, err)
		return
	}
	if !x.Engine.Discovery.Approve(id, &meta) {
		fail(w, api.CodeNotFound, "no discovered model %q", id)
		return
	}
	if err := x.rebuild(r.Context(), "admin:approve"); err != nil {
		fail(w, api.CodeInternal, "%v", err)
		return
	}
	x.audit(r.Context(), a, "", "approve", "deployment", id, nil, meta, "")
	writeJSON(w, 200, map[string]any{"approved": id})
}

type playReq struct {
	Tenant   string          `json:"tenant"`
	Models   []string        `json:"models"`
	Messages json.RawMessage `json:"messages"`
	Prompt   string          `json:"prompt"`
}

func (x *API) playContext(w http.ResponseWriter, a *Admin, req *playReq) (*gateway.RequestContext, bool) {
	tenant, ok := x.tenantParam(w, a, req.Tenant)
	if !ok {
		return nil, false
	}
	s := x.Engine.Snapshot()
	t := s.Tenants[tenant]
	if t == nil {
		fail(w, api.CodeInvalidRequest, "choose a tenant to bill playground requests to")
		return nil, false
	}
	p := &governance.Principal{Tenant: t, KeyID: "playground:" + a.ID, KeyPrefix: "playground", Scopes: config.Scopes{DefaultTags: []string{"playground"}}}
	if len(t.Projects) > 0 {
		p.Project = &t.Projects[0]
	}
	return &gateway.RequestContext{ID: gateway.NewRequestID(), Start: time.Now(), Endpoint: "playground", Principal: p}, true
}

func (p *playReq) chat(model string) *api.ChatRequest {
	req := &api.ChatRequest{Model: model}
	if len(p.Messages) > 0 {
		_ = json.Unmarshal(p.Messages, &req.Messages)
	} else {
		req.Messages = []api.Message{{Role: "user", Content: api.TextContent(p.Prompt)}}
	}
	return req
}

// playExplain shows the route decision with each candidate's estimated cost [ADM-03].
func (x *API) playExplain(w http.ResponseWriter, r *http.Request, a *Admin) {
	var req playReq
	if err := decode(r, &req); err != nil {
		api.WriteError(w, api.FormatOpenAI, err)
		return
	}
	rc, ok := x.playContext(w, a, &req)
	if !ok {
		return
	}
	model := "auto"
	if len(req.Models) > 0 {
		model = req.Models[0]
	}
	dec, err := x.Engine.Explain(rc, req.chat(model))
	out := map[string]any{"decision": dec}
	if err != nil {
		out["error"] = err.Error()
	}
	writeJSON(w, 200, out)
}

// playRun sends the prompt to each model (side by side) and returns answers with their
// actual cost [ADM-03].
func (x *API) playRun(w http.ResponseWriter, r *http.Request, a *Admin) {
	var req playReq
	if err := decode(r, &req); err != nil {
		api.WriteError(w, api.FormatOpenAI, err)
		return
	}
	if len(req.Models) == 0 {
		req.Models = []string{"auto"}
	}
	if len(req.Models) > 6 {
		fail(w, api.CodeInvalidRequest, "at most 6 models side by side")
		return
	}
	var results []any
	for _, m := range req.Models {
		rc, ok := x.playContext(w, a, &req)
		if !ok {
			return
		}
		rc.Hints.Cache = "bypass"
		resp, meta, err := x.Engine.Chat(r.Context(), rc, req.chat(m))
		item := map[string]any{"model": m, "request_id": rc.ID}
		if meta != nil {
			item["served_model"], item["cost_usd"], item["latency_ms"], item["route_reason"] = meta.Deployment, meta.CostUSD, meta.LatencyMs, meta.RouteReason
			item["usage"] = meta.Usage
		}
		if err != nil {
			item["error"] = err.Error()
		} else if len(resp.Choices) > 0 {
			item["answer"] = resp.Choices[0].Message.Content.PlainText()
			item["tool_calls"] = resp.Choices[0].Message.ToolCalls
		}
		results = append(results, item)
	}
	writeJSON(w, 200, map[string]any{"results": results})
}
