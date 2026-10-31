package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/gateway"
	"github.com/SureshKhemka/llm-gateway/internal/sim"
	"github.com/SureshKhemka/llm-gateway/internal/testenv"
)

const m1YAML = `
providers:
  - id: sima
    type: openai
    base_url: ${SIM_SIMA_URL}
tenants:
  - id: acme
    projects:
      - id: web
        keys:
          - id: k1
            secret: env:TEST_KEY_1
          - id: k2
            secret: env:TEST_KEY_2
            scopes: {models: ["sima/qwen-4b"]}
defaults:
  baseline: sima/qwen-27b
` + testenv.StandardDeployments

const (
	key1 = "gw-test-k1_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	key2 = "gw-test-k2_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func m1Env(t *testing.T) *testenv.Env {
	return testenv.New(t, testenv.Options{YAML: m1YAML, Env: map[string]string{"TEST_KEY_1": key1, "TEST_KEY_2": key2}})
}

// Covers: API-01, API-07, API-09, PRV-01, CST-01
func TestAPI01_ChatCompletionNonStreaming(t *testing.T) {
	env := m1Env(t)
	r := env.Post("/v1/chat/completions", key1, map[string]any{
		"model": "sima/qwen-9b",
		"messages": []map[string]any{
			{"role": "system", "content": "You are terse."},
			{"role": "user", "content": "hello there"},
			{"role": "assistant", "content": "Hi."},
			{"role": "user", "content": "what is a gateway"},
		},
		"temperature": 0.2, "top_p": 0.9, "stop": []string{"END"}, "max_tokens": 50,
	})
	if r.Status != 200 {
		t.Fatalf("status %d: %s", r.Status, r.Body)
	}
	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := r.JSON(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Model != "sima/qwen-9b" || !strings.Contains(resp.Choices[0].Message.Content, "gateway") {
		t.Fatalf("unexpected response: %s", r.Body)
	}
	// API-09: every response carries the routing and cost metadata.
	for _, h := range []string{"X-Request-Id", "X-Gw-Served-Model", "X-Gw-Provider", "X-Gw-Route-Reason", "X-Gw-Cache",
		"X-Gw-Input-Tokens", "X-Gw-Output-Tokens", "X-Gw-Cost-Usd", "X-Gw-Fallback-Attempts"} {
		if r.Header.Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
	if r.Header.Get("X-Gw-Route-Reason") != "explicit_model" || r.Header.Get("X-Gw-Provider") != "sima" {
		t.Errorf("route headers: %v", r.Header)
	}
	// CST-01: cost = input × $0.50/M + output × $2.00/M for qwen-9b.
	in, _ := strconv.Atoi(r.Header.Get("X-Gw-Input-Tokens"))
	out, _ := strconv.Atoi(r.Header.Get("X-Gw-Output-Tokens"))
	got, _ := strconv.ParseFloat(r.Header.Get("X-Gw-Cost-Usd"), 64)
	want := float64(in)*0.5/1e6 + float64(out)*2.0/1e6
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("cost %v, want %v", got, want)
	}
	if in != resp.Usage.PromptTokens || out != resp.Usage.CompletionTokens {
		t.Errorf("header tokens %d/%d differ from usage %+v", in, out, resp.Usage)
	}
}

// Covers: API-01, API-09, API-13
func TestAPI01_StreamingSSE(t *testing.T) {
	env := m1Env(t)
	r := env.Chat(key1, "sima/qwen-4b", "stream these words please", map[string]any{"stream": true, "stream_options": map[string]any{"include_usage": true}})
	if r.Status != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d %s", r.Status, r.Header.Get("Content-Type"))
	}
	events, done := testenv.SSE(r.Body)
	if !done {
		t.Fatal("stream did not end with [DONE]")
	}
	if txt := testenv.StreamText(events); !strings.Contains(txt, "stream these words") {
		t.Fatalf("stream text %q", txt)
	}
	var last struct {
		Choices []any          `json:"choices"`
		Usage   map[string]int `json:"usage"`
		Gateway map[string]any `json:"x_gateway"`
	}
	_ = json.Unmarshal(events[len(events)-1], &last)
	if len(last.Choices) != 0 || last.Usage["completion_tokens"] == 0 {
		t.Fatalf("final chunk should carry usage: %s", events[len(events)-1])
	}
	if last.Gateway["served_model"] != "sima/qwen-4b" || last.Gateway["cost_usd"] == nil {
		t.Fatalf("final chunk should carry gateway metadata: %v", last.Gateway)
	}
	if r.Header.Get("X-Gw-Served-Model") != "sima/qwen-4b" {
		t.Errorf("served model header missing on stream")
	}
	if r.Trailer.Get("X-Gw-Cost-Usd") == "" {
		t.Errorf("cost trailer missing: %v", r.Trailer)
	}
}

// Covers: API-01, API-13
func TestAPI01_StreamWithoutUsageRequestOmitsUsageChunk(t *testing.T) {
	env := m1Env(t)
	r := env.Chat(key1, "sima/qwen-4b", "hello", map[string]any{"stream": true})
	events, done := testenv.SSE(r.Body)
	if !done {
		t.Fatal("no [DONE]")
	}
	for _, ev := range events {
		if strings.Contains(string(ev), `"usage"`) {
			t.Fatalf("usage chunk sent although not requested: %s", ev)
		}
	}
}

// Covers: API-01
func TestAPI01_ToolCalling(t *testing.T) {
	env := m1Env(t)
	tools := []map[string]any{{"type": "function", "function": map[string]any{
		"name": "get_weather", "description": "Weather for a city",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{"city"}},
	}}}
	r := env.Chat(key1, "sima/qwen-9b", "weather in Pune?", map[string]any{"tools": tools, "tool_choice": "auto"})
	var resp struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	_ = r.JSON(&resp)
	if r.Status != 200 || len(resp.Choices[0].Message.ToolCalls) != 1 || resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("expected a tool call: %s", r.Body)
	}
	call := resp.Choices[0].Message.ToolCalls[0]
	var args map[string]any
	if call.Function.Name != "get_weather" || json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args["city"] == nil {
		t.Fatalf("bad tool call %+v", call)
	}
	// Second turn with the tool result.
	r2 := env.Post("/v1/chat/completions", key1, map[string]any{
		"model": "sima/qwen-9b", "tools": tools,
		"messages": []map[string]any{
			{"role": "user", "content": "weather in Pune?"},
			{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{"id": call.ID, "type": "function", "function": map[string]any{"name": "get_weather", "arguments": call.Function.Arguments}}}},
			{"role": "tool", "tool_call_id": call.ID, "content": "31C and sunny"},
		},
	})
	if r2.Status != 200 || !strings.Contains(string(r2.Body), "31C") {
		t.Fatalf("tool result turn: %d %s", r2.Status, r2.Body)
	}
}

// Covers: API-01
func TestAPI01_StreamingToolCalls(t *testing.T) {
	env := m1Env(t)
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "lookup",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}}}}
	r := env.Chat(key1, "sima/qwen-9b", "look it up", map[string]any{"tools": tools, "stream": true})
	events, _ := testenv.SSE(r.Body)
	var name, args string
	for _, ev := range events {
		var ch struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct{ Name, Arguments string }
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		_ = json.Unmarshal(ev, &ch)
		for _, c := range ch.Choices {
			for _, tc := range c.Delta.ToolCalls {
				name += tc.Function.Name
				args += tc.Function.Arguments
			}
		}
	}
	var parsed map[string]any
	if name != "lookup" || json.Unmarshal([]byte(args), &parsed) != nil {
		t.Fatalf("streamed tool call name=%q args=%q", name, args)
	}
}

// Covers: API-01
func TestAPI01_ResponseFormatJSONSchema(t *testing.T) {
	env := m1Env(t)
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"name": map[string]any{"type": "string"}, "age": map[string]any{"type": "integer"}}, "required": []string{"name", "age"}}
	r := env.Chat(key1, "sima/qwen-4b", "extract: Asha is 34", map[string]any{
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "person", "schema": schema}}})
	var resp struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	_ = r.JSON(&resp)
	var obj map[string]any
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &obj); err != nil || obj["name"] == nil || obj["age"] == nil {
		t.Fatalf("not schema-shaped JSON: %s", resp.Choices[0].Message.Content)
	}
	r2 := env.Chat(key1, "sima/qwen-4b", "give json", map[string]any{"response_format": map[string]any{"type": "json_object"}})
	_ = r2.JSON(&resp)
	if json.Unmarshal([]byte(resp.Choices[0].Message.Content), &obj) != nil {
		t.Fatalf("json_object mode not JSON: %s", resp.Choices[0].Message.Content)
	}
}

// Covers: API-01
func TestAPI01_ImageInput(t *testing.T) {
	env := m1Env(t)
	r := env.Post("/v1/chat/completions", key1, map[string]any{"model": "sima/qwen-vl-8b", "messages": []map[string]any{{
		"role": "user", "content": []map[string]any{
			{"type": "text", "text": "what is in this picture?"},
			{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgo="}},
		}}}})
	if r.Status != 200 || !strings.Contains(string(r.Body), "image") {
		t.Fatalf("image request: %d %s", r.Status, r.Body)
	}
}

// Covers: API-03
func TestAPI03_Embeddings(t *testing.T) {
	env := m1Env(t)
	r := env.Post("/v1/embeddings", key1, map[string]any{"model": "sima/nomic-embed", "input": []string{"hello world", "goodbye"}})
	var resp struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage struct{ PromptTokens int } `json:"usage"`
	}
	if r.Status != 200 || r.JSON(&resp) != nil || len(resp.Data) != 2 || len(resp.Data[0].Embedding) != 256 {
		t.Fatalf("embeddings: %d %s", r.Status, r.Body)
	}
	if r.Header.Get("X-Gw-Cost-Usd") == "" {
		t.Error("embeddings response lacks cost header")
	}
	// Chat models cannot serve embeddings.
	if r := env.Post("/v1/embeddings", key1, map[string]any{"model": "sima/qwen-4b", "input": "x"}); r.Status != 400 {
		t.Errorf("chat model for embeddings: %d", r.Status)
	}
}

// Covers: API-04, TEN-03
func TestAPI04_ModelsListedPerKey(t *testing.T) {
	env := m1Env(t)
	ids := func(key string) map[string]bool {
		var l struct {
			Data []struct{ ID string } `json:"data"`
		}
		_ = env.Do("GET", "/v1/models", key, nil, nil).JSON(&l)
		out := map[string]bool{}
		for _, d := range l.Data {
			out[d.ID] = true
		}
		return out
	}
	all := ids(key1)
	if !all["auto"] || !all["sima/qwen-27b"] || !all["sima/nomic-embed"] {
		t.Fatalf("full key should see virtual and concrete models: %v", all)
	}
	limited := ids(key2)
	if len(limited) != 1 || !limited["sima/qwen-4b"] {
		t.Fatalf("scoped key should see only its model: %v", limited)
	}
	if r := env.Chat(key2, "sima/qwen-27b", "hi", nil); r.Status != 403 {
		t.Fatalf("scoped key used a forbidden model: %d", r.Status)
	}
}

// Covers: API-07
func TestAPI07_ConcreteModelByProviderSlashUpstreamName(t *testing.T) {
	env := m1Env(t)
	r := env.Chat(key1, "sima/qwen-4b", "hi", nil)
	if r.Status != 200 || r.Header.Get("X-Gw-Served-Model") != "sima/qwen-4b" {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	if r := env.Chat(key1, "sima/does-not-exist", "hi", nil); r.Status != 404 {
		t.Fatalf("unknown model: %d", r.Status)
	}
}

// Covers: API-10
func TestAPI10_ErrorShapesAndCodes(t *testing.T) {
	env := m1Env(t)
	cases := []struct {
		name   string
		resp   *testenv.Response
		status int
		code   string
	}{
		{"no key", env.Chat("", "sima/qwen-4b", "hi", nil), 401, "unauthorized"},
		{"bad key", env.Chat("nope", "sima/qwen-4b", "hi", nil), 401, "unauthorized"},
		{"forbidden model", env.Chat(key2, "sima/qwen-9b", "hi", nil), 403, "forbidden"},
		{"bad json", env.Post("/v1/chat/completions", key1, "{not json"), 400, "invalid_request"},
		{"no messages", env.Post("/v1/chat/completions", key1, map[string]any{"model": "sima/qwen-4b", "messages": []any{}}), 400, "invalid_request"},
		{"bad hint", env.Chat(key1, "sima/qwen-4b", "hi", nil, map[string]string{"X-Gw-Max-Cost-Usd": "lots"}), 400, "invalid_request"},
	}
	for _, c := range cases {
		var body struct {
			Error struct {
				Message, Type string
				Code          string `json:"code"`
			} `json:"error"`
		}
		if c.resp.Status != c.status || c.resp.JSON(&body) != nil || body.Error.Code != c.code || body.Error.Message == "" || body.Error.Type == "" {
			t.Errorf("%s: status %d body %s", c.name, c.resp.Status, c.resp.Body)
		}
		if c.resp.Header.Get("X-Gw-Error-Code") != c.code {
			t.Errorf("%s: missing X-Gw-Error-Code", c.name)
		}
	}
}

// Covers: API-10, RES-03
func TestAPI10_ContextTooLong(t *testing.T) {
	env := m1Env(t)
	long := strings.Repeat("word ", 9000)
	r := env.Chat(key1, "sima/gemma-e2b", long, nil)
	if r.Status != 400 || !strings.Contains(string(r.Body), "context_too_long") {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
}

// Covers: API-13, CST-02
func TestAPI13_UsageEstimatedWhenProviderOmitsIt(t *testing.T) {
	env := m1Env(t)
	env.Sims["sima"].SetFaults("", sim.Faults{OmitUsage: true})
	r := env.Chat(key1, "sima/qwen-4b", "count my tokens please", nil)
	if r.Status != 200 || r.Header.Get("X-Gw-Usage-Estimated") != "true" {
		t.Fatalf("expected estimated usage: %d %v", r.Status, r.Header)
	}
	if n, _ := strconv.Atoi(r.Header.Get("X-Gw-Input-Tokens")); n == 0 {
		t.Fatal("estimated input tokens should be > 0")
	}
	// Streaming: the gateway still reports usage in the final chunk.
	rs := env.Chat(key1, "sima/qwen-4b", "count my tokens please", map[string]any{"stream": true, "stream_options": map[string]any{"include_usage": true}})
	events, _ := testenv.SSE(rs.Body)
	if !strings.Contains(string(events[len(events)-1]), `"completion_tokens"`) || !strings.Contains(string(events[len(events)-1]), `"usage_estimated":true`) {
		t.Fatalf("final chunk: %s", events[len(events)-1])
	}
	env.Flush()
	recs, _ := env.Store.ListCostRecords(context.Background(), storeFilterAll(), 10)
	if len(recs) == 0 || !recs[0].Estimated {
		t.Fatalf("cost record should be flagged estimated: %+v", recs)
	}
}

// Covers: API-11
func TestAPI11_ClientDisconnectCancelsUpstreamAndBillsDelivered(t *testing.T) {
	env := m1Env(t)
	env.Sims["sima"].SetFaults("qwen-4b", sim.Faults{TokensPerSec: 20})
	body := `{"model":"sima/qwen-4b","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("alpha ", 20) + `"}]}`
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", env.HTTP.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key1)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	_, _ = resp.Body.Read(buf)
	time.Sleep(150 * time.Millisecond)
	cancel()
	resp.Body.Close()
	sentBefore := env.Sims["sima"].TokensSent.Load()
	time.Sleep(400 * time.Millisecond)
	if after := env.Sims["sima"].TokensSent.Load(); after > sentBefore+1 {
		t.Fatalf("upstream kept generating after disconnect: %d -> %d", sentBefore, after)
	}
	waitFor(t, func() bool {
		env.Flush()
		recs, _ := env.Store.ListCostRecords(context.Background(), storeFilterAll(), 1)
		return len(recs) == 1 && recs[0].Status == 499
	})
	env.Flush()
	recs, _ := env.Store.ListCostRecords(context.Background(), storeFilterAll(), 1)
	if recs[0].OutputTokens == 0 || recs[0].OutputTokens >= 25 || !recs[0].Estimated {
		t.Fatalf("should bill only delivered tokens (estimated): %+v", recs[0])
	}
}

// Covers: PRV-03, PRV-04, PRV-05
func TestPRV05_PriceCardInEffectAtRequestTime(t *testing.T) {
	yaml := m1YAML + `
  - id: sima/priced
    provider: sima
    model: qwen-4b
    tier: 1
    residency: [local]
    prices:
      - {effective_from: 2026-01-01T00:00:00Z, input: 1.0, output: 1.0}
      - {effective_from: 2099-01-01T00:00:00Z, input: 100.0, output: 100.0}
      - {effective_from: 2026-06-01T00:00:00Z, input: 2.0, output: 4.0}
`
	env := testenv.New(t, testenv.Options{YAML: yaml, Env: map[string]string{"TEST_KEY_1": key1, "TEST_KEY_2": key2}})
	d := env.Engine.Snapshot().Catalog.Deployments["sima/priced"]
	if p := d.PriceAt(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)); p.Input != 1.0 {
		t.Errorf("march price %+v", p)
	}
	if p := d.PriceAt(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); p.Input != 2.0 || p.Output != 4.0 {
		t.Errorf("october price %+v", p)
	}
	r := env.Chat(key1, "sima/priced", "price me", nil)
	in, _ := strconv.Atoi(r.Header.Get("X-Gw-Input-Tokens"))
	out, _ := strconv.Atoi(r.Header.Get("X-Gw-Output-Tokens"))
	got, _ := strconv.ParseFloat(r.Header.Get("X-Gw-Cost-Usd"), 64)
	if want := (float64(in)*2 + float64(out)*4) / 1e6; got-want > 1e-12 || want-got > 1e-12 {
		t.Fatalf("cost %v want %v (current card)", got, want)
	}
	// PRV-04: a free local model costs zero.
	free := testenv.New(t, testenv.Options{YAML: strings.Replace(m1YAML, "input: 0.15, output: 0.60", "input: 0, output: 0", 1), Env: map[string]string{"TEST_KEY_1": key1, "TEST_KEY_2": key2}})
	if c := free.Chat(key1, "sima/qwen-4b", "free", nil).Header.Get("X-Gw-Cost-Usd"); c != "0.0000000000" {
		t.Errorf("free model cost %s", c)
	}
}

// Covers: CFG-02, CFG-04
func TestCFG02_ValidationReportsFilePathReason(t *testing.T) {
	bad := `
providers:
  - id: p1
    type: openai
    base_url: http://localhost:1234/v1
    api_key: sk-inline-secret
deployments:
  - id: d1
    provider: nope
    model: m
    tier: 7
    capabilities: [chat, telepathy]
    prices: [{effective_from: 2026-01-01T00:00:00Z, input: -1, output: 1}]
`
	_, err := config.Parse("gateway.yaml", []byte(bad))
	ve, ok := err.(*config.ValidationError)
	if !ok {
		t.Fatalf("expected validation error, got %v", err)
	}
	want := map[string]string{
		"providers[0].api_key":           "secret",
		"deployments[0].provider":        "unknown provider",
		"deployments[0].tier":            "between 0 and 3",
		"deployments[0].capabilities[1]": "unknown capability",
		"deployments[0].prices[0]":       "≥ 0",
	}
	for path, frag := range want {
		found := false
		for _, is := range ve.Issues {
			if is.Path == path && strings.Contains(is.Reason, frag) && is.File == "gateway.yaml" {
				found = true
			}
		}
		if !found {
			t.Errorf("missing issue %s (%s) in %v", path, frag, ve.Issues)
		}
	}
	// Unknown fields are rejected with the YAML line.
	_, err = config.Parse("g.yaml", []byte("providers:\n  - id: p\n    typo_field: 1\n"))
	if err == nil || !strings.Contains(err.Error(), "typo_field") || !strings.Contains(err.Error(), "line") {
		t.Errorf("unknown field error: %v", err)
	}
}

// Covers: CFG-07, PRV-06
func TestCFG07_MinimalConfigWithDiscovery(t *testing.T) {
	minimal := `
providers:
  - id: lmstudio
    base_url: ${SIM_LMSTUDIO_URL}
`
	env := testenv.New(t, testenv.Options{Sims: map[string][]sim.Model{"lmstudio": {{ID: "qwen3-4b", Tier: 1}, {ID: "text-embedding-nomic", Embeddings: true}}}, YAML: minimal})
	s := env.Engine.Snapshot()
	disc := gateway.NewDiscovery()
	found := disc.Run(context.Background(), s)
	if len(found) != 2 {
		t.Fatalf("discovered %d models", len(found))
	}
	snap, err := gateway.BuildSnapshot(gateway.BuildInput{Config: s.Config, Discovered: found, PrevVersion: s.Version})
	if err != nil {
		t.Fatal(err)
	}
	env.Engine.Swap(snap)
	r := env.Chat("", "lmstudio/qwen3-4b", "hello local", nil)
	if r.Status != 200 {
		t.Fatalf("discovered model chat: %d %s", r.Status, r.Body)
	}
	if d := snap.Catalog.Deployments["lmstudio/text-embedding-nomic"]; d == nil || !d.Has("embeddings") || !d.IsLocal() {
		t.Fatalf("embedding model metadata: %+v", d)
	}
}

// Covers: PRV-06
func TestPRV06_DiscoveredModelsNeedApproval(t *testing.T) {
	yaml := `
providers:
  - id: sima
    base_url: ${SIM_SIMA_URL}
    discover: true
deployments:
  - id: sima/qwen-4b
    provider: sima
    model: qwen-4b
    prices: [{effective_from: 2026-01-01T00:00:00Z, input: 0.1, output: 0.1}]
`
	env := testenv.New(t, testenv.Options{YAML: yaml})
	disc := gateway.NewDiscovery()
	found := disc.Run(context.Background(), env.Engine.Snapshot())
	var unapproved int
	for _, d := range found {
		if !d.Approved {
			unapproved++
		}
	}
	if unapproved != len(testenv.StandardModels)-1 {
		t.Fatalf("expected new models unregistered, got %d unapproved of %d", unapproved, len(found))
	}
	snap, _ := gateway.BuildSnapshot(gateway.BuildInput{Config: env.Config, Discovered: found})
	env.Engine.Swap(snap)
	if r := env.Chat("", "sima/qwen-27b", "hi", nil); r.Status != 404 {
		t.Fatalf("unapproved model usable: %d", r.Status)
	}
	disc.Approve("sima/qwen-27b", &config.Deployment{Tier: 3, Capabilities: []string{"chat"}, Prices: []config.PriceCard{{Input: 3, Output: 15}}})
	snap, _ = gateway.BuildSnapshot(gateway.BuildInput{Config: env.Config, Discovered: disc.List()})
	env.Engine.Swap(snap)
	if r := env.Chat("", "sima/qwen-27b", "hi", nil); r.Status != 200 {
		t.Fatalf("approved model: %d %s", r.Status, r.Body)
	}
}

// Covers: NFR-16, OBS-01
func TestNFR16_EventsCarryNoContentOrKeys(t *testing.T) {
	env := m1Env(t)
	secretPrompt := "my-super-secret-prompt-text"
	env.Chat(key1, "sima/qwen-4b", secretPrompt, nil)
	ev, ok := env.Events.Last()
	if !ok {
		t.Fatal("no event emitted")
	}
	b, _ := json.Marshal(ev)
	if strings.Contains(string(b), secretPrompt) || strings.Contains(string(b), key1) {
		t.Fatalf("event leaks content or key: %s", b)
	}
	if ev.KeyPrefix != "gw-test-k1_" || ev.Tenant != "acme" || ev.ServedModel != "sima/qwen-4b" || ev.CostUSD <= 0 || ev.LatencyMs <= 0 {
		t.Fatalf("event fields: %+v", ev)
	}
}
