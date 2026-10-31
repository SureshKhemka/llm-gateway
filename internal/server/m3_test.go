package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/sim"
	"github.com/SureshKhemka/llm-gateway/internal/testenv"
)

const m3YAML = `
server:
  max_in_flight: 256
providers:
  - id: sima
    type: openai
    base_url: ${SIM_SIMA_URL}
  - id: simb
    type: openai
    base_url: ${SIM_SIMB_URL}
resilience:
  max_attempts: 3
  backoff_base: 20ms
  backoff_max: 200ms
  latency_alpha: 0.5
  breaker:
    window: 30s
    min_requests: 3
    error_rate: 0.5
    consecutive_failures: 3
    cool_down: 300ms
policies:
  - name: fast-enough
    objective: min_cost
    latency_target_ms: 1500
virtual_models:
  - name: latency
    policy: fast-enough
    deployments: [sima/qwen-4b, simb/qwen-4b]
tenants:
  - id: acme
    projects:
      - id: web
        keys:
          - id: k1
            secret: env:TEST_KEY_1
            scopes: {priority: 2}
          - id: k2
            secret: env:TEST_KEY_2
            scopes: {priority: 0}
defaults:
  baseline: sima/qwen-27b
` + testenv.StandardDeployments

func m3Env(t *testing.T, extra string) *testenv.Env {
	yaml := m3YAML + testenv.DeploymentsFor("simb") + extra
	return testenv.New(t, testenv.Options{
		Sims: map[string][]sim.Model{"sima": testenv.StandardModels, "simb": testenv.StandardModels},
		YAML: yaml, Env: map[string]string{"TEST_KEY_1": key1, "TEST_KEY_2": key2}})
}

const tier1Prompt = "Extract the invoice number and total from: Invoice INV-%d for Kaveri Foods, total due ₹12,400."

// Scenario 8: a model is unloaded and a whole provider killed mid-replay. Circuits open,
// traffic moves, no client sees an error while an alternative exists, and traffic
// returns after half-open recovery.
// Covers: RES-01, RES-02, RES-03, RES-08
func TestScenario08_ProviderFailover(t *testing.T) {
	env := m3Env(t, "")
	var errors atomic.Int64
	served := sync.Map{}
	send := func(n int) {
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r := env.Chat(key1, "auto", strings.Replace(tier1Prompt, "%d", string(rune('A'+i%26)), 1), nil)
				if r.Status != 200 {
					errors.Add(1)
					t.Logf("client error %d: %s", r.Status, r.Body)
					return
				}
				v, _ := served.LoadOrStore(r.Header.Get("X-Gw-Served-Model"), &atomic.Int64{})
				v.(*atomic.Int64).Add(1)
			}(i)
		}
		wg.Wait()
	}
	count := func(dep string) int64 {
		if v, ok := served.Load(dep); ok {
			return v.(*atomic.Int64).Load()
		}
		return 0
	}
	send(20)
	if count("sima/qwen-4b") == 0 {
		t.Fatal("cheapest tier-1 deployment unused before the fault")
	}
	// Unload the model on sima (LM Studio returns 404 for it) mid-replay.
	env.Sims["sima"].RemoveModel("qwen-4b")
	send(30)
	if st := env.Engine.Health.Status("sima/qwen-4b"); st.Breaker != "open" {
		t.Fatalf("breaker for unloaded model: %+v", st)
	}
	before := count("simb/qwen-4b")
	if before == 0 {
		t.Fatal("traffic did not move to the twin deployment")
	}
	// Kill the second provider entirely.
	env.KillSim("simb")
	send(30)
	if errors.Load() != 0 {
		t.Fatalf("%d client errors although alternatives existed", errors.Load())
	}
	if st := env.Engine.Health.Status("simb/qwen-4b"); st.Breaker != "open" {
		t.Fatalf("breaker for killed provider: %+v", st)
	}
	// Recovery: model reloaded, provider back; after cool-down a half-open probe closes the circuit.
	env.Sims["sima"].AddModel(sim.Model{ID: "qwen-4b", Tier: 1})
	env.RestartSim("simb")
	time.Sleep(350 * time.Millisecond)
	beforeA := count("sima/qwen-4b")
	send(10)
	if st := env.Engine.Health.Status("sima/qwen-4b"); st.Breaker != "closed" {
		t.Fatalf("breaker did not close after recovery: %+v", st)
	}
	if count("sima/qwen-4b") <= beforeA {
		t.Fatal("traffic did not return to the recovered deployment")
	}
	if errors.Load() != 0 {
		t.Fatalf("%d client errors", errors.Load())
	}
}

// Scenario 9: 3 s of added latency on one deployment under a latency-target policy moves
// traffic off it (well within one minute).
// Covers: RTE-07, RES-01
func TestScenario09_LatencyAwareRouting(t *testing.T) {
	env := m3Env(t, "")
	for i := 0; i < 4; i++ {
		if r := env.Chat(key1, "latency", "hello", nil); r.Header.Get("X-Gw-Served-Model") != "sima/qwen-4b" {
			t.Fatalf("baseline routing went to %s", r.Header.Get("X-Gw-Served-Model"))
		}
	}
	env.Sims["sima"].SetFaults("qwen-4b", sim.Faults{LatencyMs: 3000})
	injected := time.Now()
	var moved time.Duration
	for i := 0; i < 20; i++ {
		r := env.Chat(key1, "latency", "hello", nil)
		if r.Status != 200 {
			t.Fatalf("status %d", r.Status)
		}
		if r.Header.Get("X-Gw-Served-Model") == "simb/qwen-4b" {
			moved = time.Since(injected)
			break
		}
	}
	if moved == 0 || moved > time.Minute {
		t.Fatalf("traffic did not move within a minute (moved after %v)", moved)
	}
	t.Logf("traffic moved off the slow deployment after %v", moved.Round(time.Millisecond))
	for i := 0; i < 5; i++ {
		if r := env.Chat(key1, "latency", "hello", nil); r.Header.Get("X-Gw-Served-Model") != "simb/qwen-4b" {
			t.Fatalf("traffic drifted back to the slow deployment")
		}
	}
}

// Scenario 10: the provider drops the stream after 50 tokens. The client gets a clean
// error event and only delivered tokens are billed.
// Covers: RES-04, API-11, API-13
func TestScenario10_MidStreamFailure(t *testing.T) {
	env := m3Env(t, "")
	prompt := "Tell me a long story about the monsoon."
	long := strings.Repeat("rain falls on the hills and rivers run ", 15) // 120 words
	env.Sims["sima"].LoadOracle(map[string]sim.OracleItem{prompt: {Reference: long, Difficulty: 0}})
	env.Sims["simb"].LoadOracle(map[string]sim.OracleItem{prompt: {Reference: long, Difficulty: 0}})
	env.Sims["sima"].SetFaults("qwen-9b", sim.Faults{DropAfterTokens: 50})
	r := env.Chat(key1, "sima/qwen-9b", prompt, map[string]any{"stream": true, "stream_options": map[string]any{"include_usage": true}})
	if r.Status != 200 {
		t.Fatalf("status %d", r.Status)
	}
	events, done := testenv.SSE(r.Body)
	if !done {
		t.Fatal("stream not terminated with [DONE] after the error event")
	}
	var errEvent map[string]any
	models := map[string]bool{}
	for _, ev := range events {
		var m map[string]any
		_ = json.Unmarshal(ev, &m)
		if e, ok := m["error"].(map[string]any); ok {
			errEvent = e
		}
		if mo, ok := m["model"].(string); ok {
			models[mo] = true
		}
	}
	if errEvent == nil || errEvent["code"] != "upstream_unavailable" {
		t.Fatalf("no well-formed error event: %s", r.Body)
	}
	if len(models) != 1 {
		t.Fatalf("model switched mid-stream: %v", models)
	}
	text := testenv.StreamText(events)
	if n := len(strings.Fields(text)); n != 50 {
		t.Fatalf("delivered %d words, expected 50", n)
	}
	env.Flush()
	recs, _ := env.Store.ListCostRecords(context.Background(), storeFilterAll(), 1)
	if recs[0].OutputTokens < 45 || recs[0].OutputTokens > 75 || !recs[0].Estimated || recs[0].Status != 502 {
		t.Fatalf("billing of a dropped stream: %+v", recs[0])
	}
}

// Covers: RES-03, RES-05
func TestRES05_RetriesSameDeploymentHonouringRetryAfter(t *testing.T) {
	env := m3Env(t, "")
	env.Sims["sima"].SetFaults("qwen-27b", sim.Faults{FailNext: 1, ErrorStatus: 429, RetryAfterSec: 1})
	start := time.Now()
	// Concrete model: one candidate, so the gateway retries it after Retry-After.
	r := env.Chat(key1, "sima/qwen-27b", "hi", nil)
	if r.Status != 200 || r.Header.Get("X-Gw-Fallback-Attempts") != "1" {
		t.Fatalf("status %d attempts %s %s", r.Status, r.Header.Get("X-Gw-Fallback-Attempts"), r.Body)
	}
	if el := time.Since(start); el < time.Second {
		t.Fatalf("retried after %v, before Retry-After", el)
	}
}

// Covers: RES-06
func TestRES06_Timeouts(t *testing.T) {
	env := m3Env(t, "")
	// Per-request total timeout, bounded by the configured maximum.
	env.Sims["sima"].SetFaults("qwen-27b", sim.Faults{LatencyMs: 2000})
	start := time.Now()
	r := env.Chat(key1, "sima/qwen-27b", "hi", nil, map[string]string{"X-Gw-Timeout-Ms": "200"})
	if r.Status != 503 || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("timeout override: %d after %v", r.Status, time.Since(start))
	}
	// TTFT timeout on a stream falls back to the twin deployment before any token.
	env.Sims["sima"].ClearFaults()
	env.Sims["sima"].SetFaults("qwen-4b", sim.Faults{LatencyMs: 2000})
	start = time.Now()
	body := map[string]any{"model": "latency", "stream": true, "messages": []map[string]any{{"role": "user", "content": "hello"}}}
	r = env.Post("/v1/chat/completions", key1, body, map[string]string{"X-Gw-Ttft-Timeout-Ms": "200"})
	if r.Status != 200 || r.Header.Get("X-Gw-Served-Model") != "simb/qwen-4b" || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("TTFT fallback: %d %s after %v", r.Status, r.Header.Get("X-Gw-Served-Model"), time.Since(start))
	}
	// Inter-token idle timeout ends a started stream with an error event.
	idle := strings.Replace(m3YAML, "  latency_alpha: 0.5", "  latency_alpha: 0.5\n  timeouts: {idle: 150ms}", 1)
	env2 := testenv.New(t, testenv.Options{Sims: map[string][]sim.Model{"sima": testenv.StandardModels, "simb": testenv.StandardModels},
		YAML: idle + testenv.DeploymentsFor("simb"), Env: map[string]string{"TEST_KEY_1": key1, "TEST_KEY_2": key2}})
	env2.Sims["sima"].SetFaults("qwen-9b", sim.Faults{TokensPerSec: 2})
	r = env2.Chat(key1, "sima/qwen-9b", "one two three four", map[string]any{"stream": true})
	if !strings.Contains(string(r.Body), "inter-token idle timeout") {
		t.Fatalf("idle timeout not reported: %s", r.Body)
	}
}

// Covers: RES-07, PRV-09
func TestRES07_ConcurrencyOverflowSpillsToNextCandidate(t *testing.T) {
	yaml := strings.Replace(m3YAML+testenv.DeploymentsFor("simb"), "virtual_models:\n  - name: latency", "virtual_models:\n  - name: pooled\n    policy: default\n    deployments: [sima/pool-a, simb/pool-b]\n  - name: latency", 1) + `
  - id: sima/pool-a
    provider: sima
    model: qwen-4b
    tier: 1
    pool: p1
    concurrency: 1
    queue_wait: 20ms
    prices: [{effective_from: 2026-01-01T00:00:00Z, input: 0.1, output: 0.1}]
  - id: simb/pool-b
    provider: simb
    model: qwen-4b
    tier: 1
    pool: p1
    concurrency: 1
    queue_wait: 20ms
    prices: [{effective_from: 2026-01-01T00:00:00Z, input: 0.1, output: 0.1}]
`
	env := testenv.New(t, testenv.Options{Sims: map[string][]sim.Model{"sima": testenv.StandardModels, "simb": testenv.StandardModels},
		YAML: yaml, Env: map[string]string{"TEST_KEY_1": key1, "TEST_KEY_2": key2}})
	env.Sims["sima"].SetFaults("qwen-4b", sim.Faults{LatencyMs: 400})
	env.Sims["simb"].SetFaults("qwen-4b", sim.Faults{LatencyMs: 400})
	var wg sync.WaitGroup
	served := make([]string, 3)
	statuses := make([]int, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := env.Chat(key1, "pooled", "hello", nil)
			served[i], statuses[i] = r.Header.Get("X-Gw-Served-Model"), r.Status
		}(i)
		time.Sleep(30 * time.Millisecond)
	}
	wg.Wait()
	got := map[string]int{}
	for i := range served {
		got[served[i]+"/"+http.StatusText(statuses[i])]++
	}
	// Two slots in the pool: requests 1 and 2 use one each (least in-flight), request 3
	// overflows both queues and is rejected as overloaded.
	if got["sima/pool-a/OK"] != 1 || got["simb/pool-b/OK"] != 1 || got["/Service Unavailable"] != 1 {
		t.Fatalf("pool/overflow distribution: %v", got)
	}
}

// Covers: RES-08
func TestRES08_FallbackNeverRelaxesHardConstraints(t *testing.T) {
	yaml := m3YAML + testenv.DeploymentsFor("simb") + `
  - id: simb/cloud-27b
    provider: simb
    model: qwen-27b
    tier: 3
    residency: [external]
    prices: [{effective_from: 2026-01-01T00:00:00Z, input: 0.01, output: 0.01}]
`
	env := testenv.New(t, testenv.Options{Sims: map[string][]sim.Model{"sima": testenv.StandardModels, "simb": testenv.StandardModels},
		YAML: yaml, Env: map[string]string{"TEST_KEY_1": key1, "TEST_KEY_2": key2}})
	for _, m := range []string{"gemma-e2b", "qwen-4b", "qwen-9b", "qwen-27b", "qwen-vl-8b"} {
		env.Sims["sima"].SetFaults(m, sim.Faults{Down: true})
	}
	env.KillSim("simb") // takes down the local twins too; only the external one is "cheap"
	r := env.Chat(key1, "auto", "hello", nil, map[string]string{"X-Gw-Sensitivity": "confidential"})
	if r.Status == 200 && r.Header.Get("X-Gw-Served-Model") == "simb/cloud-27b" {
		t.Fatal("confidential request fell back to an external deployment")
	}
	env.Flush()
	_, dec := explain(t, env, key1, userMsg("auto", "hello"), map[string]string{"X-Gw-Sensitivity": "confidential"})
	for _, id := range dec.Ordered {
		if id == "simb/cloud-27b" {
			t.Fatal("external deployment is a fallback candidate for a confidential request")
		}
	}
}

// Covers: RES-09
func TestRES09_LoadSheddingRejectsLowPriorityFirst(t *testing.T) {
	env := testenv.New(t, testenv.Options{Sims: map[string][]sim.Model{"sima": testenv.StandardModels, "simb": testenv.StandardModels},
		YAML: strings.Replace(m3YAML, "max_in_flight: 256", "max_in_flight: 4", 1) + testenv.DeploymentsFor("simb"),
		Env:  map[string]string{"TEST_KEY_1": key1, "TEST_KEY_2": key2}})
	env.Sims["sima"].SetFaults("qwen-27b", sim.Faults{LatencyMs: 600})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); env.Chat(key1, "sima/qwen-27b", "slow", nil) }()
	}
	time.Sleep(150 * time.Millisecond)
	low := env.Chat(key2, "sima/gemma-e2b", "hi", nil)
	wg.Wait()
	if low.Status != 503 || low.Header.Get("Retry-After") == "" {
		t.Fatalf("low-priority request at saturation: %d", low.Status)
	}
	if high := env.Chat(key1, "sima/gemma-e2b", "hi", nil); high.Status != 200 {
		t.Fatalf("high-priority request after load drained: %d", high.Status)
	}
}

// Covers: RES-10
func TestRES10_DrainFinishesInFlightStreamsAndRejectsNew(t *testing.T) {
	env := m3Env(t, "")
	env.Sims["sima"].SetFaults("qwen-9b", sim.Faults{TokensPerSec: 20})
	done := make(chan *testenv.Response)
	go func() {
		done <- env.Chat(key1, "sima/qwen-9b", "one two three four five six seven eight", map[string]any{"stream": true})
	}()
	time.Sleep(100 * time.Millisecond)
	drained := make(chan error)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		drained <- env.Server.Drain(ctx)
	}()
	time.Sleep(50 * time.Millisecond)
	if r := env.Chat(key1, "sima/qwen-9b", "new", nil); r.Status != 503 {
		t.Fatalf("new request during drain: %d", r.Status)
	}
	r := <-done
	if _, ok := testenv.SSE(r.Body); !ok {
		t.Fatal("in-flight stream was cut during drain")
	}
	if err := <-drained; err != nil {
		t.Fatalf("drain: %v", err)
	}
	if r := env.Do("GET", "/readyz", "", nil, nil); r.Status != 503 {
		t.Fatalf("readyz while draining: %d", r.Status)
	}
}

// Covers: RES-01, PRV-10
func TestPRV10_ActiveProbesAndColdDeployments(t *testing.T) {
	env := m3Env(t, "")
	env.Sims["sima"].RemoveModel("qwen-4b")
	env.Engine.ProbeOnce(context.Background())
	if st := env.Engine.Health.Status("sima/qwen-4b"); !st.Cold || st.Health != "unhealthy" {
		t.Fatalf("unloaded model after probe: %+v", st)
	}
	// Latency-sensitive request avoids the cold deployment while a warm twin exists.
	_, dec := explain(t, env, key1, userMsg("latency", "hello"))
	if dec.Chosen != "simb/qwen-4b" {
		t.Fatalf("latency-sensitive request chose %s", dec.Chosen)
	}
	env.KillSim("simb")
	env.Engine.ProbeOnce(context.Background())
	if st := env.Engine.Health.Status("simb/qwen-27b"); st.Health != "unhealthy" {
		t.Fatalf("killed provider after probe: %+v", st)
	}
}

// Covers: RTE-11
func TestRTE11_SessionPinning(t *testing.T) {
	env := m3Env(t, "")
	h := map[string]string{"X-Gw-Session-Id": "sess-1"}
	first := env.Chat(key1, "auto", "Explain step by step why the sky is blue, then derive the scattering law.", nil, h)
	pinned := first.Header.Get("X-Gw-Served-Model")
	// A trivially easy follow-up in the same session stays on the same deployment.
	r := env.Chat(key1, "auto", "thanks!", nil, h)
	if r.Header.Get("X-Gw-Served-Model") != pinned || r.Header.Get("X-Gw-Route-Reason") != "session_pinned" {
		t.Fatalf("follow-up went to %s (%s), pinned %s", r.Header.Get("X-Gw-Served-Model"), r.Header.Get("X-Gw-Route-Reason"), pinned)
	}
	// Without the session it routes on its own merits.
	if r := env.Chat(key1, "auto", "thanks!", nil); r.Header.Get("X-Gw-Served-Model") == pinned {
		t.Fatalf("non-session request pinned too")
	}
	// When the pinned deployment is unhealthy, the session moves.
	dep := env.Engine.Snapshot().Catalog.Deployments[pinned]
	for _, sname := range []string{"sima", "simb"} {
		if dep.Provider == sname {
			env.Sims[sname].SetFaults(dep.Model, sim.Faults{Down: true})
		}
	}
	for i := 0; i < 4; i++ {
		env.Chat(key1, "auto", "thanks!", nil, h)
	}
	if r := env.Chat(key1, "auto", "thanks!", nil, h); r.Status != 200 || r.Header.Get("X-Gw-Served-Model") == pinned {
		t.Fatalf("session stuck on an unhealthy deployment: %d %s", r.Status, r.Header.Get("X-Gw-Served-Model"))
	}
}

// When no candidate can meet the latency target, the closest one goes first rather than
// the cheapest (the target can't be met, so cost no longer decides).
// Covers: RTE-05
func TestRTE05_ClosestToLatencyTargetWhenNoneMeetsIt(t *testing.T) {
	env := testenv.New(t, testenv.Options{YAML: `
providers:
  - {id: sima, type: openai, base_url: "${SIM_SIMA_URL}"}
deployments:
  - {id: slow-cheap, provider: sima, model: qwen-4b, tier: 1, latency: {ttft_ms: 3000, tokens_per_sec: 50},
     prices: [{effective_from: 2026-01-01T00:00:00Z, input: 0.1, output: 0.1}]}
  - {id: fast-dear, provider: sima, model: qwen-9b, tier: 1, latency: {ttft_ms: 900, tokens_per_sec: 200},
     prices: [{effective_from: 2026-01-01T00:00:00Z, input: 1.0, output: 1.0}]}
policies:
  - {name: default, objective: min_cost, latency_target_ms: 100}
`})
	r := env.Chat("", "auto", "hello", nil)
	if r.Status != 200 || r.Header.Get("X-Gw-Served-Model") != "fast-dear" || r.Header.Get("X-Gw-Route-Reason") != "closest_to_latency_target" {
		t.Fatalf("served %s (%s), want fast-dear (closest_to_latency_target)", r.Header.Get("X-Gw-Served-Model"), r.Header.Get("X-Gw-Route-Reason"))
	}
}
