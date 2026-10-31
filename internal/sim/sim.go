// Package sim is the fault-injecting simulated provider used by tests, demos and load
// tests [ENV-04]. Answers are deterministic functions of the prompt and the model's tier;
// faults are set at runtime through /_sim/faults.
package sim

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/tokens"
)

type Model struct {
	ID            string `json:"id"`
	Tier          int    `json:"tier"`
	Embeddings    bool   `json:"embeddings,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
}

// Faults are injected per instance (Model == "") or per model.
type Faults struct {
	LatencyMs       int     `json:"latency_ms,omitempty"`        // added before the first byte
	TokensPerSec    float64 `json:"tokens_per_sec,omitempty"`    // slow token rate when streaming
	ErrorRate       float64 `json:"error_rate,omitempty"`        // share of requests failing with ErrorStatus
	ErrorStatus     int     `json:"error_status,omitempty"`      // default 500
	RateLimit       bool    `json:"rate_limit,omitempty"`        // every request gets 429
	RetryAfterSec   int     `json:"retry_after_sec,omitempty"`   // Retry-After on 429s
	Timeout         bool    `json:"timeout,omitempty"`           // hang until the client gives up
	DropAfterTokens int     `json:"drop_after_tokens,omitempty"` // cut the stream after N tokens
	MalformedJSON   bool    `json:"malformed_json,omitempty"`    // JSON-mode answers are invalid
	OmitUsage       bool    `json:"omit_usage,omitempty"`        // responses carry no usage
	Down            bool    `json:"down,omitempty"`              // refuse with 503 (connection-level kill uses Instance.Kill)
	Refuse          bool    `json:"refuse,omitempty"`            // answer with a refusal
	EmptyAnswer     bool    `json:"empty_answer,omitempty"`
	FailNext        int     `json:"fail_next,omitempty"` // the next N requests fail with ErrorStatus (429 adds Retry-After)
}

func (f Faults) merge(o Faults) Faults {
	b, _ := json.Marshal(o)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	cur, _ := json.Marshal(f)
	var cm map[string]json.RawMessage
	_ = json.Unmarshal(cur, &cm)
	if cm == nil {
		cm = map[string]json.RawMessage{}
	}
	for k, v := range m {
		cm[k] = v
	}
	out, _ := json.Marshal(cm)
	var res Faults
	_ = json.Unmarshal(out, &res)
	return res
}

// OracleItem tells the simulator the correct answer to a known prompt.
type OracleItem struct {
	Reference  string `json:"reference"`
	Difficulty int    `json:"difficulty"` // tier needed to answer reliably
}

type Instance struct {
	Name string

	mu     sync.RWMutex
	models map[string]*Model
	faults map[string]Faults
	oracle map[string]OracleItem
	seq    atomic.Uint64

	Requests   atomic.Int64
	perModel   sync.Map // model -> *atomic.Int64
	failNext   sync.Map // model -> *atomic.Int64 remaining forced failures
	TokensSent atomic.Int64
}

func New(name string, models ...Model) *Instance {
	in := &Instance{Name: name, models: map[string]*Model{}, faults: map[string]Faults{}, oracle: map[string]OracleItem{}}
	for _, m := range models {
		m := m
		if m.ContextWindow == 0 {
			m.ContextWindow = 32768
		}
		in.models[m.ID] = &m
	}
	return in
}

// SetFaults replaces the faults for a model ("" = whole instance).
func (in *Instance) SetFaults(model string, f Faults) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.faults[model] = f
	if f.FailNext > 0 {
		n := &atomic.Int64{}
		n.Store(int64(f.FailNext))
		in.failNext.Store(model, n)
	}
}

func (in *Instance) ClearFaults() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.faults = map[string]Faults{}
	in.failNext.Range(func(k, _ any) bool { in.failNext.Delete(k); return true })
}

func (in *Instance) AddModel(m Model) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if m.ContextWindow == 0 {
		m.ContextWindow = 32768
	}
	in.models[m.ID] = &m
}

// RemoveModel simulates unloading a model (requests for it get 404 like LM Studio).
func (in *Instance) RemoveModel(id string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	delete(in.models, id)
}

// LoadOracle registers prompt → answer pairs.
func (in *Instance) LoadOracle(items map[string]OracleItem) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for prompt, it := range items {
		in.oracle[OracleKey(prompt)] = it
	}
}

func (in *Instance) ModelCalls(id string) int64 {
	if v, ok := in.perModel.Load(id); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}

func (in *Instance) countModel(id string) {
	v, _ := in.perModel.LoadOrStore(id, &atomic.Int64{})
	v.(*atomic.Int64).Add(1)
}

func (in *Instance) faultsFor(model string) Faults {
	in.mu.RLock()
	defer in.mu.RUnlock()
	f := in.faults[""]
	if mf, ok := in.faults[model]; ok {
		f = f.merge(mf)
	}
	return f
}

func (in *Instance) model(id string) *Model {
	in.mu.RLock()
	defer in.mu.RUnlock()
	return in.models[id]
}

// OracleKey normalises a prompt for oracle lookup.
func OracleKey(prompt string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(strings.ToLower(prompt)), " ")))
	return fmt.Sprintf("%x", sum[:12])
}

// Handler serves the OpenAI-format API, the Anthropic Messages API and control routes.
func (in *Instance) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", in.handleChat)
	mux.HandleFunc("POST /v1/embeddings", in.handleEmbeddings)
	mux.HandleFunc("GET /v1/models", in.handleModels)
	mux.HandleFunc("POST /v1/messages", in.handleMessages)
	mux.HandleFunc("GET /_sim/faults", in.handleGetFaults)
	mux.HandleFunc("POST /_sim/faults", in.handleSetFaults)
	mux.HandleFunc("DELETE /_sim/faults", func(w http.ResponseWriter, r *http.Request) { in.ClearFaults(); w.WriteHeader(204) })
	mux.HandleFunc("POST /_sim/models", func(w http.ResponseWriter, r *http.Request) {
		var m Model
		if json.NewDecoder(r.Body).Decode(&m) != nil || m.ID == "" {
			http.Error(w, "bad model", 400)
			return
		}
		in.AddModel(m)
		w.WriteHeader(204)
	})
	mux.HandleFunc("DELETE /_sim/models/{id...}", func(w http.ResponseWriter, r *http.Request) {
		in.RemoveModel(r.PathValue("id"))
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /_sim/oracle", func(w http.ResponseWriter, r *http.Request) {
		var items map[string]OracleItem
		if json.NewDecoder(r.Body).Decode(&items) != nil {
			http.Error(w, "bad oracle", 400)
			return
		}
		in.LoadOracle(items)
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /_sim/stats", func(w http.ResponseWriter, r *http.Request) {
		stats := map[string]any{"requests": in.Requests.Load(), "tokens_sent": in.TokensSent.Load()}
		per := map[string]int64{}
		in.perModel.Range(func(k, v any) bool { per[k.(string)] = v.(*atomic.Int64).Load(); return true })
		stats["per_model"] = per
		writeJSON(w, 200, stats)
	})
	return mux
}

func (in *Instance) handleGetFaults(w http.ResponseWriter, _ *http.Request) {
	in.mu.RLock()
	defer in.mu.RUnlock()
	writeJSON(w, 200, in.faults)
}

// POST /_sim/faults {"model": "x", "faults": {...}, "merge": true}
func (in *Instance) handleSetFaults(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model  string `json:"model"`
		Faults Faults `json:"faults"`
		Merge  bool   `json:"merge"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if body.Merge {
		body.Faults = in.faultsFor(body.Model).merge(body.Faults)
	}
	in.SetFaults(body.Model, body.Faults)
	w.WriteHeader(204)
}

func (in *Instance) handleModels(w http.ResponseWriter, _ *http.Request) {
	in.mu.RLock()
	ids := make([]string, 0, len(in.models))
	for id := range in.models {
		ids = append(ids, id)
	}
	in.mu.RUnlock()
	sort.Strings(ids)
	list := api.ModelList{Object: "list"}
	for _, id := range ids {
		list.Data = append(list.Data, api.ModelObject{ID: id, Object: "model", OwnedBy: in.Name})
	}
	writeJSON(w, 200, list)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func openAIError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": "sim_error", "code": status}})
}

// preflight applies latency, errors, rate limits, timeouts. It returns false if the
// request has been answered (or abandoned).
func (in *Instance) preflight(w http.ResponseWriter, r *http.Request, model string, f Faults) bool {
	in.Requests.Add(1)
	in.countModel(model)
	if f.Down {
		openAIError(w, 503, "instance down")
		return false
	}
	if f.LatencyMs > 0 {
		select {
		case <-time.After(time.Duration(f.LatencyMs) * time.Millisecond):
		case <-r.Context().Done():
			return false
		}
	}
	if f.Timeout {
		<-r.Context().Done()
		return false
	}
	if f.RateLimit {
		ra := f.RetryAfterSec
		if ra == 0 {
			ra = 1
		}
		w.Header().Set("Retry-After", fmt.Sprint(ra))
		openAIError(w, 429, "rate limited by simulator")
		return false
	}
	for _, k := range []string{model, ""} {
		if v, ok := in.failNext.Load(k); ok && v.(*atomic.Int64).Add(-1) >= 0 {
			st := f.ErrorStatus
			if st == 0 {
				st = 500
			}
			if st == 429 {
				ra := f.RetryAfterSec
				if ra == 0 {
					ra = 1
				}
				w.Header().Set("Retry-After", fmt.Sprint(ra))
			}
			openAIError(w, st, "injected failure (fail_next)")
			return false
		}
	}
	if f.ErrorRate > 0 && in.roll() < f.ErrorRate {
		st := f.ErrorStatus
		if st == 0 {
			st = 500
		}
		openAIError(w, st, "injected error")
		return false
	}
	return true
}

// roll returns a deterministic pseudo-random number in [0,1).
func (in *Instance) roll() float64 {
	n := in.seq.Add(1)
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], n)
	h := sha256.Sum256(append([]byte(in.Name), b[:]...))
	return float64(binary.LittleEndian.Uint64(h[:8])>>11) / float64(1<<53)
}

func lastUser(msgs []api.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content.PlainText()
		}
	}
	return ""
}

func lastIsToolResult(msgs []api.Message) bool {
	return len(msgs) > 0 && msgs[len(msgs)-1].Role == "tool"
}

// answer is the simulator's reply to a canonical request.
type answer struct {
	Text      string
	ToolCalls []api.ToolCall
	Finish    string
	Refusal   bool
}

func systemText(msgs []api.Message) string {
	var parts []string
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			parts = append(parts, m.Content.PlainText())
		}
	}
	return strings.Join(parts, "\n")
}

// gatewayRole answers the gateway's own classifier and judge prompts: the classifier
// returns the oracle difficulty (or a length heuristic); the judge scores 9 when the
// response contains the oracle reference and 2 otherwise.
func (in *Instance) gatewayRole(req *api.ChatRequest) (string, bool) {
	sys := systemText(req.Messages)
	user := lastUser(req.Messages)
	switch {
	case strings.Contains(sys, "difficulty classifier"):
		in.mu.RLock()
		it, ok := in.oracle[OracleKey(user)]
		in.mu.RUnlock()
		if ok {
			return fmt.Sprint(it.Difficulty), true
		}
		n := len(strings.Fields(user))
		switch {
		case n < 8:
			return "0", true
		case n < 40:
			return "1", true
		case n < 150:
			return "2", true
		}
		return "3", true
	case strings.Contains(sys, "You are a grader"):
		reqText, respText, _ := strings.Cut(strings.TrimPrefix(user, "REQUEST:\n"), "\n\nRESPONSE:\n")
		in.mu.RLock()
		it, ok := in.oracle[OracleKey(reqText)]
		in.mu.RUnlock()
		if ok && strings.Contains(strings.ToLower(respText), strings.ToLower(it.Reference)) {
			return "9", true
		}
		if !ok && strings.TrimSpace(respText) != "" {
			return "7", true
		}
		return "2", true
	}
	return "", false
}

func (in *Instance) answerFor(m *Model, req *api.ChatRequest, f Faults) answer {
	if out, ok := in.gatewayRole(req); ok {
		return answer{Text: out, Finish: "stop"}
	}
	prompt := lastUser(req.Messages)
	if f.Refuse {
		return answer{Text: "I'm sorry, but I can't help with that request.", Finish: "stop", Refusal: true}
	}
	if f.EmptyAnswer {
		return answer{Text: "", Finish: "stop"}
	}
	if len(req.Tools) > 0 && !lastIsToolResult(req.Messages) && string(req.ToolChoice) != `"none"` {
		t := req.Tools[0].Function
		args := generateFromSchema(t.Parameters, prompt)
		b, _ := json.Marshal(args)
		return answer{ToolCalls: []api.ToolCall{{ID: fmt.Sprintf("call_%s_%d", in.Name, in.seq.Add(1)), Type: "function",
			Function: api.FunctionCall{Name: t.Name, Arguments: string(b)}}}, Finish: "tool_calls"}
	}
	text := in.textAnswer(m, prompt, req)
	if req.ResponseFormat != nil && (req.ResponseFormat.Type == "json_schema" || req.ResponseFormat.Type == "json_object") {
		if f.MalformedJSON {
			text = `{"answer": "` + firstWords(text, 5) + `", "confidence": `
		} else if req.ResponseFormat.Type == "json_schema" && req.ResponseFormat.JSONSchema != nil {
			b, _ := json.Marshal(generateFromSchema(req.ResponseFormat.JSONSchema.Schema, text))
			text = string(b)
		} else {
			b, _ := json.Marshal(map[string]string{"answer": text})
			text = string(b)
		}
	}
	finish := "stop"
	if max := req.MaxOutput(0); max > 0 {
		words := strings.Fields(text)
		if len(words) > max {
			text = strings.Join(words[:max], " ")
			finish = "length"
		}
	}
	return answer{Text: text, Finish: finish}
}

func (in *Instance) textAnswer(m *Model, prompt string, req *api.ChatRequest) string {
	in.mu.RLock()
	item, ok := in.oracle[OracleKey(prompt)]
	in.mu.RUnlock()
	if ok {
		if correct(m, prompt, item.Difficulty) {
			return item.Reference
		}
		return "I believe the answer is " + scramble(item.Reference)
	}
	if lastIsToolResult(req.Messages) {
		return "Based on the tool result: " + firstWords(req.Messages[len(req.Messages)-1].Content.PlainText(), 20)
	}
	if req.HasImages() {
		return fmt.Sprintf("[%s] The image shows a simulated scene.", m.ID)
	}
	return fmt.Sprintf("[%s] %s", m.ID, firstWords(prompt, 24))
}

// correct decides deterministically whether a model of the given tier answers an item
// of the given difficulty correctly: always at or above, with halving odds below.
func correct(m *Model, prompt string, difficulty int) bool {
	if m.Tier >= difficulty {
		return true
	}
	p := 0.6 * math.Pow(0.5, float64(difficulty-m.Tier-1))
	h := sha256.Sum256([]byte(m.ID + "|" + prompt))
	return float64(binary.LittleEndian.Uint64(h[:8])>>11)/float64(1<<53) < p
}

func scramble(s string) string {
	w := strings.Fields(s)
	for i, j := 0, len(w)-1; i < j; i, j = i+1, j-1 {
		w[i], w[j] = w[j], w[i]
	}
	return strings.Join(w, " ") + " (unsure)"
}

func firstWords(s string, n int) string {
	w := strings.Fields(s)
	if len(w) > n {
		w = w[:n]
	}
	return strings.Join(w, " ")
}

// generateFromSchema builds a JSON value that satisfies a (simple) JSON schema.
func generateFromSchema(raw json.RawMessage, hint string) any {
	var s map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return map[string]any{}
	}
	return genValue(s, hint, 0)
}

func genValue(s map[string]any, hint string, depth int) any {
	if enum, ok := s["enum"].([]any); ok && len(enum) > 0 {
		return enum[0]
	}
	if c, ok := s["const"]; ok {
		return c
	}
	typ, _ := s["type"].(string)
	if ts, ok := s["type"].([]any); ok && len(ts) > 0 {
		typ, _ = ts[0].(string)
	}
	if typ == "" {
		if _, ok := s["properties"]; ok {
			typ = "object"
		}
	}
	switch typ {
	case "object":
		out := map[string]any{}
		props, _ := s["properties"].(map[string]any)
		for name, p := range props {
			if ps, ok := p.(map[string]any); ok && depth < 6 {
				out[name] = genValue(ps, hint, depth+1)
			}
		}
		return out
	case "array":
		if items, ok := s["items"].(map[string]any); ok && depth < 6 {
			n := 1
			if mi, ok := s["minItems"].(float64); ok && int(mi) > n {
				n = int(mi)
			}
			arr := make([]any, n)
			for i := range arr {
				arr[i] = genValue(items, hint, depth+1)
			}
			return arr
		}
		return []any{}
	case "integer":
		if min, ok := s["minimum"].(float64); ok {
			return int(min)
		}
		return 1
	case "number":
		if min, ok := s["minimum"].(float64); ok {
			return min
		}
		return 1.5
	case "boolean":
		return true
	case "null":
		return nil
	default:
		if f, ok := s["format"].(string); ok && f == "email" {
			return "user@example.com"
		}
		if hint != "" {
			return firstWords(hint, 6)
		}
		return "sample"
	}
}

func usageFor(req *api.ChatRequest, a answer) *api.Usage {
	pt := tokens.Prompt(req)
	ct := tokens.Completion(a.Text, a.ToolCalls)
	return &api.Usage{PromptTokens: pt, CompletionTokens: ct, TotalTokens: pt + ct}
}
