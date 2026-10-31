package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/eval"
	"github.com/SureshKhemka/llm-gateway/internal/evalrun"
	"github.com/SureshKhemka/llm-gateway/internal/sim"
	"github.com/SureshKhemka/llm-gateway/internal/store"
	"github.com/SureshKhemka/llm-gateway/internal/testenv"
	"github.com/SureshKhemka/llm-gateway/internal/workload"
)

const m7YAML = `
providers:
  - id: sima
    type: openai
    base_url: ${SIM_SIMA_URL}
policies:
  - name: default
    objective: min_cost
  - name: strongest
    objective: max_quality
  - name: experiment
    objective: min_cost
    split:
      - {policy: default, weight: 50}
      - {policy: strongest, weight: 50}
  - name: with-shadow
    objective: min_cost
    shadow: {policy: strongest, sample_rate: 1.0}
virtual_models:
  - {name: experiment, policy: experiment}
  - {name: shadowed, policy: with-shadow}
evals:
  judge: sima/qwen-27b
  embedder: sima/nomic-embed
  gate_max_drop: 2
  online_sample_rate: 1.0
  grader_webhook: ${GRADER_URL}
  webhook_allow: [127.0.0.1]
storage:
  body_key: env:BODY_KEY
budgets:
  - {id: acme-eval, scope: eval, scope_id: acme, period: monthly, hard_usd: 50}
  - {id: acme-shadow, scope: shadow, scope_id: acme, period: monthly, hard_usd: 5}
  - {id: acme-main, scope: tenant, scope_id: acme, period: monthly, hard_usd: 50}
tenants:
  - id: acme
    body_logging: {enabled: true}
    projects: [{id: web, keys: [{id: k1, secret: env:TEST_KEY_1}]}]
admins:
  - {id: org, role: org_admin, secret: env:ADMIN_ORG}
  - {id: ta, role: tenant_admin, tenant: acme, secret: env:ADMIN_A}
defaults:
  baseline: sima/qwen-27b
` + testenv.StandardDeployments

func m7Env(t *testing.T, graderURL string) *testenv.Env {
	if graderURL == "" {
		graderURL = "http://127.0.0.1:1/grade"
	}
	env := testenv.New(t, testenv.Options{YAML: m7YAML, Env: map[string]string{"TEST_KEY_1": key1, "ADMIN_ORG": adminOrg, "ADMIN_A": adminA,
		"BODY_KEY": "body-key-0123456789abcdef", "GRADER_URL": graderURL}})
	items, _ := workload.Load("../../testdata/corpus/corpus.jsonl")
	oracle := map[string]sim.OracleItem{}
	for _, it := range items {
		if it.Reference != "" {
			oracle[it.Prompt] = sim.OracleItem{Reference: it.Reference, Difficulty: it.Difficulty}
		}
	}
	env.Sims["sima"].LoadOracle(oracle)
	return env
}

// suiteFromCorpus picks every nth corpus item (plus their paraphrases) as an eval suite.
func suiteFromCorpus(t *testing.T, n int) []eval.Item {
	items, _ := workload.Load("../../testdata/corpus/corpus.jsonl")
	var out []eval.Item
	for i, it := range items {
		if i%n != 0 && !it.Paraphrase || it.Category == "long_context" {
			continue
		}
		b, _ := json.Marshal(it)
		var e eval.Item
		_ = json.Unmarshal(b, &e)
		out = append(out, e)
	}
	return out
}

func (e *m4) dummy() {}

func startRun(t *testing.T, env *testenv.Env, body map[string]any) *evalrun.Run {
	t.Helper()
	body["wait"] = true
	r := env.Post("/admin/v1/evals/runs", adminOrg, body)
	if r.Status != 200 {
		t.Fatalf("eval run: %d %s", r.Status, r.Body)
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = r.JSON(&out)
	run, err := (&evalrun.Runner{Engine: env.Engine}).Load(context.Background(), out.ID)
	if err != nil || run.Status != "done" {
		t.Fatalf("run %s: %v %+v", out.ID, err, run)
	}
	return run
}

func targetSummary(run *evalrun.Run, label string) evalrun.TargetSummary {
	for _, t := range run.Report.Targets {
		if t.Target == label {
			return t
		}
	}
	return evalrun.TargetSummary{}
}

// Scenario 13: run the eval suite across all deployments; accept the recommended
// thresholds; a worse policy is blocked by the promotion gate; rollback works.
// Covers: EVL-01, EVL-02, EVL-05, EVL-06, EVL-07, EVL-08, CFG-06, EVL-11
func TestScenario13_EvalDrivenTuning(t *testing.T) {
	env := m7Env(t, "")
	items := suiteFromCorpus(t, 4)
	if r := env.Post("/admin/v1/evals/suites", adminOrg, map[string]any{"id": "ref", "name": "reference", "items": items}); r.Status != 201 {
		t.Fatalf("suite: %d %s", r.Status, r.Body)
	}
	// EVL-01: JSONL import with validation.
	jsonl := `{"id":"a","prompt":"hi","grader":"non_empty"}` + "\n" + `{"id":"a","prompt":"dup","grader":"non_empty"}`
	if r := env.Do("POST", "/admin/v1/evals/suites?id=bad", adminOrg, jsonl, map[string]string{"Content-Type": "application/jsonl"}); r.Status != 400 {
		t.Fatalf("duplicate ids accepted: %d", r.Status)
	}
	run := startRun(t, env, map[string]any{"suite": "ref", "tenant": "acme", "all_deployments": true,
		"targets":  []map[string]any{{"kind": "policy", "name": "default"}, {"kind": "policy", "name": "strongest"}},
		"baseline": "policy:strongest@1", "concurrency": 16})
	rep := run.Report
	if len(rep.Targets) < 7 {
		t.Fatalf("targets in report: %d", len(rep.Targets))
	}
	frontier := 0
	for _, p := range rep.Pareto {
		if p.Frontier {
			frontier++
		}
	}
	def := targetSummary(run, "policy:default@1")
	base := targetSummary(run, "policy:strongest@1")
	t.Logf("default: quality %.1f [%.1f, %.1f] $%.4f | strongest: %.1f $%.4f | Δ %+.2f [%+.2f, %+.2f], cost %+.1f%% | frontier %d points",
		def.Quality, def.QualityLo, def.QualityHi, def.CostUSD, base.Quality, base.CostUSD,
		def.VsBaseline.Quality, def.VsBaseline.Lo, def.VsBaseline.Hi, def.VsBaseline.CostPct, frontier)
	if frontier == 0 || def.VsBaseline == nil || def.QualityLo > def.Quality || len(def.ByCategory) < 8 || len(rep.ByCat) < 8 {
		t.Fatalf("report incomplete: %+v", def)
	}
	if acc := rep.Accuracy["policy:default@1"]; acc < 0.7 || len(rep.Confusion["policy:default@1"]) == 0 {
		t.Fatalf("estimator confusion/accuracy missing: %v", rep.Accuracy)
	}
	if r := env.Do("GET", "/admin/v1/evals/runs/"+run.ID+"/pareto.svg", adminOrg, nil, nil); r.Status != 200 || !strings.HasPrefix(string(r.Body), "<svg") {
		t.Fatalf("pareto svg: %d", r.Status)
	}
	// EVL-11: eval spend is tagged and charged to the eval budget only.
	env.Flush()
	recs, _ := env.Store.ListCostRecords(context.Background(), store.CostFilter{EvalRun: run.ID}, 0)
	if len(recs) != len(run.Results) {
		t.Fatalf("eval cost records %d, results %d", len(recs), len(run.Results))
	}
	budgets := map[string]float64{}
	var bl struct {
		Budgets []struct {
			Budget struct{ ID string }
			Status struct {
				SpentUSD float64 `json:"spent_usd"`
			}
		}
	}
	_ = env.Do("GET", "/admin/v1/budgets", adminOrg, nil, nil).JSON(&bl)
	for _, b := range bl.Budgets {
		budgets[b.Budget.ID] = b.Status.SpentUSD
	}
	if budgets["acme-eval"] <= 0 || budgets["acme-main"] != 0 {
		t.Fatalf("eval spend charged to %v", budgets)
	}

	// EVL-07: accept the recommendation as a new version.
	r := env.Post("/admin/v1/evals/runs/"+run.ID+"/recommend", adminOrg, map[string]any{"policy": "default"})
	var rec evalrun.Recommendation
	if r.Status != 200 || r.JSON(&rec) != nil || len(rec.Thresholds) != 3 || rec.SemanticThreshold == nil {
		t.Fatalf("recommendation: %d %s", r.Status, r.Body)
	}
	t.Logf("recommended thresholds %v (predicted quality %.1f, $%.4f) vs current %v (%.1f, $%.4f); semantic %.2f (recall %.0f%%)",
		rec.Thresholds, rec.PredictedQuality, rec.PredictedCostUSD, rec.Current, rec.CurrentQuality, rec.CurrentCostUSD, *rec.SemanticThreshold, 100*rec.SemanticRecall)
	if rec.PredictedCostUSD > rec.CurrentCostUSD+1e-9 || rec.PredictedQuality < rec.TargetQuality {
		t.Fatalf("recommendation does not improve cost at target quality: %+v", rec)
	}
	r = env.Post("/admin/v1/policies", adminOrg, map[string]any{"name": "default", "objective": "min_cost", "difficulty": map[string]any{"thresholds": rec.Thresholds}})
	if r.Status != 201 || !strings.Contains(string(r.Body), `"version": 2`) {
		t.Fatalf("new version: %d %s", r.Status, r.Body)
	}
	if v := env.Chat(key1, "auto", "hello", nil).Header.Get("X-Gw-Policy-Version"); v != "1" {
		t.Fatalf("new version became active before promotion: %s", v)
	}
	// Activating without an eval is blocked; after an eval it passes the gate.
	if r := env.Post("/admin/v1/policies/default/activate", adminOrg, map[string]any{"version": 2}); r.Status != 409 {
		t.Fatalf("activation without eval: %d %s", r.Status, r.Body)
	}
	run2 := startRun(t, env, map[string]any{"suite": "ref", "tenant": "acme", "concurrency": 16,
		"targets": []map[string]any{{"kind": "policy", "name": "default", "version": 1}, {"kind": "policy", "name": "default", "version": 2}}})
	v1, v2 := targetSummary(run2, "policy:default@1"), targetSummary(run2, "policy:default@2")
	t.Logf("v1 quality %.1f $%.4f, v2 quality %.1f $%.4f", v1.Quality, v1.CostUSD, v2.Quality, v2.CostUSD)
	if r := env.Post("/admin/v1/policies/default/activate", adminOrg, map[string]any{"version": 2, "eval_run": run2.ID}); r.Status != 200 {
		t.Fatalf("gate should pass for v2: %d %s", r.Status, r.Body)
	}
	if v := env.Chat(key1, "auto", "hello again", nil).Header.Get("X-Gw-Policy-Version"); v != "2" {
		t.Fatalf("v2 not active: %s", v)
	}
	// A worse version (everything to tier 0) is blocked by the gate.
	env.Post("/admin/v1/policies", adminOrg, map[string]any{"name": "default", "objective": "min_cost", "difficulty": map[string]any{"thresholds": []float64{0.97, 0.98, 0.99}}})
	run3 := startRun(t, env, map[string]any{"suite": "ref", "tenant": "acme", "concurrency": 16,
		"targets": []map[string]any{{"kind": "policy", "name": "default", "version": 2}, {"kind": "policy", "name": "default", "version": 3}}})
	t.Logf("v3 quality %.1f vs v2 %.1f", targetSummary(run3, "policy:default@3").Quality, targetSummary(run3, "policy:default@2").Quality)
	r = env.Post("/admin/v1/policies/default/activate", adminOrg, map[string]any{"version": 3, "eval_run": run3.ID})
	if r.Status != 409 || !strings.Contains(string(r.Body), "below the active version") {
		t.Fatalf("worse version not blocked: %d %s", r.Status, r.Body)
	}
	if r := env.Post("/admin/v1/policies/default/activate", adminA, map[string]any{"version": 3, "override_reason": "trust me"}); r.Status != 403 {
		t.Fatalf("tenant admin override: %d", r.Status)
	}
	// Rollback to any prior version in one action.
	if r := env.Post("/admin/v1/policies/default/rollback", adminOrg, map[string]any{"version": 1, "reason": "back to baseline"}); r.Status != 200 {
		t.Fatalf("rollback: %d %s", r.Status, r.Body)
	}
	if v := env.Chat(key1, "auto", "after rollback", nil).Header.Get("X-Gw-Policy-Version"); v != "1" {
		t.Fatalf("rollback not effective: %s", v)
	}
	var pl struct {
		Policies []struct {
			Name     string
			Versions []struct {
				Version int
				Active  bool
			}
		}
	}
	_ = env.Do("GET", "/admin/v1/policies", adminOrg, nil, nil).JSON(&pl)
	for _, p := range pl.Policies {
		if p.Name == "default" && len(p.Versions) != 3 {
			t.Fatalf("all versions retained: %+v", p)
		}
	}
	// Overrides need an org admin and a reason, and are audited.
	if r := env.Post("/admin/v1/policies/default/activate", adminOrg, map[string]any{"version": 3, "eval_run": run3.ID, "override_reason": "incident drill"}); r.Status != 200 {
		t.Fatalf("org admin override: %d", r.Status)
	}
	var audit struct {
		Entries []store.AuditEntry `json:"entries"`
	}
	_ = env.Do("GET", "/admin/v1/audit?kind=policy", adminOrg, nil, nil).JSON(&audit)
	if len(audit.Entries) == 0 || audit.Entries[0].Reason != "incident drill" {
		t.Fatalf("override not audited: %+v", audit.Entries)
	}
}

// Covers: EVL-03, EVL-04
func TestEVL03_Graders(t *testing.T) {
	grader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Answer string `json:"answer"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		score := 0.0
		if strings.Contains(p.Answer, "Paris") {
			score = 1
		}
		_ = json.NewEncoder(w).Encode(map[string]float64{"score": score})
	}))
	defer grader.Close()
	env := m7Env(t, grader.URL)
	env.Sims["sima"].LoadOracle(map[string]sim.OracleItem{"capital of France?": {Reference: "Paris", Difficulty: 0}})
	items := []map[string]any{
		{"id": "exact", "prompt": "capital of France?", "reference": "Paris", "grader": "exact"},
		{"id": "regex", "prompt": "capital of France?", "reference": "^Par", "grader": "regex"},
		{"id": "schema", "prompt": "Asha is 34", "schema": json.RawMessage(personSchemaJSON), "grader": "json_schema"},
		{"id": "embedding", "prompt": "capital of France?", "reference": "Paris", "grader": "embedding"},
		{"id": "judge", "prompt": "capital of France?", "reference": "Paris", "rubric": "names the city", "grader": "llm_judge"},
		{"id": "webhook", "prompt": "capital of France?", "grader": "webhook"},
	}
	env.Post("/admin/v1/evals/suites", adminOrg, map[string]any{"id": "graders", "items": items})
	run := startRun(t, env, map[string]any{"suite": "graders", "tenant": "acme", "targets": []map[string]any{{"kind": "deployment", "name": "sima/qwen-9b"}}})
	for _, r := range run.Results {
		// The judge scores on a 0–10 scale; the simulator's judge gives a correct answer 9.
		if r.Score < 0.85 {
			t.Errorf("grader %s scored %.2f (%s)", r.ItemID, r.Score, r.Error)
		}
	}
}

const personSchemaJSON = `{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"integer"}},"required":["name","age"]}`

// Covers: RTE-12
func TestRTE12_ABSplitStickyByEndUser(t *testing.T) {
	env := m7Env(t, "")
	variants := map[string]int{}
	for i := 0; i < 40; i++ {
		user := fmt.Sprintf("user-%d", i)
		first := ""
		for j := 0; j < 3; j++ {
			r := env.Chat(key1, "experiment", "hello", nil, map[string]string{"X-Gw-End-User": user})
			env.Flush()
			var dec struct {
				SplitVariant string `json:"split_variant"`
			}
			_ = env.Do("GET", "/v1/route/decisions/"+r.Header.Get("X-Request-Id"), key1, nil, nil).JSON(&dec)
			if j == 0 {
				first = dec.SplitVariant
				variants[first]++
			} else if dec.SplitVariant != first {
				t.Fatalf("user %s switched variant %s → %s", user, first, dec.SplitVariant)
			}
		}
	}
	if variants["default"] < 10 || variants["strongest"] < 10 {
		t.Fatalf("50/50 split skewed: %v", variants)
	}
}

// Covers: RTE-13
func TestRTE13_ShadowTraffic(t *testing.T) {
	env := m7Env(t, "")
	r := env.Chat(key1, "shadowed", "hello shadow", nil)
	if r.Status != 200 || r.Header.Get("X-Gw-Served-Model") == "sima/qwen-27b" {
		t.Fatalf("primary: %d %s", r.Status, r.Header.Get("X-Gw-Served-Model"))
	}
	env.Engine.WaitShadows()
	env.Flush()
	all, _ := env.Store.ListCostRecords(context.Background(), store.CostFilter{IncludeShadow: true}, 0)
	var shadow *store.CostRecord
	for i := range all {
		if all[i].Shadow {
			shadow = &all[i]
		}
	}
	if shadow == nil || shadow.Deployment != "sima/qwen-27b" || shadow.CostUSD <= 0 || !strings.Contains(strings.Join(shadow.Tags, ","), "shadow-of:"+r.Header.Get("X-Request-Id")) {
		t.Fatalf("shadow record: %+v", shadow)
	}
	// Shadow spend is excluded from normal reports and charged to the shadow budget.
	visible, _ := env.Store.ListCostRecords(context.Background(), store.CostFilter{}, 0)
	for _, v := range visible {
		if v.Shadow {
			t.Fatal("shadow record in default reports")
		}
	}
	var bl struct {
		Budgets []struct {
			Budget struct{ ID string }
			Status struct {
				SpentUSD float64 `json:"spent_usd"`
			}
		}
	}
	_ = env.Do("GET", "/admin/v1/budgets", adminOrg, nil, nil).JSON(&bl)
	for _, b := range bl.Budgets {
		if b.Budget.ID == "acme-shadow" && b.Status.SpentUSD < shadow.CostUSD-1e-12 {
			t.Fatalf("shadow budget not charged: %+v", b)
		}
	}
}

// Covers: EVL-09
func TestEVL09_OnlineQualitySignals(t *testing.T) {
	env := m7Env(t, "")
	r := env.Chat(key1, "sima/qwen-4b", "rate me", nil)
	id := r.Header.Get("X-Request-Id")
	if fb := env.Post("/v1/feedback", key1, map[string]any{"request_id": id, "score": 0.25, "comment": "meh"}); fb.Status != 202 {
		t.Fatalf("feedback: %d %s", fb.Status, fb.Body)
	}
	if fb := env.Post("/v1/feedback", key1, map[string]any{"request_id": "req_unknown", "score": 1}); fb.Status != 404 {
		t.Fatalf("feedback for unknown request: %d", fb.Status)
	}
	env.Engine.WaitShadows()
	var q struct {
		Quality map[string]struct {
			Signals  int     `json:"signals"`
			Mean     float64 `json:"mean_score"`
			Feedback int     `json:"feedback"`
			Judged   int     `json:"judged"`
		} `json:"quality"`
	}
	_ = env.Do("GET", "/admin/v1/quality?group_by=deployment", adminOrg, nil, nil).JSON(&q)
	g := q.Quality["sima/qwen-4b"]
	if g.Feedback != 1 || g.Judged != 1 || g.Signals != 2 {
		t.Fatalf("quality by route: %+v", q.Quality)
	}
}

// Covers: EVL-10
func TestEVL10_ReplayCapturedTrafficAgainstCandidate(t *testing.T) {
	env := m7Env(t, "")
	from := time.Now().Add(-time.Minute)
	for _, p := range []string{"What are the opening hours of Lotus Bakery?", "Classify the sentiment: great product", "hello there"} {
		env.Chat(key1, "auto", p, nil)
	}
	env.Flush()
	r := env.Post("/admin/v1/evals/replay", adminOrg, map[string]any{"tenant": "acme", "from": from, "to": time.Now().Add(time.Minute), "policy": "strongest"})
	var out struct {
		Requests        int     `json:"requests"`
		OriginalCostUSD float64 `json:"original_cost_usd"`
		CandidateCost   float64 `json:"candidate_cost_usd"`
		CostChangePct   float64 `json:"cost_change_pct"`
	}
	if r.Status != 200 || r.JSON(&out) != nil || out.Requests != 3 || out.CandidateCost <= out.OriginalCostUSD {
		t.Fatalf("replay: %d %s", r.Status, r.Body)
	}
	t.Logf("replay of %d captured requests on 'strongest': cost %+.0f%%", out.Requests, out.CostChangePct)
}

// Covers: CFG-05
func TestCFG05_PolicyDiff(t *testing.T) {
	env := m7Env(t, "")
	env.Post("/admin/v1/policies", adminOrg, map[string]any{"name": "default", "objective": "min_latency"})
	r := env.Do("GET", "/admin/v1/policies/default/diff?from=1&to=2", adminOrg, nil, nil)
	if r.Status != 200 || !strings.Contains(string(r.Body), `"objective"`) || !strings.Contains(string(r.Body), "min_latency") {
		t.Fatalf("diff: %d %s", r.Status, r.Body)
	}
}
