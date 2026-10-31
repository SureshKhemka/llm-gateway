package sim

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/tokens"
)

func (in *Instance) handleChat(w http.ResponseWriter, r *http.Request) {
	var req api.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		openAIError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	m := in.model(req.Model)
	if m == nil {
		openAIError(w, 404, fmt.Sprintf("model %q not found", req.Model))
		return
	}
	f := in.faultsFor(req.Model)
	if !in.preflight(w, r, req.Model, f) {
		return
	}
	if pt := tokens.Prompt(&req); pt > m.ContextWindow {
		openAIError(w, 400, fmt.Sprintf("This model's maximum context length is %d tokens, but the prompt has %d", m.ContextWindow, pt))
		return
	}
	a := in.answerFor(m, &req, f)
	id := fmt.Sprintf("chatcmpl-sim-%s-%d", in.Name, in.seq.Add(1))
	if req.Stream {
		in.streamChat(w, r, &req, m, a, f, id)
		return
	}
	msg := api.Message{Role: "assistant", Content: api.TextContent(a.Text), ToolCalls: a.ToolCalls}
	if len(a.ToolCalls) > 0 && a.Text == "" {
		msg.Content = api.Content{Null: true}
	}
	resp := api.ChatResponse{ID: id, Object: "chat.completion", Created: time.Now().Unix(), Model: m.ID,
		Choices: []api.Choice{{Message: msg, FinishReason: a.Finish}}}
	if !f.OmitUsage {
		resp.Usage = usageFor(&req, a)
	}
	in.TokensSent.Add(int64(tokens.Completion(a.Text, a.ToolCalls)))
	writeJSON(w, 200, resp)
}

func (in *Instance) streamChat(w http.ResponseWriter, r *http.Request, req *api.ChatRequest, m *Model, a answer, f Faults, id string) {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	created := time.Now().Unix()
	send := func(ch api.ChatChunk) bool {
		ch.ID, ch.Object, ch.Created, ch.Model = id, "chat.completion.chunk", created, m.ID
		b, _ := json.Marshal(ch)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		if fl != nil {
			fl.Flush()
		}
		return r.Context().Err() == nil
	}
	delay := time.Duration(0)
	if f.TokensPerSec > 0 {
		delay = time.Duration(float64(time.Second) / f.TokensPerSec)
	}
	empty := ""
	if !send(api.ChatChunk{Choices: []api.ChunkChoice{{Delta: api.Delta{Role: "assistant", Content: &empty}}}}) {
		return
	}
	sent := 0
	for i, tc := range a.ToolCalls {
		idx := i
		if !send(api.ChatChunk{Choices: []api.ChunkChoice{{Delta: api.Delta{ToolCalls: []api.ToolCall{{Index: &idx, ID: tc.ID, Type: "function", Function: api.FunctionCall{Name: tc.Function.Name}}}}}}}) {
			return
		}
		args := tc.Function.Arguments
		for len(args) > 0 {
			n := min(8, len(args))
			if !send(api.ChatChunk{Choices: []api.ChunkChoice{{Delta: api.Delta{ToolCalls: []api.ToolCall{{Index: &idx, Function: api.FunctionCall{Arguments: args[:n]}}}}}}}) {
				return
			}
			args = args[n:]
			sent++
		}
	}
	for i, word := range strings.Fields(a.Text) {
		if f.DropAfterTokens > 0 && sent >= f.DropAfterTokens {
			panic(http.ErrAbortHandler) // abrupt disconnect mid-stream
		}
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		tok := word
		if i > 0 {
			tok = " " + word
		}
		if !send(api.ChatChunk{Choices: []api.ChunkChoice{{Delta: api.Delta{Content: &tok}}}}) {
			return
		}
		sent++
		in.TokensSent.Add(1)
	}
	finish := a.Finish
	if !send(api.ChatChunk{Choices: []api.ChunkChoice{{Delta: api.Delta{}, FinishReason: &finish}}}) {
		return
	}
	if !f.OmitUsage && req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		send(api.ChatChunk{Choices: []api.ChunkChoice{}, Usage: usageFor(req, a)})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

func (in *Instance) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	var req api.EmbeddingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		openAIError(w, 400, "invalid JSON")
		return
	}
	m := in.model(req.Model)
	if m == nil {
		openAIError(w, 404, fmt.Sprintf("model %q not found", req.Model))
		return
	}
	f := in.faultsFor(req.Model)
	if !in.preflight(w, r, req.Model, f) {
		return
	}
	inputs, err := req.Inputs()
	if err != nil {
		openAIError(w, 400, "input must be a string or array of strings")
		return
	}
	resp := api.EmbeddingsResponse{Object: "list", Model: m.ID}
	total := 0
	for i, s := range inputs {
		resp.Data = append(resp.Data, api.Embedding{Object: "embedding", Index: i, Embedding: Embed(s, 256)})
		total += tokens.Count(s)
	}
	if !f.OmitUsage {
		resp.Usage = &api.Usage{PromptTokens: total, TotalTokens: total}
	}
	writeJSON(w, 200, resp)
}

var stopwords = map[string]bool{"a": true, "an": true, "the": true, "is": true, "are": true, "of": true, "to": true,
	"please": true, "can": true, "you": true, "could": true, "would": true, "me": true, "i": true, "my": true, "for": true,
	"and": true, "in": true, "on": true, "what": true, "tell": true, "kindly": true, "do": true, "it": true, "this": true,
	"that": true, "with": true, "be": true, "hey": true, "hi": true, "just": true}

// Embed is a deterministic bag-of-words hashing embedding: paraphrases that share
// content words score a high cosine similarity, unrelated prompts score low.
func Embed(s string, dim int) []float64 {
	v := make([]float64, dim)
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for _, w := range words {
		if stopwords[w] {
			continue
		}
		w = stem(w)
		h := sha256.Sum256([]byte(w))
		for k := 0; k < 4; k++ {
			idx := binary.LittleEndian.Uint32(h[k*4:]) % uint32(dim)
			sign := 1.0
			if h[16+k]&1 == 1 {
				sign = -1
			}
			v[idx] += sign
		}
	}
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	if norm == 0 {
		v[0] = 1
		return v
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] /= norm
	}
	return v
}

func stem(w string) string {
	for _, suf := range []string{"ing", "ed", "es", "s"} {
		if len(w) > len(suf)+2 && strings.HasSuffix(w, suf) {
			return strings.TrimSuffix(w, suf)
		}
	}
	return w
}
