package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SureshKhemka/llm-gateway/internal/sim"
	"github.com/SureshKhemka/llm-gateway/internal/store"
	"github.com/SureshKhemka/llm-gateway/internal/testenv"
)

const m6YAML = `
providers:
  - id: sima
    type: openai
    base_url: ${SIM_SIMA_URL}
  - id: anth
    type: anthropic
    base_url: ${SIM_ANTH_URL}
    api_key: env:ANTH_KEY
  - id: cloud
    type: openai
    base_url: ${SIM_CLOUD_URL}
model_aliases:
  - {match: "claude-*", model: auto}
storage:
  body_key: env:BODY_KEY
guardrails:
  pii: log
  sensitivity:
    confidential: [local]
policies:
  - name: local-anthropic
    objective: min_cost
virtual_models:
  - {name: via-anthropic, policy: local-anthropic, deployments: [anth/claude-local]}
tenants:
  - id: acme
    projects: [{id: web, keys: [{id: k1, secret: env:TEST_KEY_1}]}]
  - id: redactco
    pii: redact
    projects: [{id: r1, keys: [{id: k2, secret: env:TEST_KEY_2}]}]
  - id: blockco
    pii: block
    body_logging: {enabled: true, retention: 1h}
    caps: {max_messages: 4, max_output_tokens: 10}
    projects: [{id: b1, keys: [{id: k3, secret: env:TEST_KEY_3}]}]
defaults:
  baseline: sima/qwen-27b
` + testenv.StandardDeployments + `
  - id: anth/claude-local
    provider: anth
    model: qwen-9b
    tier: 2
    capabilities: [chat, tools, json_schema, vision]
    context_window: 32768
    residency: [local]
    prices: [{effective_from: 2026-01-01T00:00:00Z, input: 0.5, output: 2.0}]
  - id: cloud/frontier
    provider: cloud
    model: qwen-27b
    tier: 3
    capabilities: [chat, tools, json_schema]
    context_window: 131072
    residency: [external]
    prices: [{effective_from: 2026-01-01T00:00:00Z, input: 0.01, output: 0.01}]
`

func m6Env(t *testing.T, mutate ...func(string) string) *testenv.Env {
	yaml := m6YAML
	for _, m := range mutate {
		yaml = m(yaml)
	}
	return testenv.New(t, testenv.Options{
		Sims: map[string][]sim.Model{"sima": testenv.StandardModels, "anth": testenv.StandardModels, "cloud": testenv.StandardModels},
		YAML: yaml, Env: map[string]string{"TEST_KEY_1": key1, "TEST_KEY_2": key2, "TEST_KEY_3": key3, "ANTH_KEY": "sk-ant-test",
			"BODY_KEY": "body-key-0123456789abcdef"}})
}

func messages(env *testenv.Env, key string, body map[string]any, headers ...map[string]string) *testenv.Response {
	h := map[string]string{"anthropic-version": "2023-06-01", "x-api-key": key}
	for _, x := range headers {
		for k, v := range x {
			h[k] = v
		}
	}
	return env.Do("POST", "/v1/messages", "", body, h)
}

type anthropicEvent struct {
	Name string
	Data map[string]any
}

func parseAnthropicSSE(t *testing.T, body []byte) []anthropicEvent {
	var out []anthropicEvent
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var name string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var m map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
				t.Fatalf("bad event data %q: %v", line, err)
			}
			out = append(out, anthropicEvent{name, m})
		}
	}
	return out
}

func anthropicText(evs []anthropicEvent) string {
	var sb strings.Builder
	for _, e := range evs {
		if e.Name == "content_block_delta" {
			if d, ok := e.Data["delta"].(map[string]any); ok && d["type"] == "text_delta" {
				sb.WriteString(d["text"].(string))
			}
		}
	}
	return sb.String()
}

var weatherTool = []map[string]any{{"name": "get_weather", "description": "Weather", "input_schema": map[string]any{
	"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{"city"}}}}

// Covers: API-02, API-06
func TestAPI02_MessagesEndpointOverBothUpstreamFormats(t *testing.T) {
	env := m6Env(t)
	for _, model := range []string{"sima/qwen-9b", "anth/claude-local"} {
		// System prompt as an array of blocks with cache_control, multi-turn, stop sequences.
		r := messages(env, key1, map[string]any{"model": model, "max_tokens": 200,
			"system":         []map[string]any{{"type": "text", "text": "Be brief.", "cache_control": map[string]any{"type": "ephemeral"}}},
			"messages":       []map[string]any{{"role": "user", "content": "hello"}, {"role": "assistant", "content": "Hi!"}, {"role": "user", "content": "tell me about gateways"}},
			"stop_sequences": []string{"END"}})
		var resp struct {
			Type       string `json:"type"`
			Model      string `json:"model"`
			StopReason string `json:"stop_reason"`
			Content    []struct{ Type, Text string }
			Usage      struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if r.Status != 200 || r.JSON(&resp) != nil || resp.Type != "message" || resp.Model != model || resp.StopReason != "end_turn" ||
			!strings.Contains(resp.Content[0].Text, "gateways") || resp.Usage.OutputTokens == 0 {
			t.Fatalf("%s: %d %s", model, r.Status, r.Body)
		}
		if r.Header.Get("X-Gw-Cost-Usd") == "" || r.Header.Get("X-Gw-Served-Model") != model {
			t.Errorf("%s: gateway headers missing", model)
		}
		// Streaming.
		r = messages(env, key1, map[string]any{"model": model, "max_tokens": 100, "stream": true,
			"messages": []map[string]any{{"role": "user", "content": "stream about rivers"}}})
		evs := parseAnthropicSSE(t, r.Body)
		names := []string{}
		for _, e := range evs {
			names = append(names, e.Name)
		}
		if evs[0].Name != "message_start" || evs[len(evs)-1].Name != "message_stop" || !strings.Contains(anthropicText(evs), "rivers") {
			t.Fatalf("%s stream events: %v", model, names)
		}
		if ms := evs[0].Data["message"].(map[string]any); ms["model"] != model {
			t.Errorf("%s: message_start model %v", model, ms["model"])
		}
		// Tool use and the tool_result round trip.
		r = messages(env, key1, map[string]any{"model": model, "max_tokens": 200, "tools": weatherTool,
			"messages": []map[string]any{{"role": "user", "content": "weather in Pune?"}}})
		var tu struct {
			StopReason string `json:"stop_reason"`
			Content    []struct {
				Type, ID, Name string
				Input          map[string]any
			}
		}
		_ = r.JSON(&tu)
		var call *struct {
			Type, ID, Name string
			Input          map[string]any
		}
		for i := range tu.Content {
			if tu.Content[i].Type == "tool_use" {
				call = &tu.Content[i]
			}
		}
		if tu.StopReason != "tool_use" || call == nil || call.Name != "get_weather" || call.Input["city"] == nil {
			t.Fatalf("%s tool use: %s", model, r.Body)
		}
		r = messages(env, key1, map[string]any{"model": model, "max_tokens": 200, "tools": weatherTool, "messages": []map[string]any{
			{"role": "user", "content": "weather in Pune?"},
			{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": call.ID, "name": "get_weather", "input": call.Input}}},
			{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": call.ID, "content": "31C sunny"}}},
		}})
		if r.Status != 200 || !strings.Contains(string(r.Body), "31C") {
			t.Fatalf("%s tool result turn: %d %s", model, r.Status, r.Body)
		}
		// Images (base64 source).
		r = messages(env, key1, map[string]any{"model": model, "max_tokens": 50, "messages": []map[string]any{{"role": "user", "content": []map[string]any{
			{"type": "text", "text": "what is this?"},
			{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}}}}}})
		if r.Status != 200 {
			t.Fatalf("%s image: %d %s", model, r.Status, r.Body)
		}
	}
	// The Anthropic upstream received the request in its own format, with its key.
	if env.Sims["anth"].Requests.Load() == 0 {
		t.Fatal("anthropic-format upstream never called")
	}
}

// Covers: API-06, API-01
func TestAPI06_OpenAIClientToAnthropicUpstream(t *testing.T) {
	env := m6Env(t)
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}}}}}
	r := env.Chat(key1, "anth/claude-local", "weather in Goa?", map[string]any{"tools": tools})
	var resp struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if r.Status != 200 || r.JSON(&resp) != nil || resp.Choices[0].FinishReason != "tool_calls" || len(resp.Choices[0].Message.ToolCalls) != 1 || resp.Usage.PromptTokens == 0 {
		t.Fatalf("openai→anthropic tools: %d %s", r.Status, r.Body)
	}
	// Streaming tool calls translate to OpenAI deltas.
	r = env.Chat(key1, "anth/claude-local", "weather in Goa?", map[string]any{"tools": tools, "stream": true, "stream_options": map[string]any{"include_usage": true}})
	events, done := testenv.SSE(r.Body)
	var args string
	for _, ev := range events {
		var ch struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct{ Arguments string }
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		_ = json.Unmarshal(ev, &ch)
		for _, c := range ch.Choices {
			for _, tc := range c.Delta.ToolCalls {
				args += tc.Function.Arguments
			}
		}
	}
	var parsed map[string]any
	if !done || json.Unmarshal([]byte(args), &parsed) != nil || !strings.Contains(string(events[len(events)-1]), `"prompt_tokens"`) {
		t.Fatalf("streamed tool args %q (done=%v): %s", args, done, r.Body)
	}
	// JSON schema maps to output_config.format.
	schema := map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}}
	r = env.Chat(key1, "anth/claude-local", "name: Asha", map[string]any{"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "x", "schema": schema}}})
	if r.Status != 200 || !strings.Contains(string(r.Body), `\"name\"`) {
		t.Fatalf("json schema via anthropic: %s", r.Body)
	}
}

// Covers: API-02, API-10, API-07
func TestAPI02_AliasesCountTokensAndErrors(t *testing.T) {
	env := m6Env(t)
	r := messages(env, key1, map[string]any{"model": "claude-sonnet-4-5", "max_tokens": 50, "messages": []map[string]any{{"role": "user", "content": "hi"}}})
	if r.Status != 200 || r.Header.Get("X-Gw-Policy") == "" {
		t.Fatalf("alias claude-* → auto: %d %s", r.Status, r.Body)
	}
	r = env.Do("POST", "/v1/messages/count_tokens", "", map[string]any{"model": "claude-sonnet-4-5", "messages": []map[string]any{{"role": "user", "content": "count these words please"}}},
		map[string]string{"x-api-key": key1})
	var ct map[string]int
	if r.Status != 200 || r.JSON(&ct) != nil || ct["input_tokens"] == 0 {
		t.Fatalf("count_tokens: %d %s", r.Status, r.Body)
	}
	// Errors use Anthropic's shape with the gateway code.
	r = messages(env, "bad-key", map[string]any{"model": "auto", "max_tokens": 5, "messages": []map[string]any{{"role": "user", "content": "x"}}})
	var e struct {
		Type  string `json:"type"`
		Error struct{ Type, Message, Code string }
	}
	if r.Status != 401 || r.JSON(&e) != nil || e.Type != "error" || e.Error.Type != "authentication_error" || e.Error.Code != "unauthorized" {
		t.Fatalf("anthropic error shape: %d %s", r.Status, r.Body)
	}
	r = messages(env, key1, map[string]any{"model": "auto", "messages": []map[string]any{{"role": "user", "content": "x"}}})
	if r.Status != 400 || !strings.Contains(string(r.Body), "max_tokens") {
		t.Fatalf("missing max_tokens: %d %s", r.Status, r.Body)
	}
	// Server tools cannot run on an OpenAI-format upstream.
	r = messages(env, key1, map[string]any{"model": "sima/qwen-9b", "max_tokens": 5, "tools": []map[string]any{{"type": "web_search_20260209", "name": "web_search"}},
		"messages": []map[string]any{{"role": "user", "content": "x"}}})
	if r.Status != 400 || !strings.Contains(string(r.Body), "Anthropic-format deployment") {
		t.Fatalf("server tool on openai upstream: %d %s", r.Status, r.Body)
	}
	// Anthropic model list shape.
	r = env.Do("GET", "/v1/models", key1, nil, map[string]string{"anthropic-version": "2023-06-01"})
	if !strings.Contains(string(r.Body), `"has_more"`) || !strings.Contains(string(r.Body), `"type": "model"`) && !strings.Contains(string(r.Body), `"type":"model"`) {
		t.Fatalf("anthropic models list: %s", r.Body)
	}
}

// Scenario 11: a confidential request with a cloud provider configured sees only local
// candidates; a prompt with an email and a PAN shows PII detections in log-only mode.
// Covers: GRD-02, GRD-04, RTE-01, RES-08
func TestScenario11_DataResidencyAndPII(t *testing.T) {
	env := m6Env(t)
	// The cloud deployment is the cheapest tier-3: a hard prompt goes there by default.
	hard := "Reason step by step: prove the bound, then derive the corollary and analyze the trade-offs."
	if r := env.Chat(key1, "auto", hard, nil); r.Header.Get("X-Gw-Served-Model") != "cloud/frontier" {
		t.Fatalf("baseline: %s", r.Header.Get("X-Gw-Served-Model"))
	}
	st, dec := explain(t, env, key1, userMsg("auto", hard), map[string]string{"X-Gw-Sensitivity": "confidential"})
	if st != 200 {
		t.Fatalf("explain: %d", st)
	}
	for _, c := range dec.Candidates {
		if !env.Engine.Snapshot().Catalog.Deployments[c.Deployment].IsLocal() {
			t.Errorf("non-local candidate %s for a confidential request", c.Deployment)
		}
	}
	found := false
	for _, ex := range dec.Exclusions {
		if ex.Deployment == "cloud/frontier" && strings.Contains(strings.Join(ex.Reasons, ","), "residency") {
			found = true
		}
	}
	if !found {
		t.Errorf("cloud deployment not excluded for residency: %+v", dec.Exclusions)
	}
	r := env.Chat(key1, "auto", hard, nil, map[string]string{"X-Gw-Sensitivity": "confidential"})
	if r.Status != 200 || r.Header.Get("X-Gw-Served-Model") == "cloud/frontier" {
		t.Fatalf("confidential request served by %s", r.Header.Get("X-Gw-Served-Model"))
	}
	// PII in log-only mode: detected and logged (types only), request still served.
	r = env.Chat(key1, "auto", hard+" Contact asha.k@example.in, PAN ABCPE1234F.", nil)
	ev, _ := env.Events.Last()
	if r.Status != 200 || strings.Join(ev.PII, ",") != "email,pan" {
		t.Fatalf("PII log-only: %d %v", r.Status, ev.PII)
	}
	b, _ := json.Marshal(ev)
	if strings.Contains(string(b), "asha.k@example.in") {
		t.Fatal("event contains the PII itself")
	}
	if r := env.Chat(key1, "auto", hard, nil, map[string]string{"X-Gw-Sensitivity": "secret-ish"}); r.Status != 400 {
		t.Fatalf("unknown sensitivity accepted: %d", r.Status)
	}
}

// Covers: GRD-04
func TestGRD04_RedactAndBlockModes(t *testing.T) {
	env := m6Env(t)
	hard := "Reason step by step: prove the bound, then derive the corollary. Email me at asha.k@example.in"
	// Redact: the non-local deployment receives the redacted prompt (the simulator echoes it).
	r := env.Chat(key2, "auto", hard, nil)
	if r.Header.Get("X-Gw-Served-Model") != "cloud/frontier" || strings.Contains(string(r.Body), "asha.k@example.in") || !strings.Contains(string(r.Body), "REDACTED_EMAIL") {
		t.Fatalf("redact mode: %s %s", r.Header.Get("X-Gw-Served-Model"), r.Body)
	}
	// Local deployments still get the original text.
	r = env.Chat(key2, "sima/qwen-27b", hard, nil)
	if !strings.Contains(string(r.Body), "asha.k@example.in") {
		t.Fatalf("local deployment got redacted text: %s", r.Body)
	}
	// Block: non-local deployments drop out; a local one serves.
	r = env.Chat(key3, "auto", hard, nil)
	if r.Status != 200 || r.Header.Get("X-Gw-Served-Model") == "cloud/frontier" {
		t.Fatalf("block mode served by %s (%d)", r.Header.Get("X-Gw-Served-Model"), r.Status)
	}
	if r := env.Chat(key3, "cloud/frontier", hard, nil); r.Status != 403 || !strings.Contains(string(r.Body), "policy_blocked") {
		t.Fatalf("block with only a cloud candidate: %d %s", r.Status, r.Body)
	}
}

// Covers: GRD-01
func TestGRD01_RequestCaps(t *testing.T) {
	env := m6Env(t)
	many := []map[string]any{}
	for i := 0; i < 5; i++ {
		many = append(many, map[string]any{"role": "user", "content": "hi"})
	}
	if r := env.Post("/v1/chat/completions", key3, map[string]any{"model": "sima/qwen-4b", "messages": many}); r.Status != 400 {
		t.Fatalf("message cap: %d", r.Status)
	}
	// Output cap clamps max_tokens.
	r := env.Chat(key3, "sima/qwen-4b", strings.Repeat("word ", 100), map[string]any{"max_tokens": 1000})
	var resp struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	_ = r.JSON(&resp)
	if resp.Choices[0].FinishReason != "length" {
		t.Fatalf("output cap not applied: %s", r.Body)
	}
	// Byte cap.
	big := testenv.New(t, testenv.Options{Sims: map[string][]sim.Model{"sima": testenv.StandardModels, "simb": testenv.StandardModels},
		YAML: strings.Replace(m3YAML, "server:\n", "server:\n  max_body_bytes: 2000\n", 1) + testenv.DeploymentsFor("simb"),
		Env:  map[string]string{"TEST_KEY_1": key1, "TEST_KEY_2": key2}})
	if r := big.Chat(key1, "sima/qwen-4b", strings.Repeat("x", 3000), nil); r.Status != 400 {
		t.Fatalf("body cap: %d", r.Status)
	}
}

// Covers: GRD-03, NFR-12
func TestGRD03_BodyLoggingOptInEncryptedWithRetention(t *testing.T) {
	env := m6Env(t)
	r := env.Chat(key3, "sima/qwen-4b", "my email is asha.k@example.in", nil)
	env.Chat(key1, "sima/qwen-4b", "acme does not log bodies", nil)
	if docs, _ := env.Store.ListDocs(context.Background(), "body", "acme"); len(docs) != 0 {
		t.Fatal("body stored for a tenant without opt-in")
	}
	docs, _ := env.Store.ListDocs(context.Background(), "body", "blockco")
	if len(docs) != 1 || strings.Contains(string(docs[0].Body), "example.in") || strings.Contains(string(docs[0].Body), "my email") {
		t.Fatalf("stored body not encrypted: %v", docs)
	}
	plain, err := env.Engine.OpenBody(context.Background(), r.Header.Get("X-Request-Id"))
	if err != nil || strings.Contains(string(plain), "asha.k@example.in") || !strings.Contains(string(plain), "REDACTED_EMAIL") {
		t.Fatalf("decrypted body: %v %s", err, plain)
	}
	if n := env.Engine.PurgeBodies(context.Background()); n != 0 {
		t.Fatal("purged a body inside retention")
	}
	// Body logging without a key is a configuration error.
	cfgErr := strings.Replace(m6YAML, "storage:\n  body_key: env:BODY_KEY\n", "", 1)
	if _, err := testenvParse(cfgErr); err == nil || !strings.Contains(err.Error(), "body_key") {
		t.Fatalf("missing body key accepted: %v", err)
	}
}

// Covers: GRD-05
func TestGRD05_PreAndPostWebhooks(t *testing.T) {
	var mu sync.Mutex
	var stages []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		stages = append(stages, p["stage"].(string))
		mu.Unlock()
		allow := !strings.Contains(string(mustJSON(p)), "forbidden-topic")
		_ = json.NewEncoder(w).Encode(map[string]any{"allow": allow, "reason": "topic not allowed"})
	}))
	defer hook.Close()
	env := m6Env(t, func(y string) string {
		return strings.Replace(y, "guardrails:\n", "guardrails:\n  pre_webhook: "+hook.URL+"\n  post_webhook: "+hook.URL+"\n  webhook_allow: [127.0.0.1]\n", 1)
	})
	if r := env.Chat(key1, "sima/qwen-4b", "a normal question", nil); r.Status != 200 {
		t.Fatalf("allowed: %d %s", r.Status, r.Body)
	}
	if r := env.Chat(key1, "sima/qwen-4b", "tell me about forbidden-topic", nil); r.Status != 403 || !strings.Contains(string(r.Body), "topic not allowed") {
		t.Fatalf("pre-webhook block: %d %s", r.Status, r.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(stages, ",") != "pre,post,pre" {
		t.Fatalf("webhook stages %v", stages)
	}
}

// Scenario 17 (emulated): the request pattern Claude Code uses — aliased Claude model
// name, system blocks with cache_control, tools, streaming, tool_result loop, metadata
// user id — works on local models and spend is attributed to its key.
// Covers: API-02, API-07, NFR-20
func TestScenario17_ClaudeCodePatternThroughGateway(t *testing.T) {
	env := m6Env(t)
	tools := []map[string]any{
		{"name": "Read", "description": "Read a file", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}, "required": []string{"file_path"}}},
		{"name": "Bash", "description": "Run a command", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}, "required": []string{"command"}}},
	}
	system := []map[string]any{{"type": "text", "text": "You are Claude Code.", "cache_control": map[string]any{"type": "ephemeral"}}}
	convo := []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "Read main.go and fix the bug"}}}}
	// Turn 0: the model calls a tool. Turn 1: after the tool result it answers in text,
	// which ends Claude Code's loop. Turn 2: the next user instruction starts a new call.
	for turn := 0; turn < 3; turn++ {
		r := messages(env, key1, map[string]any{"model": "claude-sonnet-4-5", "max_tokens": 1024, "stream": true, "system": system,
			"tools": tools, "messages": convo, "metadata": map[string]any{"user_id": "dev-42"}})
		if r.Status != 200 {
			t.Fatalf("turn %d: %d %s", turn, r.Status, r.Body)
		}
		evs := parseAnthropicSSE(t, r.Body)
		var toolID, toolName, args string
		stop := ""
		for _, e := range evs {
			switch e.Name {
			case "content_block_start":
				if b := e.Data["content_block"].(map[string]any); b["type"] == "tool_use" {
					toolID, toolName = b["id"].(string), b["name"].(string)
				}
			case "content_block_delta":
				if d := e.Data["delta"].(map[string]any); d["type"] == "input_json_delta" {
					args += d["partial_json"].(string)
				}
			case "message_delta":
				stop = e.Data["delta"].(map[string]any)["stop_reason"].(string)
			}
		}
		if turn == 1 {
			if stop != "end_turn" || anthropicText(evs) == "" {
				t.Fatalf("turn 1: expected a text answer after the tool result, got stop=%q", stop)
			}
			convo = append(convo, map[string]any{"role": "assistant", "content": anthropicText(evs)},
				map[string]any{"role": "user", "content": "Now run the tests"})
			continue
		}
		if stop != "tool_use" || toolID == "" {
			t.Fatalf("turn %d: expected a tool call, got stop=%q", turn, stop)
		}
		var input map[string]any
		if json.Unmarshal([]byte(args), &input) != nil {
			t.Fatalf("turn %d: tool input %q", turn, args)
		}
		convo = append(convo,
			map[string]any{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": toolID, "name": toolName, "input": input}}},
			map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": toolID, "content": "package main\nfunc main() {}"}}})
	}
	env.Flush()
	recs, _ := env.Store.ListCostRecords(context.Background(), store.CostFilter{KeyID: "k1", EndUser: "dev-42"}, 0)
	var spend float64
	for _, r := range recs {
		spend += r.CostUSD
		if !env.Engine.Snapshot().Catalog.Deployments[r.Deployment].IsLocal() && r.Deployment != "cloud/frontier" {
			t.Errorf("served by %s", r.Deployment)
		}
	}
	if len(recs) != 3 || spend <= 0 {
		t.Fatalf("spend attribution: %d records $%v", len(recs), spend)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
