// Package testenv wires simulated providers, configuration, engine and HTTP server
// in-process for tests and scenario runs.
package testenv

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/gateway"
	"github.com/SureshKhemka/llm-gateway/internal/observe"
	"github.com/SureshKhemka/llm-gateway/internal/server"
	"github.com/SureshKhemka/llm-gateway/internal/sim"
	"github.com/SureshKhemka/llm-gateway/internal/store"
)

// Standard simulated lineup mirroring spec §8.2.
var StandardModels = []sim.Model{
	{ID: "gemma-e2b", Tier: 0, ContextWindow: 8192},
	{ID: "qwen-4b", Tier: 1, ContextWindow: 16384},
	{ID: "qwen-9b", Tier: 2, ContextWindow: 32768},
	{ID: "qwen-27b", Tier: 3, ContextWindow: 131072},
	{ID: "qwen-vl-8b", Tier: 2, ContextWindow: 32768},
	{ID: "nomic-embed", Tier: 0, Embeddings: true},
}

// DeploymentsFor returns StandardDeployments rewritten for another provider ID.
func DeploymentsFor(provider string) string {
	body := strings.TrimPrefix(strings.TrimSpace(StandardDeployments), "deployments:")
	return strings.ReplaceAll(body, "sima", provider)
}

// StandardDeployments is the deployment section matching StandardModels on provider
// "sima"; tier prices mimic real-world ratios (ENV-03).
const StandardDeployments = `
deployments:
  - id: sima/gemma-e2b
    provider: sima
    model: gemma-e2b
    tier: 0
    capabilities: [chat]
    context_window: 8192
    residency: [local]
    latency: {ttft_ms: 50, tokens_per_sec: 200}
    prices: [{effective_from: 2026-01-01T00:00:00Z, input: 0.05, output: 0.20}]
  - id: sima/qwen-4b
    provider: sima
    model: qwen-4b
    tier: 1
    capabilities: [chat, tools, json_schema]
    context_window: 16384
    residency: [local]
    latency: {ttft_ms: 80, tokens_per_sec: 120}
    prices: [{effective_from: 2026-01-01T00:00:00Z, input: 0.15, output: 0.60}]
  - id: sima/qwen-9b
    provider: sima
    model: qwen-9b
    tier: 2
    capabilities: [chat, tools, json_schema, reasoning]
    context_window: 32768
    residency: [local]
    latency: {ttft_ms: 150, tokens_per_sec: 70}
    prices: [{effective_from: 2026-01-01T00:00:00Z, input: 0.50, output: 2.00}]
  - id: sima/qwen-27b
    provider: sima
    model: qwen-27b
    tier: 3
    capabilities: [chat, tools, json_schema, reasoning]
    context_window: 131072
    residency: [local]
    latency: {ttft_ms: 400, tokens_per_sec: 30}
    prices: [{effective_from: 2026-01-01T00:00:00Z, input: 3.00, output: 15.00}]
  - id: sima/qwen-vl-8b
    provider: sima
    model: qwen-vl-8b
    tier: 2
    capabilities: [chat, vision]
    context_window: 32768
    residency: [local]
    prices: [{effective_from: 2026-01-01T00:00:00Z, input: 0.50, output: 2.00}]
  - id: sima/nomic-embed
    provider: sima
    model: nomic-embed
    tier: 0
    capabilities: [embeddings]
    residency: [local]
    prices: [{effective_from: 2026-01-01T00:00:00Z, input: 0.02, output: 0}]
`

type Env struct {
	T       testing.TB
	Sims    map[string]*sim.Instance
	simSrv  map[string]*httptest.Server
	Config  *config.Config
	Engine  *gateway.Engine
	Server  *server.Server
	HTTP    *httptest.Server
	Events  *observe.Recorder
	Store   store.Store
	cleanup []func()
}

// Options configure an environment. YAML may reference ${SIM_<NAME>_URL} for each sim.
type Options struct {
	Sims  map[string][]sim.Model
	YAML  string
	Env   map[string]string
	Store store.Store
	// Setup runs after the engine is built but before the HTTP server starts.
	Setup func(*Env)
}

// New starts sims, loads YAML config and starts the gateway.
func New(t testing.TB, o Options) *Env {
	t.Helper()
	env := &Env{T: t, Sims: map[string]*sim.Instance{}, simSrv: map[string]*httptest.Server{}, Events: observe.NewRecorder(10000)}
	if o.Sims == nil {
		o.Sims = map[string][]sim.Model{"sima": StandardModels}
	}
	for name, models := range o.Sims {
		in := sim.New(name, models...)
		srv := httptest.NewServer(in.Handler())
		env.Sims[name] = in
		env.simSrv[name] = srv
		t.Setenv("SIM_"+strings.ToUpper(name)+"_URL", srv.URL+"/v1")
		env.cleanup = append(env.cleanup, srv.Close)
	}
	for k, v := range o.Env {
		t.Setenv(k, v)
	}
	cfg, err := config.Parse("test.yaml", []byte(o.YAML))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	env.Config = cfg
	snap, err := gateway.BuildSnapshot(gateway.BuildInput{Config: cfg})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	env.Store = o.Store
	if env.Store == nil {
		env.Store = store.NewMemory()
	}
	env.Engine = gateway.NewEngine(snap, gateway.EngineOptions{Store: env.Store, Events: env.Events})
	env.Server = server.New(env.Engine, nil)
	if o.Setup != nil {
		o.Setup(env)
	}
	env.HTTP = httptest.NewServer(env.Server.Handler())
	t.Cleanup(env.Close)
	return env
}

func (e *Env) Close() {
	e.HTTP.Close()
	e.Engine.Close()
	for _, c := range e.cleanup {
		c()
	}
}

// SimURL returns the base URL (without /v1) of a sim instance.
func (e *Env) SimURL(name string) string { return e.simSrv[name].URL }

// KillSim closes a sim's listener and connections so requests are refused.
func (e *Env) KillSim(name string) {
	e.simSrv[name].CloseClientConnections()
	e.simSrv[name].Listener.Close()
}

// RestartSim brings a killed sim back on the same address.
func (e *Env) RestartSim(name string) {
	e.T.Helper()
	addr := e.simSrv[name].Listener.Addr().String()
	var l net.Listener
	var err error
	for i := 0; i < 50; i++ {
		if l, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		e.T.Fatalf("restart %s: %v", name, err)
	}
	srv := httptest.NewUnstartedServer(e.Sims[name].Handler())
	srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	e.simSrv[name] = srv
	e.cleanup = append(e.cleanup, srv.Close)
}

// Response is a decoded HTTP response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
	// Trailer holds HTTP trailers (streaming responses).
	Trailer http.Header
}

func (r *Response) JSON(v any) error { return json.Unmarshal(r.Body, v) }

func (r *Response) Map() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(r.Body, &m)
	return m
}

// Do sends a request to the gateway.
func (e *Env) Do(method, path, key string, body any, headers map[string]string) *Response {
	e.T.Helper()
	var rdr io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rdr = strings.NewReader(b)
		case []byte:
			rdr = bytes.NewReader(b)
		default:
			raw, _ := json.Marshal(b)
			rdr = bytes.NewReader(raw)
		}
	}
	req, _ := http.NewRequest(method, e.HTTP.URL+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.T.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: b, Trailer: resp.Trailer}
}

func (e *Env) Post(path, key string, body any, headers ...map[string]string) *Response {
	var h map[string]string
	if len(headers) > 0 {
		h = headers[0]
	}
	return e.Do(http.MethodPost, path, key, body, h)
}

// Chat sends a single-user-message chat request.
func (e *Env) Chat(key, model, prompt string, extra map[string]any, headers ...map[string]string) *Response {
	body := map[string]any{"model": model, "messages": []map[string]any{{"role": "user", "content": prompt}}}
	for k, v := range extra {
		body[k] = v
	}
	return e.Post("/v1/chat/completions", key, body, headers...)
}

// SSE parses "data:" lines of an SSE body.
func SSE(body []byte) (events []json.RawMessage, done bool) {
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		d := strings.TrimPrefix(line, "data: ")
		if d == "[DONE]" {
			done = true
			continue
		}
		events = append(events, json.RawMessage(d))
	}
	return events, done
}

// StreamText concatenates delta content from OpenAI stream chunks.
func StreamText(events []json.RawMessage) string {
	var sb strings.Builder
	for _, ev := range events {
		var ch struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(ev, &ch) == nil {
			for _, c := range ch.Choices {
				sb.WriteString(c.Delta.Content)
			}
		}
	}
	return sb.String()
}

// Flush waits for async record writes.
func (e *Env) Flush() { e.Engine.Writer.Flush() }

// MustSetenv sets an environment variable for the process (scenario runners).
func MustSetenv(k, v string) {
	if err := os.Setenv(k, v); err != nil {
		panic(fmt.Sprint(err))
	}
}
