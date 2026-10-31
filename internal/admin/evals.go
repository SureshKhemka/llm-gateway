package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/eval"
	"github.com/SureshKhemka/llm-gateway/internal/evalrun"
	"github.com/SureshKhemka/llm-gateway/internal/store"
)

func (x *API) evalRoutes() {
	x.add("POST", "/admin/v1/evals/suites", "Create or replace an eval suite (JSON or JSONL body)", PermEvalsRun, x.putSuite)
	x.add("GET", "/admin/v1/evals/suites", "List eval suites", PermEvalsRead, x.listSuites)
	x.add("POST", "/admin/v1/evals/runs", "Start an eval run across deployments and policy versions", PermEvalsRun, x.startRun)
	x.add("GET", "/admin/v1/evals/runs", "List eval runs", PermEvalsRead, x.listRuns)
	x.add("GET", "/admin/v1/evals/runs/{id}", "An eval run's status and report", PermEvalsRead, x.getRun)
	x.add("GET", "/admin/v1/evals/runs/{id}/pareto.svg", "Quality-versus-cost chart for a run", PermEvalsRead, x.paretoSVG)
	x.add("POST", "/admin/v1/evals/runs/{id}/recommend", "Recommend difficulty and semantic-cache thresholds", PermEvalsRead, x.recommend)
	x.add("POST", "/admin/v1/evals/replay", "Replay captured production requests against a candidate policy", PermEvalsRun, x.replay)

	x.add("GET", "/admin/v1/policies", "Policies with their versions", PermPoliciesRead, x.listPolicies)
	x.add("POST", "/admin/v1/policies", "Create a new (inactive) policy version", PermPoliciesWrite, x.createPolicyVersion)
	x.add("POST", "/admin/v1/policies/{name}/activate", "Activate a policy version (promotion gate)", PermPoliciesWrite, x.activatePolicy)
	x.add("POST", "/admin/v1/policies/{name}/rollback", "Roll back to a prior policy version", PermPoliciesWrite, x.rollbackPolicy)
	x.add("GET", "/admin/v1/policies/{name}/diff", "Field-level diff between two policy versions", PermPoliciesRead, x.diffPolicy)
	x.add("GET", "/admin/v1/quality", "Online quality signals by route", PermCostRead, x.quality)
}

func (x *API) runner() *evalrun.Runner { return &evalrun.Runner{Engine: x.Engine} }

func (x *API) putSuite(w http.ResponseWriter, r *http.Request, a *Admin) {
	body, _ := io.ReadAll(http.MaxBytesReader(nil, r.Body, 64<<20))
	var suite eval.Suite
	if strings.Contains(r.Header.Get("Content-Type"), "jsonl") || strings.Contains(r.Header.Get("Content-Type"), "x-ndjson") {
		items, err := eval.ImportJSONL(bytes.NewReader(body))
		if err != nil {
			fail(w, api.CodeInvalidRequest, "%v", err)
			return
		}
		suite = eval.Suite{ID: r.URL.Query().Get("id"), Name: r.URL.Query().Get("name"), Items: items}
	} else {
		if err := json.Unmarshal(body, &suite); err != nil {
			fail(w, api.CodeInvalidRequest, "invalid suite: %v", err)
			return
		}
		var buf bytes.Buffer
		for _, it := range suite.Items {
			b, _ := json.Marshal(it)
			buf.Write(append(b, '\n'))
		}
		items, err := eval.ImportJSONL(&buf)
		if err != nil {
			fail(w, api.CodeInvalidRequest, "%v", err)
			return
		}
		suite.Items = items
	}
	if suite.ID == "" || len(suite.Items) == 0 {
		fail(w, api.CodeInvalidRequest, "suite id and at least one item are required")
		return
	}
	suite.Created = time.Now().UTC()
	if err := x.runner().SaveSuite(r.Context(), &suite, a.Tenant); err != nil {
		fail(w, api.CodeInternal, "%v", err)
		return
	}
	x.audit(r.Context(), a, a.Tenant, "create", "eval_suite", suite.ID, nil, map[string]any{"items": len(suite.Items)}, "")
	writeJSON(w, 201, map[string]any{"id": suite.ID, "items": len(suite.Items)})
}

func (x *API) listSuites(w http.ResponseWriter, r *http.Request, a *Admin) {
	docs, _ := x.Engine.Store.ListDocs(r.Context(), evalrun.DocSuite, a.Tenant)
	var out []map[string]any
	for _, d := range docs {
		var s eval.Suite
		if json.Unmarshal(d.Body, &s) == nil {
			out = append(out, map[string]any{"id": s.ID, "name": s.Name, "items": len(s.Items), "created": s.Created})
		}
	}
	writeJSON(w, 200, map[string]any{"suites": out})
}

type startRunReq struct {
	Suite       string           `json:"suite"`
	Tenant      string           `json:"tenant"`
	Project     string           `json:"project"`
	Targets     []evalrun.Target `json:"targets"`
	AllDeploys  bool             `json:"all_deployments"`
	Baseline    string           `json:"baseline"`
	Concurrency int              `json:"concurrency"`
	Wait        bool             `json:"wait"`
}

func (x *API) startRun(w http.ResponseWriter, r *http.Request, a *Admin) {
	var req startRunReq
	if err := decode(r, &req); err != nil {
		api.WriteError(w, api.FormatOpenAI, err)
		return
	}
	tenant, ok := x.tenantParam(w, a, req.Tenant)
	if !ok {
		return
	}
	if tenant == "" {
		fail(w, api.CodeInvalidRequest, "tenant is required (eval spend is charged to its eval budget)")
		return
	}
	suite, err := x.runner().Suite(r.Context(), req.Suite)
	if err != nil {
		fail(w, api.CodeNotFound, "suite %q not found", req.Suite)
		return
	}
	s := x.Engine.Snapshot()
	targets := req.Targets
	if req.AllDeploys {
		for _, d := range s.Catalog.Usable() {
			if d.Has("chat") {
				targets = append(targets, evalrun.Target{Kind: "deployment", Name: d.ID})
			}
		}
	}
	for i, t := range targets {
		switch t.Kind {
		case "deployment":
			if s.Catalog.Deployments[t.Name] == nil {
				fail(w, api.CodeInvalidRequest, "unknown deployment %q", t.Name)
				return
			}
		case "policy":
			if t.Version == 0 {
				if p := s.Policies[t.Name]; p != nil {
					targets[i].Version = p.Version
				}
			}
		default:
			fail(w, api.CodeInvalidRequest, "target kind must be deployment or policy")
			return
		}
	}
	if len(targets) == 0 {
		fail(w, api.CodeInvalidRequest, "no targets")
		return
	}
	run := &evalrun.Run{ID: evalrun.NewRunID(), Suite: suite.ID, Tenant: tenant, Project: req.Project, Targets: targets,
		Baseline: req.Baseline, Status: "running", Started: time.Now().UTC(), Concurrency: req.Concurrency}
	_ = x.runner().Save(r.Context(), run)
	x.audit(r.Context(), a, tenant, "start", "eval_run", run.ID, nil, map[string]any{"suite": suite.ID, "targets": targets}, "")
	exec := func(ctx context.Context) {
		if err := x.runner().Execute(ctx, run, suite); err != nil {
			run.Status, run.Error = "failed", err.Error()
			_ = x.runner().Save(ctx, run)
		}
	}
	if req.Wait {
		exec(r.Context())
		writeJSON(w, 200, summary(run))
		return
	}
	go exec(context.Background())
	writeJSON(w, 202, map[string]any{"id": run.ID, "status": run.Status})
}

func summary(run *evalrun.Run) map[string]any {
	return map[string]any{"id": run.ID, "suite": run.Suite, "status": run.Status, "error": run.Error, "started": run.Started,
		"finished": run.Finished, "targets": run.Targets, "baseline": run.Baseline, "report": run.Report, "results": len(run.Results)}
}

func (x *API) loadRun(w http.ResponseWriter, r *http.Request, a *Admin) *evalrun.Run {
	run, err := x.runner().Load(r.Context(), r.PathValue("id"))
	if err != nil || !a.CanTenant(run.Tenant) {
		fail(w, api.CodeNotFound, "run not found")
		return nil
	}
	return run
}

func (x *API) listRuns(w http.ResponseWriter, r *http.Request, a *Admin) {
	docs, _ := x.Engine.Store.ListDocs(r.Context(), evalrun.DocRun, a.Tenant)
	var out []map[string]any
	for _, d := range docs {
		var run evalrun.Run
		if json.Unmarshal(d.Body, &run) == nil {
			s := summary(&run)
			delete(s, "report")
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["started"].(time.Time).After(out[j]["started"].(time.Time)) })
	writeJSON(w, 200, map[string]any{"runs": out})
}

func (x *API) getRun(w http.ResponseWriter, r *http.Request, a *Admin) {
	run := x.loadRun(w, r, a)
	if run == nil {
		return
	}
	out := summary(run)
	if r.URL.Query().Get("results") == "1" {
		out["results"] = run.Results
	}
	writeJSON(w, 200, out)
}

func (x *API) paretoSVG(w http.ResponseWriter, r *http.Request, a *Admin) {
	run := x.loadRun(w, r, a)
	if run == nil {
		return
	}
	var pts []eval.ParetoPoint
	if run.Report != nil {
		pts = run.Report.Pareto
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Write([]byte(eval.ParetoSVG(pts)))
}

func (x *API) recommend(w http.ResponseWriter, r *http.Request, a *Admin) {
	run := x.loadRun(w, r, a)
	if run == nil {
		return
	}
	var req struct {
		Policy        string   `json:"policy"`
		TargetQuality *float64 `json:"target_quality"`
	}
	_ = decode(r, &req)
	s := x.Engine.Snapshot()
	pol := s.Policies[req.Policy]
	if pol == nil {
		fail(w, api.CodeInvalidRequest, "unknown policy %q", req.Policy)
		return
	}
	target := 0.0
	if req.TargetQuality != nil {
		target = *req.TargetQuality
	} else if q, ok := evalrun.QualityOf(run, run.Baseline); ok {
		target = q - s.Config.Evals.GateMaxDrop
	} else if q, ok := evalrun.QualityOf(run, fmt.Sprintf("policy:%s@%d", pol.Name, pol.Version)); ok {
		target = q
	}
	rec, err := evalrun.Recommend(run, s, pol.Name, pol.Difficulty.Thresholds, target)
	if err != nil {
		fail(w, api.CodeInvalidRequest, "%v", err)
		return
	}
	if emb := s.Config.Evals.Embedder; emb != "" {
		if suite, err := x.runner().Suite(r.Context(), run.Suite); err == nil {
			if th, recall, err := evalrun.RecommendSemanticThreshold(r.Context(), x.Engine, emb, suite.Items); err == nil {
				rec.SemanticThreshold, rec.SemanticRecall = &th, recall
			}
		}
	}
	writeJSON(w, 200, rec)
}

// replay builds a suite from captured (opted-in) production bodies in a window and runs
// it against a candidate policy, comparing cost with what the originals cost [EVL-10].
func (x *API) replay(w http.ResponseWriter, r *http.Request, a *Admin) {
	var req struct {
		Tenant  string    `json:"tenant"`
		From    time.Time `json:"from"`
		To      time.Time `json:"to"`
		Policy  string    `json:"policy"`
		Version int       `json:"version"`
	}
	if err := decode(r, &req); err != nil {
		api.WriteError(w, api.FormatOpenAI, err)
		return
	}
	tenant, ok := x.tenantParam(w, a, req.Tenant)
	if !ok || tenant == "" {
		if ok {
			fail(w, api.CodeInvalidRequest, "tenant is required")
		}
		return
	}
	ctx := r.Context()
	x.Engine.Writer.Flush()
	recs, _ := x.Engine.Store.ListCostRecords(ctx, store.CostFilter{Tenant: tenant, From: req.From, To: req.To}, 0)
	grader := "non_empty"
	if x.Engine.Snapshot().Config.Evals.Embedder != "" {
		grader = "embedding"
	}
	var items []eval.Item
	var originalCost float64
	for _, rec := range recs {
		plain, err := x.Engine.OpenBody(ctx, rec.RequestID)
		if err != nil {
			continue
		}
		var body struct {
			Request  []map[string]string `json:"request"`
			Response string              `json:"response"`
		}
		if json.Unmarshal(plain, &body) != nil || len(body.Request) == 0 {
			continue
		}
		msgs := make([]map[string]any, len(body.Request))
		for i, m := range body.Request {
			msgs[i] = map[string]any{"role": m["role"], "content": m["content"]}
		}
		items = append(items, eval.Item{ID: rec.RequestID, Category: "replay", Messages: msgs, Reference: body.Response, Grader: grader})
		originalCost += rec.CostUSD
	}
	if len(items) == 0 {
		fail(w, api.CodeInvalidRequest, "no captured bodies in the window (body logging must be enabled for the tenant)")
		return
	}
	suite := &eval.Suite{ID: "replay-" + evalrun.NewRunID(), Name: "replay", Items: items, Created: time.Now().UTC()}
	_ = x.runner().SaveSuite(ctx, suite, tenant)
	run := &evalrun.Run{ID: evalrun.NewRunID(), Suite: suite.ID, Tenant: tenant, Status: "running", Started: time.Now().UTC(),
		Targets: []evalrun.Target{{Kind: "policy", Name: req.Policy, Version: req.Version}}}
	if err := x.runner().Execute(ctx, run, suite); err != nil {
		fail(w, api.CodeInternal, "%v", err)
		return
	}
	var newCost float64
	for _, res := range run.Results {
		newCost += res.CostUSD
	}
	out := map[string]any{"run": run.ID, "requests": len(items), "original_cost_usd": originalCost, "candidate_cost_usd": newCost,
		"report": run.Report}
	if originalCost > 0 {
		out["cost_change_pct"] = 100 * (newCost - originalCost) / originalCost
	}
	writeJSON(w, 200, out)
}

// ---------- policies [CFG-06, EVL-08] ----------

type versionView struct {
	Version        int    `json:"version"`
	Source         string `json:"source"`
	Active         bool   `json:"active"`
	EvalRun        string `json:"eval_run,omitempty"`
	OverrideReason string `json:"override_reason,omitempty"`
}

func (x *API) storedVersions(ctx context.Context, name string) map[int]store.PolicyVersion {
	out := map[int]store.PolicyVersion{}
	vs, _ := x.Engine.Store.ListPolicyVersions(ctx, name)
	for _, v := range vs {
		out[v.Version] = v
	}
	return out
}

func (x *API) listPolicies(w http.ResponseWriter, r *http.Request, _ *Admin) {
	s := x.Engine.Snapshot()
	names := make([]string, 0, len(s.AllPolicy))
	for n := range s.AllPolicy {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []map[string]any
	for _, n := range names {
		stored := x.storedVersions(r.Context(), n)
		var vs []versionView
		for _, p := range s.AllPolicy[n] {
			v := versionView{Version: p.Version, Source: "file", Active: s.Policies[n] != nil && s.Policies[n].Version == p.Version}
			if sv, ok := stored[p.Version]; ok {
				v.Source, v.EvalRun, v.OverrideReason = "api", sv.EvalRun, sv.OverrideReason
			}
			vs = append(vs, v)
		}
		sort.Slice(vs, func(i, j int) bool { return vs[i].Version < vs[j].Version })
		out = append(out, map[string]any{"name": n, "active": s.Policies[n], "versions": vs})
	}
	writeJSON(w, 200, map[string]any{"policies": out})
}

func (x *API) createPolicyVersion(w http.ResponseWriter, r *http.Request, a *Admin) {
	var p config.Policy
	if err := decode(r, &p); err != nil {
		api.WriteError(w, api.FormatOpenAI, err)
		return
	}
	s := x.Engine.Snapshot()
	if p.Name == "" {
		fail(w, api.CodeInvalidRequest, "name is required")
		return
	}
	next := 1
	for _, v := range s.AllPolicy[p.Name] {
		next = max(next, v.Version+1)
	}
	p.Version = next
	// Validate the new version against the running configuration.
	probe := *s.Config
	probe.Policies = append(append([]config.Policy(nil), s.Config.Policies...), p)
	config.ApplyDefaults(&probe)
	if err := config.Validate(&probe); err != nil {
		fail(w, api.CodeInvalidRequest, "%v", err)
		return
	}
	pv := probe.Policies[len(probe.Policies)-1]
	body, _ := json.Marshal(pv)
	// Keep the currently active version active: new versions start inactive.
	if cur := s.Policies[p.Name]; cur != nil {
		if _, stored := x.storedVersions(r.Context(), p.Name)[cur.Version]; !stored {
			cb, _ := json.Marshal(cur)
			_ = x.Engine.Store.PutPolicyVersion(r.Context(), store.PolicyVersion{Name: p.Name, Version: cur.Version, CreatedAt: time.Now().UTC(), Actor: "file", Body: cb, Active: true})
		}
	}
	if err := x.Engine.Store.PutPolicyVersion(r.Context(), store.PolicyVersion{Name: p.Name, Version: p.Version, CreatedAt: time.Now().UTC(), Actor: a.ID, Body: body}); err != nil {
		fail(w, api.CodeInternal, "%v", err)
		return
	}
	if err := x.rebuild(r.Context(), "admin:policy_version"); err != nil {
		fail(w, api.CodeInternal, "%v", err)
		return
	}
	x.audit(r.Context(), a, "", "create", "policy", fmt.Sprintf("%s@%d", p.Name, p.Version), nil, pv, "")
	writeJSON(w, 201, pv)
}

// latestQuality finds the most recent finished run containing label.
func (x *API) latestQuality(ctx context.Context, label, preferRun string) (float64, string, bool) {
	if preferRun != "" {
		if run, err := x.runner().Load(ctx, preferRun); err == nil {
			if q, ok := evalrun.QualityOf(run, label); ok {
				return q, run.ID, true
			}
		}
	}
	docs, _ := x.Engine.Store.ListDocs(ctx, evalrun.DocRun, "")
	var best *evalrun.Run
	for _, d := range docs {
		var run evalrun.Run
		if json.Unmarshal(d.Body, &run) != nil || run.Status != "done" {
			continue
		}
		if _, ok := evalrun.QualityOf(&run, label); ok && (best == nil || run.Started.After(best.Started)) {
			r := run
			best = &r
		}
	}
	if best == nil {
		return 0, "", false
	}
	q, _ := evalrun.QualityOf(best, label)
	return q, best.ID, true
}

func (x *API) activate(ctx context.Context, name string, version int, evalRun, override string) error {
	s := x.Engine.Snapshot()
	var target *config.Policy
	for _, p := range s.AllPolicy[name] {
		if p.Version == version {
			p := p
			target = &p
		}
	}
	if target == nil {
		return fmt.Errorf("policy %s has no version %d", name, version)
	}
	stored := x.storedVersions(ctx, name)
	for _, p := range s.AllPolicy[name] {
		if _, ok := stored[p.Version]; !ok {
			b, _ := json.Marshal(p)
			_ = x.Engine.Store.PutPolicyVersion(ctx, store.PolicyVersion{Name: name, Version: p.Version, CreatedAt: time.Now().UTC(), Actor: "file", Body: b,
				Active: s.Policies[name] != nil && s.Policies[name].Version == p.Version})
		}
	}
	sv, _ := x.storedVersions(ctx, name)[version]
	sv.EvalRun, sv.OverrideReason = evalRun, override
	_ = x.Engine.Store.PutPolicyVersion(ctx, sv)
	if err := x.Engine.Store.ActivatePolicyVersion(ctx, name, version); err != nil {
		return err
	}
	return x.rebuild(ctx, "admin:policy_activate")
}

// activatePolicy enforces the promotion gate: the candidate's latest eval quality must be
// within gate_max_drop of the active version's; an org admin may override with a
// recorded reason [EVL-08].
func (x *API) activatePolicy(w http.ResponseWriter, r *http.Request, a *Admin) {
	name := r.PathValue("name")
	var req struct {
		Version        int    `json:"version"`
		EvalRun        string `json:"eval_run"`
		OverrideReason string `json:"override_reason"`
	}
	if err := decode(r, &req); err != nil {
		api.WriteError(w, api.FormatOpenAI, err)
		return
	}
	s := x.Engine.Snapshot()
	active := s.Policies[name]
	if active == nil {
		fail(w, api.CodeNotFound, "policy %q not found", name)
		return
	}
	candLabel := fmt.Sprintf("policy:%s@%d", name, req.Version)
	activeLabel := fmt.Sprintf("policy:%s@%d", name, active.Version)
	cq, runID, haveCand := x.latestQuality(r.Context(), candLabel, req.EvalRun)
	aq, _, haveActive := x.latestQuality(r.Context(), activeLabel, runID)
	drop := s.Config.Evals.GateMaxDrop
	gate := map[string]any{"candidate": candLabel, "active": activeLabel, "max_drop": drop}
	passed := false
	switch {
	case !haveCand:
		gate["reason"] = "no eval run covers the candidate version"
	case !haveActive:
		gate["reason"] = "no eval run covers the active version"
	default:
		gate["candidate_quality"], gate["active_quality"], gate["eval_run"] = cq, aq, runID
		passed = cq >= aq-drop
		if !passed {
			gate["reason"] = fmt.Sprintf("quality %.2f is more than %.1f points below the active version's %.2f", cq, drop, aq)
		}
	}
	if !passed {
		if req.OverrideReason == "" || a.Role != "org_admin" {
			e := api.NewError(api.CodeConflict, "promotion gate: %v (an org admin may override with override_reason)", gate["reason"])
			e.Details = gate
			api.WriteError(w, api.FormatOpenAI, e)
			return
		}
		gate["overridden_by"] = a.ID
	}
	if err := x.activate(r.Context(), name, req.Version, runID, req.OverrideReason); err != nil {
		fail(w, api.CodeInvalidRequest, "%v", err)
		return
	}
	x.audit(r.Context(), a, "", "activate", "policy", candLabel, map[string]any{"active": activeLabel}, gate, req.OverrideReason)
	writeJSON(w, 200, map[string]any{"active": candLabel, "gate": gate})
}

// rollbackPolicy re-activates any prior version in one action [CFG-06].
func (x *API) rollbackPolicy(w http.ResponseWriter, r *http.Request, a *Admin) {
	name := r.PathValue("name")
	var req struct {
		Version int    `json:"version"`
		Reason  string `json:"reason"`
	}
	if err := decode(r, &req); err != nil {
		api.WriteError(w, api.FormatOpenAI, err)
		return
	}
	before := x.Engine.Snapshot().Policies[name]
	if before == nil {
		fail(w, api.CodeNotFound, "policy %q not found", name)
		return
	}
	if err := x.activate(r.Context(), name, req.Version, "", ""); err != nil {
		fail(w, api.CodeInvalidRequest, "%v", err)
		return
	}
	x.audit(r.Context(), a, "", "rollback", "policy", fmt.Sprintf("%s@%d", name, req.Version), map[string]any{"version": before.Version}, map[string]any{"version": req.Version}, req.Reason)
	writeJSON(w, 200, map[string]any{"active": fmt.Sprintf("%s@%d", name, req.Version), "previous": before.Version})
}

func (x *API) diffPolicy(w http.ResponseWriter, r *http.Request, _ *Admin) {
	s := x.Engine.Snapshot()
	name := r.PathValue("name")
	var from, to int
	fmt.Sscan(r.URL.Query().Get("from"), &from)
	fmt.Sscan(r.URL.Query().Get("to"), &to)
	get := func(v int) map[string]any {
		for _, p := range s.AllPolicy[name] {
			if p.Version == v {
				b, _ := json.Marshal(p)
				var m map[string]any
				_ = json.Unmarshal(b, &m)
				return m
			}
		}
		return nil
	}
	a, b := get(from), get(to)
	if a == nil || b == nil {
		fail(w, api.CodeNotFound, "versions not found")
		return
	}
	writeJSON(w, 200, map[string]any{"policy": name, "from": from, "to": to, "changes": diffMaps("", a, b)})
}

// diffMaps lists changed fields as path → {from, to}.
func diffMaps(prefix string, a, b map[string]any) map[string]any {
	out := map[string]any{}
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	for k := range keys {
		if k == "version" {
			continue
		}
		p := k
		if prefix != "" {
			p = prefix + "." + k
		}
		am, aok := a[k].(map[string]any)
		bm, bok := b[k].(map[string]any)
		if aok && bok {
			for kk, vv := range diffMaps(p, am, bm) {
				out[kk] = vv
			}
			continue
		}
		ja, _ := json.Marshal(a[k])
		jb, _ := json.Marshal(b[k])
		if !bytes.Equal(ja, jb) {
			out[p] = map[string]any{"from": a[k], "to": b[k]}
		}
	}
	return out
}

// quality aggregates online signals (client feedback and sampled judge grades) by
// deployment or policy [EVL-09].
func (x *API) quality(w http.ResponseWriter, r *http.Request, a *Admin) {
	group := r.URL.Query().Get("group_by")
	if group != "policy" {
		group = "deployment"
	}
	docs, _ := x.Engine.Store.ListDocs(r.Context(), "quality_signal", a.Tenant)
	type agg struct {
		Sum      float64 `json:"-"`
		N        int     `json:"signals"`
		Mean     float64 `json:"mean_score"`
		Feedback int     `json:"feedback"`
		Judged   int     `json:"judged"`
	}
	out := map[string]*agg{}
	for _, d := range docs {
		var sig map[string]any
		if json.Unmarshal(d.Body, &sig) != nil {
			continue
		}
		k, _ := sig[group].(string)
		sc, _ := sig["score"].(float64)
		g := out[k]
		if g == nil {
			g = &agg{}
			out[k] = g
		}
		g.Sum += sc
		g.N++
		if sig["source"] == "feedback" {
			g.Feedback++
		} else {
			g.Judged++
		}
		g.Mean = g.Sum / float64(g.N)
	}
	writeJSON(w, 200, map[string]any{"group_by": group, "quality": out})
}
