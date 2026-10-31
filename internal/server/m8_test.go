package server_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/SureshKhemka/llm-gateway/internal/gateway"
	"github.com/SureshKhemka/llm-gateway/internal/observe"
	"github.com/SureshKhemka/llm-gateway/internal/sim"
)

func setupSpans(t *testing.T) *tracetest.SpanRecorder {
	rec := tracetest.NewSpanRecorder()
	shutdown, err := observe.SetupTracing(context.Background(), observe.TracingConfig{}, rec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	return rec
}

func scrape(t *testing.T, url string) string {
	resp, err := http.Get(url + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// Scenario 15: inspect a cascaded request — the trace has a span per attempt, the
// per-request event and the metrics agree, and the request explorer shows the decision.
// Covers: OBS-01, OBS-02, OBS-03, ADM-02
func TestScenario15_ObservabilityOfACascadedRequest(t *testing.T) {
	rec := setupSpans(t)
	env := m5Env(t)
	env.Sims["sima"].SetFaults("qwen-4b", sim.Faults{MalformedJSON: true})
	traceID := "4bf92f3577b34da6a3ce929d0e0e4736"
	r := env.Chat(key1, "cascade", "extract: Asha is 34", map[string]any{"temperature": 0.9,
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "p", "schema": personSchema}}},
		map[string]string{"traceparent": "00-" + traceID + "-00f067aa0ba902b7-01"})
	if r.Status != 200 || r.Header.Get("X-Gw-Route-Reason") != "cascade_escalated" {
		t.Fatalf("cascade: %d %s", r.Status, r.Header.Get("X-Gw-Route-Reason"))
	}
	// Trace: continues the caller's trace, one span per upstream attempt with GenAI attributes.
	names := map[string]int{}
	var attempts []string
	for _, sp := range rec.Ended() {
		if sp.SpanContext().TraceID().String() != traceID {
			continue
		}
		names[sp.Name()]++
		if strings.HasPrefix(sp.Name(), "chat ") {
			attrs := map[string]string{}
			for _, kv := range sp.Attributes() {
				attrs[string(kv.Key)] = kv.Value.Emit()
			}
			if attrs["gen_ai.request.model"] == "" || attrs["gen_ai.usage.input_tokens"] == "" || attrs["gen_ai.operation.name"] != "chat" {
				t.Errorf("attempt span lacks GenAI attributes: %v", attrs)
			}
			attempts = append(attempts, attrs["gateway.deployment"])
		}
	}
	if names["gateway.auth"] != 1 || names["gateway.route"] != 1 || names["gateway.cache.lookup"] != 1 || len(attempts) != 2 ||
		names["gateway.request /v1/chat/completions"] != 1 {
		t.Fatalf("spans in the caller's trace: %v attempts %v", names, attempts)
	}
	// Event: one per request, attempts match, trace ID recorded.
	ev, _ := env.Events.Last()
	if len(ev.Attempts) != 2 || ev.Attempts[0].Status != "invalid" || ev.TraceID != traceID || ev.CostUSD <= 0 {
		t.Fatalf("event: %+v", ev)
	}
	// Metrics agree with the event.
	m := scrape(t, env.HTTP.URL)
	for _, want := range []string{`gw_cascade_escalations_total{policy="cascade-json"} 1`, `gw_requests_total{cache="bypass",model="sima/qwen-9b"`,
		"gw_gateway_overhead_seconds_bucket", "gw_cost_usd_total", "gw_circuit_state", "gw_deployment_queue_depth"} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	// Request explorer shows the decision with both attempts.
	env.Flush()
	d := env.Do("GET", "/admin/v1/requests/"+r.Header.Get("X-Request-Id"), adminOrg, nil, nil)
	var out struct {
		Decision gateway.Decision `json:"decision"`
	}
	if d.Status != 200 || d.JSON(&out) != nil || len(out.Decision.Attempts) != 2 {
		t.Fatalf("request explorer: %d %s", d.Status, d.Body)
	}
}

// Covers: OBS-02
func TestOBS02_MetricLabelsAreBounded(t *testing.T) {
	env := m5Env(t)
	for i := 0; i < 5; i++ {
		env.Chat(key1, "no-such-model-"+string(rune('a'+i)), "hi", nil)
	}
	m := scrape(t, env.HTTP.URL)
	if strings.Contains(m, "no-such-model") {
		t.Fatal("unbounded model label exported")
	}
}

// Covers: OBS-05
func TestOBS05_LivenessAndReadiness(t *testing.T) {
	env := m5Env(t)
	ready := true
	env.Server.Ready = func() (bool, string) {
		if !ready {
			return false, "no provider reachable"
		}
		return true, ""
	}
	if r := env.Do("GET", "/livez", "", nil, nil); r.Status != 200 {
		t.Fatalf("livez %d", r.Status)
	}
	if r := env.Do("GET", "/readyz", "", nil, nil); r.Status != 200 {
		t.Fatalf("readyz %d", r.Status)
	}
	ready = false
	if r := env.Do("GET", "/readyz", "", nil, nil); r.Status != 503 || !strings.Contains(string(r.Body), "no provider") {
		t.Fatalf("readyz when unready: %d %s", r.Status, r.Body)
	}
}

// Covers: ADM-02, ADM-03, NFR-13
func TestADM02_ConsoleAndPlayground(t *testing.T) {
	env := m5Env(t)
	r := env.Do("GET", "/console/", "", nil, nil)
	if r.Status != 200 || !strings.Contains(string(r.Body), "LLM Gateway Console") || !strings.Contains(r.Header.Get("Content-Security-Policy"), "default-src 'self'") ||
		r.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("console: %d %v", r.Status, r.Header)
	}
	js := string(env.Do("GET", "/console/console.js", "", nil, nil).Body)
	for _, view := range []string{"overview", "health", "catalog", "policies", "tenants", "budgets", "requests", "costs", "evals", "audit", "playground"} {
		if !strings.Contains(js, "async "+view+"()") {
			t.Errorf("console lacks the %s view", view)
		}
	}
	env.Chat(key1, "auto", "hello console", nil)
	if r := env.Do("GET", "/admin/v1/overview", adminOrg, nil, nil); r.Status != 200 || !strings.Contains(string(r.Body), "spend_today_usd") || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("overview: %d %s", r.Status, r.Body)
	}
	// Playground: explain (no model call) and side-by-side run with actual costs.
	r = env.Post("/admin/v1/playground/explain", adminOrg, map[string]any{"tenant": "t1", "prompt": "Explain why the sky is blue"})
	if r.Status != 200 || !strings.Contains(string(r.Body), "est_cost_usd") {
		t.Fatalf("playground explain: %d %s", r.Status, r.Body)
	}
	r = env.Post("/admin/v1/playground/run", adminOrg, map[string]any{"tenant": "t1", "prompt": "compare us", "models": []string{"sima/qwen-4b", "sima/qwen-27b"}})
	var pr struct {
		Results []struct {
			ServedModel string  `json:"served_model"`
			CostUSD     float64 `json:"cost_usd"`
			Answer      string
		}
	}
	if r.JSON(&pr) != nil || len(pr.Results) != 2 || pr.Results[1].CostUSD <= pr.Results[0].CostUSD || pr.Results[0].Answer == "" {
		t.Fatalf("playground run: %s", r.Body)
	}
}
