package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/SureshKhemka/llm-gateway/internal/cache"
	"github.com/SureshKhemka/llm-gateway/internal/gateway"
	"github.com/SureshKhemka/llm-gateway/internal/sim"
	"github.com/SureshKhemka/llm-gateway/internal/store"
	"github.com/SureshKhemka/llm-gateway/internal/testenv"
	"github.com/SureshKhemka/llm-gateway/internal/workload"
)

const m5YAML = `
providers:
  - id: sima
    type: openai
    base_url: ${SIM_SIMA_URL}
cache:
  enabled: true
  ttl: 1h
  semantic: true
  semantic_threshold: 0.80
  embedder: sima/nomic-embed
policies:
  - name: cascade-json
    objective: min_cost
    cascade: {enabled: true, max_escalations: 2}
  - name: cascade-judge
    objective: min_cost
    cascade: {enabled: true, validators: [non_empty, judge], judge: sima/qwen-27b}
  - name: cascade-stream
    objective: min_cost
    cascade: {enabled: true, buffer_streams: true}
  - name: classify
    objective: min_cost
    difficulty: {estimator: classifier, classifier: sima/gemma-e2b}
  - name: similar
    objective: min_cost
    difficulty: {estimator: similarity, examples: ${EXAMPLES_FILE}, embedder: sima/nomic-embed}
  - name: tools-cached
    objective: min_cost
    cache: {allow_tools: true}
virtual_models:
  - {name: cascade, policy: cascade-json}
  - {name: judged, policy: cascade-judge}
  - {name: cascade-stream, policy: cascade-stream}
  - {name: classify, policy: classify}
  - {name: similar, policy: similar}
  - {name: tools-cached, policy: tools-cached}
tenants:
  - id: t1
    cache: {shared: true}
    projects: [{id: p1, keys: [{id: k1, secret: env:TEST_KEY_1}]}]
  - id: t2
    cache: {shared: true}
    projects: [{id: p2, keys: [{id: k2, secret: env:TEST_KEY_2}]}]
  - id: t3
    projects: [{id: p3, keys: [{id: k3, secret: env:TEST_KEY_3}]}]
admins:
  - {id: org, role: org_admin, secret: env:ADMIN_ORG}
defaults:
  baseline: sima/qwen-27b
` + testenv.StandardDeployments

func m5Env(t *testing.T, opts ...func(*testenv.Options)) *testenv.Env {
	examples := filepath.Join(t.TempDir(), "examples.jsonl")
	items, _ := workload.Load("../../testdata/corpus/corpus.jsonl")
	f, _ := os.Create(examples)
	for i, it := range items {
		if i%3 == 0 && !it.Paraphrase { // a labelled subset
			b, _ := json.Marshal(map[string]any{"prompt": it.Prompt, "difficulty": it.Difficulty})
			f.Write(append(b, '\n'))
		}
	}
	f.Close()
	o := testenv.Options{YAML: m5YAML, Env: map[string]string{"TEST_KEY_1": key1, "TEST_KEY_2": key2, "TEST_KEY_3": key3,
		"ADMIN_ORG": adminOrg, "EXAMPLES_FILE": examples}}
	for _, fn := range opts {
		fn(&o)
	}
	return testenv.New(t, o)
}

func costOf(r *testenv.Response) float64 {
	v, _ := strconv.ParseFloat(r.Header.Get("X-Gw-Cost-Usd"), 64)
	return v
}

var personSchema = map[string]any{"type": "object", "properties": map[string]any{
	"name": map[string]any{"type": "string"}, "age": map[string]any{"type": "integer"}}, "required": []string{"name", "age"}}

// Scenario 4: a JSON-schema request where tier 1 returns invalid JSON escalates to tier
// 2; the decision record shows both attempts and the total cost.
// Covers: RTE-06, RTE-14, CST-01
func TestScenario04_CascadeEscalation(t *testing.T) {
	env := m5Env(t)
	env.Sims["sima"].SetFaults("qwen-4b", sim.Faults{MalformedJSON: true})
	r := env.Chat(key1, "cascade", "extract: Asha is 34", map[string]any{"temperature": 0.9,
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "p", "schema": personSchema}}})
	if r.Status != 200 || r.Header.Get("X-Gw-Served-Model") != "sima/qwen-9b" || r.Header.Get("X-Gw-Route-Reason") != "cascade_escalated" {
		t.Fatalf("cascade: %d %s %s %s", r.Status, r.Header.Get("X-Gw-Served-Model"), r.Header.Get("X-Gw-Route-Reason"), r.Body)
	}
	var resp struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	_ = r.JSON(&resp)
	var obj map[string]any
	if json.Unmarshal([]byte(resp.Choices[0].Message.Content), &obj) != nil {
		t.Fatalf("final answer not JSON: %s", resp.Choices[0].Message.Content)
	}
	env.Flush()
	var dec gateway.Decision
	_ = env.Do("GET", "/v1/route/decisions/"+r.Header.Get("X-Request-Id"), key1, nil, nil).JSON(&dec)
	if len(dec.Attempts) != 2 || dec.Attempts[0].Deployment != "sima/qwen-4b" || dec.Attempts[0].Status != "invalid" ||
		dec.Attempts[0].Error != "validation:json_schema" || dec.Attempts[1].Deployment != "sima/qwen-9b" || !dec.Cascade {
		t.Fatalf("decision attempts: %+v", dec.Attempts)
	}
	total := dec.Attempts[0].CostUSD + dec.Attempts[1].CostUSD
	if math.Abs(costOf(r)-total) > 1e-12 || dec.Attempts[0].CostUSD == 0 {
		t.Fatalf("total cost %v should be the sum of attempts %v", costOf(r), total)
	}
	recs, _ := env.Store.ListCostRecords(context.Background(), store.CostFilter{Tenant: "t1"}, 1)
	if math.Abs(recs[0].OverheadUSD-dec.Attempts[0].CostUSD) > 1e-12 || recs[0].Attempts != 2 {
		t.Fatalf("cost record overhead: %+v", recs[0])
	}
	// Without the fault, the cheapest model's answer validates and no escalation happens.
	env.Sims["sima"].ClearFaults()
	r = env.Chat(key1, "cascade", "extract: Ravi is 40", map[string]any{"temperature": 0.9,
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "p", "schema": personSchema}}})
	if r.Header.Get("X-Gw-Served-Model") != "sima/qwen-4b" || r.Header.Get("X-Gw-Route-Reason") != "cascade_start" {
		t.Fatalf("valid cheap answer: %s %s", r.Header.Get("X-Gw-Served-Model"), r.Header.Get("X-Gw-Route-Reason"))
	}
}

// Covers: RTE-06
func TestRTE06_CascadeValidators(t *testing.T) {
	env := m5Env(t)
	hot := map[string]any{"temperature": 0.9}
	cases := []struct {
		fault  sim.Faults
		reason string
	}{
		{sim.Faults{EmptyAnswer: true}, "validation:non_empty"},
		{sim.Faults{Refuse: true}, "validation:no_refusal"},
	}
	for _, c := range cases {
		env.Sims["sima"].ClearFaults()
		env.Sims["sima"].SetFaults("gemma-e2b", c.fault)
		r := env.Chat(key1, "cascade", "hello there friend", hot)
		env.Flush()
		var dec gateway.Decision
		_ = env.Do("GET", "/v1/route/decisions/"+r.Header.Get("X-Request-Id"), key1, nil, nil).JSON(&dec)
		if r.Status != 200 || len(dec.Attempts) < 2 || dec.Attempts[0].Error != c.reason {
			t.Errorf("%s: %d %+v", c.reason, r.Status, dec.Attempts)
		}
	}
	// Truncation: max_tokens below the answer length fails not_truncated on every tier;
	// after max_escalations the best effort is returned.
	env.Sims["sima"].ClearFaults()
	r := env.Chat(key1, "cascade", "one two three four five six seven eight", map[string]any{"temperature": 0.9, "max_tokens": 3})
	if r.Status != 200 || r.Header.Get("X-Gw-Route-Reason") != "cascade_exhausted" {
		t.Errorf("truncation: %d %s", r.Status, r.Header.Get("X-Gw-Route-Reason"))
	}
	// Judge validator (opt-in): a low judge score escalates.
	prompt := "What is the capital of France?"
	env.Sims["sima"].LoadOracle(map[string]sim.OracleItem{prompt: {Reference: "Paris", Difficulty: 2}})
	r = env.Chat(key1, "judged", prompt, hot)
	if r.Status != 200 || !strings.Contains(string(r.Body), "Paris") {
		t.Fatalf("judge cascade did not reach a correct answer: %s", r.Body)
	}
	// Buffered cascade for streaming requests.
	env.Sims["sima"].SetFaults("gemma-e2b", sim.Faults{EmptyAnswer: true})
	r = env.Chat(key1, "cascade-stream", "hello stream", map[string]any{"temperature": 0.9, "stream": true})
	events, done := testenv.SSE(r.Body)
	if !done || r.Header.Get("X-Gw-Route-Reason") != "cascade_escalated" || testenv.StreamText(events) == "" {
		t.Fatalf("buffered stream cascade: %s %s", r.Header.Get("X-Gw-Route-Reason"), r.Body)
	}
}

// Scenario 5: replay with exact repeats and paraphrases. Exact and semantic hits are
// served at zero cost with cost avoided recorded; a bypass skips the cache.
// Covers: CCH-01, CCH-02, CCH-06, CCH-07
func TestScenario05_Caching(t *testing.T) {
	env := m5Env(t)
	items, _ := workload.Load("../../testdata/corpus/corpus.jsonl")
	oracle := map[string]sim.OracleItem{}
	for _, it := range items {
		if it.Reference != "" {
			oracle[it.Prompt] = sim.OracleItem{Reference: it.Reference, Difficulty: it.Difficulty}
		}
	}
	env.Sims["sima"].LoadOracle(oracle)
	results, sum, err := workload.Replay(context.Background(), items, workload.ReplayOptions{
		BaseURL: env.HTTP.URL, Model: "auto", Tenants: []workload.Tenant{{Key: key1}}, Concurrency: 1, RepeatRate: 0.2, Seed: 3})
	if err != nil {
		t.Fatal(err)
	}
	var exact, semantic, semanticWrong int
	for _, r := range results {
		switch r.Cache {
		case "exact_hit":
			exact++
		case "semantic_hit":
			semantic++
			if r.Score < 1 {
				semanticWrong++
			}
		default:
			continue
		}
		if r.CostUSD != 0 {
			t.Errorf("cache hit billed $%v", r.CostUSD)
		}
	}
	t.Logf("requests %d: exact %d, semantic %d (wrong %d), hit rate %.1f%%, quality %.2f, errors %d",
		sum.Requests, exact, semantic, semanticWrong, 100*sum.CacheRate, sum.Quality, sum.Errors)
	if sum.CacheRate < 0.25 {
		t.Errorf("cache hit rate %.1f%% < 25%%", 100*sum.CacheRate)
	}
	if exact == 0 || semantic == 0 || sum.Errors > 0 {
		t.Fatalf("expected exact and semantic hits without errors")
	}
	if float64(semanticWrong) > 0.05*float64(semantic) {
		t.Errorf("%d of %d semantic hits served a wrong answer", semanticWrong, semantic)
	}
	env.Flush()
	rows, _ := env.Store.QueryCost(context.Background(), store.CostQuery{GroupBy: []string{"cache_status"}})
	for _, row := range rows {
		if (row.Group["cache_status"] == "exact_hit" || row.Group["cache_status"] == "semantic_hit") && (row.CacheSavings <= 0 || row.CostUSD != 0) {
			t.Errorf("hits must record cost avoided at zero cost: %+v", row)
		}
	}
	// Bypass skips the cache and calls the model; refresh overwrites the entry.
	it := items[0]
	before := env.Sims["sima"].Requests.Load()
	r := env.Post("/v1/chat/completions", key1, it.Body("auto"), map[string]string{"X-Gw-Cache": "bypass"})
	if r.Header.Get("X-Gw-Cache") != "bypass" || env.Sims["sima"].Requests.Load() == before {
		t.Fatalf("bypass: %s", r.Header.Get("X-Gw-Cache"))
	}
	r = env.Post("/v1/chat/completions", key1, it.Body("auto"), map[string]string{"X-Gw-Cache": "refresh"})
	if r.Header.Get("X-Gw-Cache") != "miss" {
		t.Fatalf("refresh: %s", r.Header.Get("X-Gw-Cache"))
	}
	if r := env.Post("/v1/chat/completions", key1, it.Body("auto")); r.Header.Get("X-Gw-Cache") != "exact_hit" {
		t.Fatalf("after refresh: %s", r.Header.Get("X-Gw-Cache"))
	}
}

// Covers: CCH-03
func TestCCH03_TenantScopingAndSharedOptIn(t *testing.T) {
	env := m5Env(t)
	body := map[string]any{"temperature": 0}
	env.Chat(key3, "auto", "what is the tallest mountain", body)
	if r := env.Chat(key3, "auto", "what is the tallest mountain", body); r.Header.Get("X-Gw-Cache") != "exact_hit" {
		t.Fatalf("own tenant: %s", r.Header.Get("X-Gw-Cache"))
	}
	if r := env.Chat(key1, "auto", "what is the tallest mountain", body); r.Header.Get("X-Gw-Cache") != "miss" {
		t.Fatalf("t1 read t3's private entry: %s", r.Header.Get("X-Gw-Cache"))
	}
	// t1 and t2 both opted in to sharing.
	if r := env.Chat(key2, "auto", "what is the tallest mountain", body); r.Header.Get("X-Gw-Cache") != "exact_hit" {
		t.Fatalf("shared namespace: %s", r.Header.Get("X-Gw-Cache"))
	}
	if r := env.Chat(key3, "auto", "explain the monsoon season in Kerala", body); r.Status != 200 {
		t.Fatal(r.Status)
	}
	if r := env.Chat(key1, "auto", "explain the monsoon season in Kerala", body); r.Header.Get("X-Gw-Cache") != "miss" {
		t.Fatalf("shared tenant read a non-sharing tenant's entry: %s", r.Header.Get("X-Gw-Cache"))
	}
}

// Covers: CCH-04
func TestCCH04_OnlyDeterministicRequestsCached(t *testing.T) {
	env := m5Env(t)
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "f", "parameters": map[string]any{"type": "object"}}}}
	cases := []struct {
		name  string
		model string
		extra map[string]any
		want  string
	}{
		{"no temperature (provider default ~1)", "auto", nil, "bypass"},
		{"hot", "auto", map[string]any{"temperature": 0.9}, "bypass"},
		{"cool", "auto", map[string]any{"temperature": 0.2}, "miss"},
		{"tools", "auto", map[string]any{"temperature": 0, "tools": tools}, "bypass"},
		{"tools allowed by policy", "tools-cached", map[string]any{"temperature": 0, "tools": tools}, "miss"},
		{"n>1", "auto", map[string]any{"temperature": 0, "n": 2}, "bypass"},
	}
	for i, c := range cases {
		r := env.Chat(key1, c.model, fmt.Sprintf("question %d", i), c.extra)
		if r.Header.Get("X-Gw-Cache") != c.want {
			t.Errorf("%s: %s want %s", c.name, r.Header.Get("X-Gw-Cache"), c.want)
		}
	}
	if r := env.Chat(key1, "auto", "question 2", map[string]any{"temperature": 0.2}, map[string]string{"X-Gw-Cache": "no-store"}); r.Header.Get("X-Gw-Cache") != "bypass" {
		t.Errorf("no-store hint: %s", r.Header.Get("X-Gw-Cache"))
	}
}

// Covers: CCH-05
func TestCCH05_HitsReturnedInRequestedFormatAndStreamed(t *testing.T) {
	env := m5Env(t)
	cold := map[string]any{"temperature": 0}
	env.Chat(key1, "auto", "stream me later", cold)
	r := env.Chat(key1, "auto", "stream me later", map[string]any{"temperature": 0, "stream": true, "stream_options": map[string]any{"include_usage": true}})
	events, done := testenv.SSE(r.Body)
	if r.Header.Get("X-Gw-Cache") != "exact_hit" || !done || !strings.Contains(testenv.StreamText(events), "stream me later") {
		t.Fatalf("streamed hit: %s %s", r.Header.Get("X-Gw-Cache"), r.Body)
	}
	// A streamed miss populates the cache for non-streaming callers too.
	env.Chat(key1, "auto", "first as a stream", map[string]any{"temperature": 0, "stream": true})
	if r := env.Chat(key1, "auto", "first as a stream", cold); r.Header.Get("X-Gw-Cache") != "exact_hit" || !strings.Contains(string(r.Body), "first as a stream") {
		t.Fatalf("stream-populated entry: %s %s", r.Header.Get("X-Gw-Cache"), r.Body)
	}
}

// Covers: CCH-08
func TestCCH08_InvalidationAndBoundedSize(t *testing.T) {
	env := m5Env(t)
	cold := map[string]any{"temperature": 0}
	env.Chat(key3, "auto", "alpha question", cold)
	env.Chat(key3, "sima/qwen-9b", "beta question", cold)
	r := env.Post("/admin/v1/cache/invalidate", adminOrg, map[string]any{"tenant": "t3", "model": "sima/qwen-9b"})
	if r.Status != 200 || !strings.Contains(string(r.Body), `"invalidated": 1`) {
		t.Fatalf("invalidate by model: %d %s", r.Status, r.Body)
	}
	if r := env.Chat(key3, "sima/qwen-9b", "beta question", cold); r.Header.Get("X-Gw-Cache") != "miss" {
		t.Fatal("invalidated entry still served")
	}
	if r := env.Chat(key3, "auto", "alpha question", cold); r.Header.Get("X-Gw-Cache") != "exact_hit" {
		t.Fatal("unrelated entry invalidated")
	}
	n, _ := env.Engine.InvalidateCache(context.Background(), "t3", "", "", 0)
	if n < 2 {
		t.Fatalf("invalidate by tenant: %d", n)
	}
	// Bounded size: the LRU keeps at most max_entries.
	m := cache.NewMemory()
	for i := 0; i < 20; i++ {
		_ = m.Put(context.Background(), &cache.Entry{Key: fmt.Sprint(i), Namespace: "n"}, 3600e9, 5)
	}
	if e, _ := m.Get(context.Background(), "n", "0"); e != nil {
		t.Fatal("oldest entry not evicted")
	}
	if e, _ := m.Get(context.Background(), "n", "19"); e == nil {
		t.Fatal("newest entry evicted")
	}
}

// The cache is shared across instances through Redis.
// Covers: CCH-01, BUD-05
func TestCCH01_RedisCacheSharedAcrossInstances(t *testing.T) {
	mr, counters := newMiniredis(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rc.Close() })
	shared := cache.NewRedis(rc, "gw:")
	env := m5Env(t, func(o *testenv.Options) { o.Counters = counters })
	env.Engine.Cache = shared
	other := env.SecondInstance()
	other.Engine.Cache = shared
	env.Chat(key1, "auto", "shared across instances", map[string]any{"temperature": 0})
	r := env.DoAt(other.HTTP.URL, "POST", "/v1/chat/completions", key1, map[string]any{"model": "auto", "temperature": 0,
		"messages": []map[string]any{{"role": "user", "content": "shared across instances"}}}, nil)
	if r.Header.Get("X-Gw-Cache") != "exact_hit" {
		t.Fatalf("second instance: %s", r.Header.Get("X-Gw-Cache"))
	}
	// Semantic entries are shared too.
	r = env.DoAt(other.HTTP.URL, "POST", "/v1/chat/completions", key1, map[string]any{"model": "auto", "temperature": 0,
		"messages": []map[string]any{{"role": "user", "content": "shared across instances please"}}}, nil)
	if r.Header.Get("X-Gw-Cache") != "semantic_hit" {
		t.Fatalf("semantic on second instance: %s", r.Header.Get("X-Gw-Cache"))
	}
	if n, err := shared.Invalidate(context.Background(), "", cache.Filter{}); err != nil || n == 0 {
		t.Fatalf("redis invalidate: %d %v", n, err)
	}
}

// Covers: RTE-03, RTE-16
func TestRTE03_ClassifierEstimatorChargesItsCost(t *testing.T) {
	env := m5Env(t)
	items, _ := workload.Load("../../testdata/corpus/corpus.jsonl")
	oracle := map[string]sim.OracleItem{}
	for _, it := range items {
		oracle[it.Prompt] = sim.OracleItem{Reference: it.Reference, Difficulty: it.Difficulty}
	}
	env.Sims["sima"].LoadOracle(oracle)
	var hard workload.Item
	for _, it := range items {
		if it.Category == "reasoning" && it.Difficulty == 3 {
			hard = it
			break
		}
	}
	r := env.Post("/v1/chat/completions", key1, hard.Body("classify"))
	env.Flush()
	var dec gateway.Decision
	_ = env.Do("GET", "/v1/route/decisions/"+r.Header.Get("X-Request-Id"), key1, nil, nil).JSON(&dec)
	if dec.Estimator != "classifier" || dec.DifficultySignals["classifier"] != 3 || dec.ClassifierCostUSD <= 0 || dec.ClassifierMs <= 0 {
		t.Fatalf("classifier decision: %+v", dec)
	}
	if env.Engine.Snapshot().Catalog.Deployments[r.Header.Get("X-Gw-Served-Model")].Tier != 3 {
		t.Fatalf("classifier tier 3 served by %s", r.Header.Get("X-Gw-Served-Model"))
	}
	recs, _ := env.Store.ListCostRecords(context.Background(), store.CostFilter{Tenant: "t1"}, 1)
	split := recs[0].InputUSD + recs[0].CachedInputUSD + recs[0].OutputUSD + recs[0].ReasoningUSD
	// Overhead = classifier call + semantic-cache lookup embedding.
	if recs[0].OverheadUSD < dec.ClassifierCostUSD || math.Abs(recs[0].CostUSD-split-recs[0].OverheadUSD) > 1e-12 || math.Abs(costOf(r)-recs[0].CostUSD) > 1e-12 {
		t.Fatalf("classifier cost not charged to the request: %+v", recs[0])
	}
	// Explain shows the classifier verdict without calling the routed model.
	if st, d := explain(t, env, key1, map[string]any{"model": "classify", "messages": hard.Messages()}); st != 200 || d.DifficultySignals["classifier"] != 3 {
		t.Fatalf("explain with classifier: %d %+v", st, d)
	}
}

// Covers: RTE-04
func TestRTE04_SimilarityEstimator(t *testing.T) {
	env := m5Env(t)
	items, _ := workload.Load("../../testdata/corpus/corpus.jsonl")
	right, total := 0, 0
	for i, it := range items {
		if i%3 == 0 || it.Paraphrase || len(it.Images) > 0 || it.Category == "long_context" {
			continue // examples are every third item; score held-out prompts
		}
		total++
		_, dec := explain(t, env, key1, map[string]any{"model": "similar", "messages": it.Messages()})
		if dec.Estimator == "similarity" && int(dec.DifficultySignals["knn_tier"]) == it.Difficulty {
			right++
		}
		if total >= 120 {
			break
		}
	}
	acc := float64(right) / float64(total)
	t.Logf("k-NN difficulty accuracy on %d held-out prompts: %.1f%%", total, 100*acc)
	if acc < 0.8 {
		t.Fatalf("similarity estimator accuracy %.1f%% < 80%%", 100*acc)
	}
}
