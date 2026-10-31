package sim

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/tokens"
	"github.com/SureshKhemka/llm-gateway/internal/translate"
)

func anthropicError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
}

// anthropicWriter adapts preflight's OpenAI-shaped error replies to Anthropic shape.
type anthropicWriter struct {
	http.ResponseWriter
	status int
}

func (a *anthropicWriter) WriteHeader(s int) { a.status = s; a.ResponseWriter.WriteHeader(s) }

func (a *anthropicWriter) Write(b []byte) (int, error) {
	if a.status >= 400 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil {
			typ := "api_error"
			switch a.status {
			case 429:
				typ = "rate_limit_error"
			case 529, 503:
				typ = "overloaded_error"
			}
			out, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": e.Error.Message}})
			return a.ResponseWriter.Write(out)
		}
	}
	return a.ResponseWriter.Write(b)
}

// handleMessages serves the Anthropic Messages format, sharing answers and faults with
// the OpenAI endpoint [ENV-04].
func (in *Instance) handleMessages(w http.ResponseWriter, r *http.Request) {
	var ar api.AnthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&ar); err != nil {
		anthropicError(w, 400, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	if ar.MaxTokens <= 0 {
		anthropicError(w, 400, "invalid_request_error", "max_tokens: field required")
		return
	}
	m := in.model(ar.Model)
	if m == nil {
		anthropicError(w, 404, "not_found_error", fmt.Sprintf("model: %s", ar.Model))
		return
	}
	req, err := translate.AnthropicToCanonical(&ar)
	if err != nil {
		anthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	f := in.faultsFor(ar.Model)
	if !in.preflight(&anthropicWriter{ResponseWriter: w}, r, ar.Model, f) {
		return
	}
	a := in.answerFor(m, req, f)
	id := fmt.Sprintf("msg_sim_%s_%d", in.Name, in.seq.Add(1))
	usage := usageFor(req, a)
	if ar.Stream {
		in.streamMessages(w, r, m, a, f, id, usage)
		return
	}
	msg := api.Message{Role: "assistant", Content: api.TextContent(a.Text), ToolCalls: a.ToolCalls}
	resp := translate.ResponseToAnthropic(&api.ChatResponse{ID: id, Choices: []api.Choice{{Message: msg, FinishReason: a.Finish}}, Usage: usage}, m.ID)
	resp.ID = id
	if a.Refusal {
		resp.StopReason = "refusal"
	}
	if f.OmitUsage {
		resp.Usage = api.AnthropicUsage{}
	}
	in.TokensSent.Add(int64(tokens.Completion(a.Text, a.ToolCalls)))
	writeJSON(w, 200, resp)
}

func (in *Instance) streamMessages(w http.ResponseWriter, r *http.Request, m *Model, a answer, f Faults, id string, usage *api.Usage) {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	send := func(name string, v any) bool {
		b, _ := json.Marshal(v)
		if _, err := fmt.Fprint(w, translate.SSE(name, b)); err != nil {
			return false
		}
		if fl != nil {
			fl.Flush()
		}
		return r.Context().Err() == nil
	}
	in_ := usage.PromptTokens
	if f.OmitUsage {
		in_ = 0
	}
	if !send("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": id, "type": "message", "role": "assistant",
		"model": m.ID, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": in_, "output_tokens": 1}}}) {
		return
	}
	delay := time.Duration(0)
	if f.TokensPerSec > 0 {
		delay = time.Duration(float64(time.Second) / f.TokensPerSec)
	}
	idx, sent := 0, 0
	if a.Text != "" || len(a.ToolCalls) == 0 {
		send("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "text", "text": ""}})
		for i, word := range strings.Fields(a.Text) {
			if f.DropAfterTokens > 0 && sent >= f.DropAfterTokens {
				panic(http.ErrAbortHandler)
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
			if !send("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "text_delta", "text": tok}}) {
				return
			}
			sent++
			in.TokensSent.Add(1)
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		idx++
	}
	for _, tc := range a.ToolCalls {
		send("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": map[string]any{}}})
		args := tc.Function.Arguments
		for len(args) > 0 {
			n := min(8, len(args))
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": args[:n]}})
			args = args[n:]
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		idx++
	}
	stop := translate.StopToAnthropic(a.Finish)
	if a.Refusal {
		stop = "refusal"
	}
	out := usage.CompletionTokens
	if f.OmitUsage {
		out = 0
	}
	send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": out}})
	send("message_stop", map[string]any{"type": "message_stop"})
}
