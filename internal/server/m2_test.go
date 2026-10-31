package server_test

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/gateway"
	"github.com/SureshKhemka/llm-gateway/internal/governance"
	"github.com/SureshKhemka/llm-gateway/internal/sim"
	"github.com/SureshKhemka/llm-gateway/internal/store"
	"github.com/SureshKhemka/llm-gateway/internal/testenv"
	"github.com/SureshKhemka/llm-gateway/internal/workload"
)

const m2YAML = `
providers:
  - id: sima
    type: openai
    base_url: ${SIM_SIMA_URL}
policies:
  - name: baseline-t3
    description: always the strongest eligible deployment (tier 3; vision model for images)
    objective: max_quality
  - name: fast
    objective: min_latency
  - name: quality
    objective: max_quality
    max_cost_usd: 0.01
  - name: balanced
    objective: weighted
    weights: {cost: 1, latency: 1, quality: 2}
  - name: tenant-pol
    objective: min_cost
    min_tier: 1
  - name: key-pol
    objective: min_cost
    min_tier: 2
  - name: capped
    objective: min_cost
    max_cost_usd: 0.0005
    denied_providers: [nobody]
virtual_models:
  - name: baseline
    policy: baseline-t3
  - name: fast
    policy: fast
  - name: quality
    policy: quality
  - name: balanced
    policy: balanced
  - name: capped
    policy: capped
tenants:
  - id: acme
    projects:
      - id: web
        keys:
          - id: k1
            secret: env:TEST_KEY_1
  - id: globex
    policy: tenant-pol
    projects:
      - id: g-web
        keys:
          - id: kg
            secret: env:TEST_KEY_2
          - id: kg2
            secret: env:TEST_KEY_3
            scopes: {policy: key-pol}
defaults:
  baseline: sima/qwen-27b
` + testenv.StandardDeployments

const key3 = "gw-test-k3_cccccccccccccccccccccccccccccccc"

func m2Env(t testing.TB) *testenv.Env {
	return testenv.New(t, testenv.Options{YAML: m2YAML, Env: map[string]string{"TEST_KEY_1": key1, "TEST_KEY_2": key2, "TEST_KEY_3": key3}})
}

func explain(t testing.TB, env *testenv.Env, key string, body map[string]any, headers ...map[string]string) (int, *gateway.Decision) {
	t.Helper()
	r := env.Post("/v1/route/explain", key, body, headers...)
	var out struct {
		Decision gateway.Decision `json:"decision"`
	}
	_ = r.JSON(&out)
	return r.Status, &out.Decision
}

func userMsg(model, prompt string) map[string]any {
	return map[string]any{"model": model, "messages": []map[string]any{{"role": "user", "content": prompt}}}
}

func tierOf(env *testenv.Env, dep string) int {
	return env.Engine.Snapshot().Catalog.Deployments[dep].Tier
}

// Scenario 2: a greeting, an extraction and a multi-step code task sent to `auto` land
// on tier 0, tier 1 and tier 2–3; route-explain shows why.
// Covers: RTE-03, RTE-05, API-07, RTE-15
func TestScenario02_DifficultyRouting(t *testing.T) {
	env := m2Env(t)
	cases := []struct {
		prompt     string
		minT, maxT int
	}{
		{"Hi there!", 0, 0},
		{"Extract the invoice number and total from: Invoice INV-2041 for Kaveri Foods, total due ₹12,400.", 1, 1},
		{"Here is a Go service:\n```go\nfunc handler(w http.ResponseWriter, r *http.Request) { db.Query(r.URL.Query().Get(\"q\")) }\n```\n" +
			"Refactor this code to use prepared statements, then explain step by step why the original is vulnerable, and finally write unit tests.", 2, 3},
	}
	for _, c := range cases {
		r := env.Chat(key1, "auto", c.prompt, nil)
		if r.Status != 200 {
			t.Fatalf("%q: %d %s", c.prompt, r.Status, r.Body)
		}
		served := r.Header.Get("X-Gw-Served-Model")
		if tier := tierOf(env, served); tier < c.minT || tier > c.maxT {
			t.Errorf("%q served by %s (tier %d), want tier %d–%d", c.prompt[:20], served, tier, c.minT, c.maxT)
		}
		st, dec := explain(t, env, key1, userMsg("auto", c.prompt))
		if st != 200 || dec.Chosen != served || len(dec.DifficultySignals) == 0 && c.minT > 0 || dec.Reason == "" {
			t.Errorf("explain disagrees or lacks reasons: status %d chosen %s served %s %+v", st, dec.Chosen, served, dec)
		}
	}
}

// Scenario 12: an image, tools and an over-long prompt go to vision, tool-capable and
// long-context deployments.
// Covers: RTE-01, RTE-02
func TestScenario12_CapabilityRouting(t *testing.T) {
	env := m2Env(t)
	img := map[string]any{"model": "auto", "messages": []map[string]any{{"role": "user", "content": []map[string]any{
		{"type": "text", "text": "hi, what is this?"}, {"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}}}}}}
	r := env.Post("/v1/chat/completions", key1, img)
	if r.Header.Get("X-Gw-Served-Model") != "sima/qwen-vl-8b" {
		t.Errorf("image request served by %q", r.Header.Get("X-Gw-Served-Model"))
	}
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "f", "parameters": map[string]any{"type": "object"}}}}
	r = env.Chat(key1, "auto", "hello", map[string]any{"tools": tools})
	served := r.Header.Get("X-Gw-Served-Model")
	if d := env.Engine.Snapshot().Catalog.Deployments[served]; d == nil || !d.Has("tools") {
		t.Errorf("tool request served by %q", served)
	}
	long := strings.Repeat("lorem ipsum dolor sit amet ", 4000) + " hello?"
	r = env.Chat(key1, "auto", long, nil)
	served = r.Header.Get("X-Gw-Served-Model")
	if d := env.Engine.Snapshot().Catalog.Deployments[served]; d == nil || d.ContextWindow < 20000 {
		t.Errorf("long prompt served by %q", served)
	}
	_, dec := explain(t, env, key1, userMsg("auto", long))
	excluded := map[string]string{}
	for _, ex := range dec.Exclusions {
		excluded[ex.Deployment] = strings.Join(ex.Reasons, ",")
	}
	if !strings.Contains(excluded["sima/gemma-e2b"], "context_window") || !strings.Contains(excluded["sima/nomic-embed"], "embeddings_only") {
		t.Errorf("exclusion reasons: %v", excluded)
	}
}

// Covers: RTE-02, API-10
func TestRTE02_NoEligibleModelListsReasons(t *testing.T) {
	env := m2Env(t)
	// Vision + tools: no deployment has both.
	body := map[string]any{"model": "auto", "tools": []map[string]any{{"type": "function", "function": map[string]any{"name": "f"}}},
		"messages": []map[string]any{{"role": "user", "content": []map[string]any{{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA"}}}}}}
	r := env.Post("/v1/chat/completions", key1, body)
	var e struct {
		Error struct {
			Code    string              `json:"code"`
			Details []gateway.Exclusion `json:"details"`
		} `json:"error"`
	}
	_ = r.JSON(&e)
	if r.Status != 422 || e.Error.Code != api.CodeNoEligibleModel || len(e.Error.Details) < 5 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	for _, ex := range e.Error.Details {
		if len(ex.Reasons) == 0 {
			t.Errorf("exclusion without reason: %+v", ex)
		}
	}
}

// Covers: RTE-05
func TestRTE05_Objectives(t *testing.T) {
	env := m2Env(t)
	hard := "Explain step by step why this proof is wrong and derive the correct bound, then analyze the trade-offs."
	// min_cost picks the cheapest deployment at the difficulty tier.
	_, dec := explain(t, env, key1, userMsg("auto", hard))
	if dec.Objective != "min_cost" || tierOf(env, dec.Chosen) < dec.MinTier {
		t.Errorf("min_cost: %+v", dec)
	}
	costs := map[string]float64{}
	for _, c := range dec.Candidates {
		costs[c.Deployment] = c.EstCostUSD
	}
	for _, c := range dec.Candidates {
		if c.Tier >= dec.MinTier && c.EstCostUSD < costs[dec.Chosen] {
			t.Errorf("cheaper eligible candidate %s ignored", c.Deployment)
		}
	}
	// min_latency picks the fastest.
	_, dec = explain(t, env, key1, userMsg("fast", "hello"))
	sort.Slice(dec.Candidates, func(i, j int) bool { return dec.Candidates[i].EstLatencyMs < dec.Candidates[j].EstLatencyMs })
	if dec.Chosen != dec.Candidates[0].Deployment {
		t.Errorf("min_latency chose %s, fastest is %s", dec.Chosen, dec.Candidates[0].Deployment)
	}
	// max_quality picks the highest tier within max cost.
	_, dec = explain(t, env, key1, userMsg("quality", "hello"))
	if tierOf(env, dec.Chosen) != 3 {
		t.Errorf("max_quality chose %s", dec.Chosen)
	}
	// weighted produces scores and picks the best.
	_, dec = explain(t, env, key1, userMsg("balanced", "hello"))
	if dec.Objective != "weighted" || dec.Candidates[0].Deployment != dec.Chosen {
		t.Errorf("weighted: %+v", dec)
	}
	// Latency target filters slow candidates.
	_, dec = explain(t, env, key1, userMsg("auto", hard), map[string]string{"X-Gw-Latency-Target-Ms": "5000"})
	for _, c := range dec.Candidates {
		if c.EstLatencyMs > 5000 && c.Deployment == dec.Chosen {
			t.Errorf("chosen candidate misses latency target: %+v", c)
		}
	}
}

// Covers: RTE-09
func TestRTE09_PolicyPrecedence(t *testing.T) {
	env := m2Env(t)
	_, dec := explain(t, env, key1, userMsg("auto", "hi"))
	if dec.Policy != "default" || dec.PolicySource != "default" {
		t.Errorf("acme: %s/%s", dec.Policy, dec.PolicySource)
	}
	_, dec = explain(t, env, key2, userMsg("auto", "hi"))
	if dec.Policy != "tenant-pol" || dec.PolicySource != "tenant" || tierOf(env, dec.Chosen) < 1 {
		t.Errorf("tenant policy: %s/%s %s", dec.Policy, dec.PolicySource, dec.Chosen)
	}
	_, dec = explain(t, env, key3, userMsg("auto", "hi"))
	if dec.Policy != "key-pol" || dec.PolicySource != "key" || tierOf(env, dec.Chosen) < 2 {
		t.Errorf("key policy should beat tenant policy: %s/%s %s", dec.Policy, dec.PolicySource, dec.Chosen)
	}
	// A specific virtual model carries its own policy.
	_, dec = explain(t, env, key3, userMsg("fast", "hi"))
	if dec.Policy != "fast" || dec.PolicySource != "virtual_model" {
		t.Errorf("virtual model policy: %s/%s", dec.Policy, dec.PolicySource)
	}
	// The policy version is reported on responses.
	r := env.Chat(key1, "auto", "hi", nil)
	if r.Header.Get("X-Gw-Policy") != "default" || r.Header.Get("X-Gw-Policy-Version") != "1" {
		t.Errorf("policy headers: %v", r.Header)
	}
}

// Covers: RTE-10, API-08
func TestRTE10_HintsNarrowButNeverWiden(t *testing.T) {
	env := m2Env(t)
	// Narrowing: a lower max cost excludes expensive deployments.
	_, dec := explain(t, env, key1, userMsg("auto", "hello"), map[string]string{"X-Gw-Max-Cost-Usd": "0.00002"})
	for _, c := range dec.Candidates {
		if c.EstCostUSD > 0.00002 {
			t.Errorf("hint max cost ignored: %+v", c)
		}
	}
	// Widening: a higher max cost than the policy's does not admit pricier models.
	_, dec = explain(t, env, key1, userMsg("capped", "hello"), map[string]string{"X-Gw-Max-Cost-Usd": "100"})
	for _, c := range dec.Candidates {
		if c.EstCostUSD > 0.0005 {
			t.Errorf("hint widened policy max cost: %+v", c)
		}
	}
	// Body metadata works like headers.
	body := userMsg("auto", "hello")
	body["metadata"] = map[string]any{"gateway": map[string]any{"min_tier": 3, "tags": []string{"exp-1"}}}
	_, dec = explain(t, env, key1, body)
	if tierOf(env, dec.Chosen) != 3 {
		t.Errorf("metadata min_tier hint ignored: %s", dec.Chosen)
	}
	// An allow-list hint can only narrow: naming an unknown provider leaves nothing.
	st, _ := explain(t, env, key1, userMsg("auto", "hello"), map[string]string{"X-Gw-Allow-Providers": "elsewhere"})
	if st != 422 {
		t.Errorf("allow-list hint for a non-existent provider should leave no candidates, got %d", st)
	}
	// Required capability hint narrows.
	_, dec = explain(t, env, key1, userMsg("auto", "hello"), map[string]string{"X-Gw-Capabilities": "reasoning"})
	if d := env.Engine.Snapshot().Catalog.Deployments[dec.Chosen]; !d.Has("reasoning") {
		t.Errorf("capability hint ignored: %s", dec.Chosen)
	}
}

// Covers: RTE-14, RTE-15
func TestRTE14_DecisionRecordAndExplainIsFree(t *testing.T) {
	env := m2Env(t)
	r := env.Chat(key1, "auto", "Extract the order id from: order #A-77 shipped", nil)
	id := r.Header.Get("X-Request-Id")
	env.Flush()
	d := env.Do("GET", "/v1/route/decisions/"+id, key1, nil, nil)
	var dec gateway.Decision
	if d.Status != 200 || d.JSON(&dec) != nil {
		t.Fatalf("decision lookup: %d %s", d.Status, d.Body)
	}
	if dec.Chosen != r.Header.Get("X-Gw-Served-Model") || len(dec.Candidates) == 0 || len(dec.Attempts) != 1 || dec.Policy == "" || len(dec.Exclusions) == 0 {
		t.Fatalf("decision record incomplete: %+v", dec)
	}
	// Other tenants cannot read it.
	if d := env.Do("GET", "/v1/route/decisions/"+id, key2, nil, nil); d.Status != 404 {
		t.Errorf("cross-tenant decision read: %d", d.Status)
	}
	// Explain calls no model and records no cost.
	before := env.Sims["sima"].Requests.Load()
	env.Flush()
	recsBefore, _ := env.Store.ListCostRecords(context.Background(), storeFilterAll(), 0)
	st, _ := explain(t, env, key1, userMsg("auto", "hello"))
	env.Flush()
	recsAfter, _ := env.Store.ListCostRecords(context.Background(), storeFilterAll(), 0)
	if st != 200 || env.Sims["sima"].Requests.Load() != before || len(recsAfter) != len(recsBefore) {
		t.Errorf("explain had side effects: sim %d→%d records %d→%d", before, env.Sims["sima"].Requests.Load(), len(recsBefore), len(recsAfter))
	}
}

// Covers: RTE-07
func TestRTE07_ObservedLatencyOverridesStaticProfile(t *testing.T) {
	env := m2Env(t)
	// Static profile says qwen-27b has 400ms TTFT; observe it at 5s.
	for i := 0; i < 5; i++ {
		env.Engine.Latency.Observe("sima/qwen-27b", 5000, 100, 1000)
	}
	_, dec := explain(t, env, key1, userMsg("quality", "hello"))
	for _, c := range dec.Candidates {
		if c.Deployment == "sima/qwen-27b" && c.EstLatencyMs < 5000 {
			t.Errorf("estimate ignores observed latency: %+v", c)
		}
	}
}

// Covers: RTE-16
func TestRTE16_RuleRoutingOverheadP95(t *testing.T) {
	if raceEnabled {
		t.Skip("timing assertion is meaningless under the race detector")
	}
	testenv.ExclusiveCPU(t)
	env := m2Env(t)
	items, err := workload.Load("../../testdata/corpus/corpus.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var reqs []*api.ChatRequest
	for i := range items {
		b, _ := json.Marshal(items[i].Body("auto"))
		var r api.ChatRequest
		_ = json.Unmarshal(b, &r)
		reqs = append(reqs, &r)
	}
	snapKey := env.Engine.Snapshot().Keys.ByHash(governance.HashKey(key1))
	var lat []time.Duration
	for round := 0; round < 3; round++ {
		for _, r := range reqs {
			rc := &gateway.RequestContext{ID: "bench", Start: time.Now(), Endpoint: "chat",
				Principal: &governance.Principal{Tenant: snapKey.Tenant, Project: snapKey.Project, Scopes: snapKey.Scopes}}
			t0 := time.Now()
			if _, err := env.Engine.Explain(rc, r); err != nil {
				t.Fatal(err)
			}
			lat = append(lat, time.Since(t0))
		}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p95 := lat[len(lat)*95/100]
	t.Logf("routing overhead over %d decisions: p50 %v p95 %v p99 %v", len(lat), lat[len(lat)/2], p95, lat[len(lat)*99/100])
	if p95 > 5*time.Millisecond {
		t.Fatalf("rule-based routing p95 %v > 5ms", p95)
	}
}

// Covers: CST-03
func TestCST03_BaselineCostAndSavings(t *testing.T) {
	env := m2Env(t)
	r := env.Chat(key1, "auto", "hello", nil)
	env.Flush()
	recs, _ := env.Store.ListCostRecords(context.Background(), store.CostFilter{}, 1)
	rec := recs[0]
	if rec.RequestID != r.Header.Get("X-Request-Id") || rec.BaselineUSD <= rec.CostUSD || rec.RoutingSavings() <= 0 {
		t.Fatalf("baseline/savings: %+v", rec)
	}
	// Baseline uses the same tokens on the baseline deployment ($3/$15 per M).
	want := (float64(rec.InputTokens)*3 + float64(rec.OutputTokens)*15) / 1e6
	if d := rec.BaselineUSD - want; d > 1e-12 || d < -1e-12 {
		t.Fatalf("baseline %v want %v", rec.BaselineUSD, want)
	}
	rows, _ := env.Store.QueryCost(context.Background(), store.CostQuery{GroupBy: []string{"tenant"}})
	if len(rows) != 1 || rows[0].RoutingSavings <= 0 || rows[0].CacheSavings != 0 {
		t.Fatalf("savings aggregates: %+v", rows)
	}
}

// Covers: ENV-05, ENV-06
func TestENV05_CorpusShape(t *testing.T) {
	items, err := workload.Load("../../testdata/corpus/corpus.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) < 500 {
		t.Fatalf("corpus has %d prompts, need ≥ 500", len(items))
	}
	cats := map[string]int{}
	paraphrases := 0
	for _, it := range items {
		cats[it.Category]++
		if it.Paraphrase {
			paraphrases++
		}
		if it.Grader == "" {
			t.Fatalf("item %s has no grader", it.ID)
		}
	}
	for _, c := range []string{"faq", "greeting", "extraction", "classification", "summarisation", "code", "reasoning", "tool_use", "json_schema", "image", "long_context"} {
		if cats[c] == 0 {
			t.Errorf("category %s missing", c)
		}
	}
	// The replay plan adds exact repeats on top of the paraphrases.
	plan := workload.Plan(items, 0.2, 1)
	if len(plan) < len(items)+len(items)/6 || paraphrases < 50 {
		t.Errorf("plan %d paraphrases %d", len(plan), paraphrases)
	}
	// Regenerating is deterministic.
	again := workload.Generate(42)
	if len(again) != len(items) || again[100].Prompt != items[100].Prompt {
		t.Error("corpus generation is not deterministic")
	}
}

// Scenario 3: replay the corpus under `auto` and under an always-tier-3 baseline and
// grade both: cost must drop ≥ 40% with quality down ≤ 2 points.
// Covers: CST-03, RTE-03, RTE-05, ENV-05, ENV-06
func TestScenario03_SavingsVsBaseline(t *testing.T) {
	env := m2Env(t)
	items, err := workload.Load("../../testdata/corpus/corpus.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	oracle := map[string]sim.OracleItem{}
	for _, it := range items {
		if it.Reference != "" {
			oracle[it.Prompt] = sim.OracleItem{Reference: it.Reference, Difficulty: it.Difficulty}
		}
	}
	env.Sims["sima"].LoadOracle(oracle)
	run := func(model string) workload.Summary {
		_, sum, err := workload.Replay(context.Background(), items, workload.ReplayOptions{
			BaseURL: env.HTTP.URL, Model: model, Tenants: []workload.Tenant{{Key: key1}}, Concurrency: 16, RepeatRate: 0, Seed: 1})
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	auto := run("auto")
	base := run("baseline")
	reduction := 100 * (1 - auto.CostUSD/base.CostUSD)
	delta := auto.Quality - base.Quality
	t.Logf("auto: $%.4f quality %.2f errors %d | baseline: $%.4f quality %.2f errors %d | cost -%.1f%% quality %+.2f",
		auto.CostUSD, auto.Quality, auto.Errors, base.CostUSD, base.Quality, base.Errors, reduction, delta)
	t.Logf("auto served: %v", auto.ByModel)
	if auto.Errors > 0 || base.Errors > 0 {
		t.Fatalf("replay errors: auto %d baseline %d", auto.Errors, base.Errors)
	}
	if reduction < 40 {
		t.Errorf("cost reduction %.1f%% < 40%%", reduction)
	}
	if delta < -2 {
		t.Errorf("quality dropped %.2f points (> 2)", -delta)
	}
}
