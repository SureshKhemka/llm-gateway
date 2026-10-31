package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/gateway"
	"github.com/SureshKhemka/llm-gateway/internal/tokens"
	"github.com/SureshKhemka/llm-gateway/internal/translate"
)

func (s *Server) anthropicRoutes() {
	s.Mux.HandleFunc("POST /v1/messages", s.track(s.handleMessages))
	s.Mux.HandleFunc("POST /v1/messages/count_tokens", s.handleCountTokens)
}

// prepareMessages authenticates and parses an Anthropic Messages request.
func (s *Server) prepareMessages(w http.ResponseWriter, r *http.Request) (*gateway.RequestContext, *api.AnthropicRequest, *api.ChatRequest, *api.Error) {
	rc := s.newRC(r, "messages", api.FormatAnthropic)
	p, aerr := s.Authenticate(r, "messages")
	if aerr != nil {
		return rc, nil, nil, aerr
	}
	rc.Principal = p
	body, aerr := readBody(w, r, s.Engine.Snapshot().Config.Server.MaxBodyBytes)
	if aerr != nil {
		return rc, nil, nil, aerr
	}
	rc.BodyBytes = len(body)
	var ar api.AnthropicRequest
	if err := json.Unmarshal(body, &ar); err != nil {
		return rc, nil, nil, api.NewError(api.CodeInvalidRequest, "invalid JSON body: %v", err)
	}
	if ar.Model == "" || len(ar.Messages) == 0 {
		return rc, nil, nil, api.NewError(api.CodeInvalidRequest, "model and messages are required")
	}
	if ar.MaxTokens <= 0 {
		return rc, nil, nil, api.NewError(api.CodeInvalidRequest, "max_tokens: field required")
	}
	hints, herr := ParseHints(r.Header, ar.Metadata)
	if herr != nil {
		return rc, nil, nil, herr
	}
	if ar.Metadata != nil {
		delete(ar.Metadata, "gateway") // never forwarded upstream
	}
	req, err := translate.AnthropicToCanonical(&ar)
	if err != nil {
		return rc, nil, nil, api.NewError(api.CodeInvalidRequest, "%v", err)
	}
	if hints.EndUser == "" {
		hints.EndUser = req.User
	}
	rc.Hints = hints
	if e := s.Shed(p, hints.Priority); e != nil {
		return rc, nil, nil, e
	}
	return rc, &ar, req, nil
}

// handleMessages serves the Anthropic-compatible Messages endpoint [API-02, API-06].
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	rc, ar, req, aerr := s.prepareMessages(w, r)
	if aerr != nil {
		w.Header().Set("X-Request-Id", rc.ID)
		api.WriteError(w, api.FormatAnthropic, aerr)
		return
	}
	if ar.Stream {
		s.streamMessages(w, r, rc, req)
		return
	}
	resp, meta, err := s.Engine.Chat(r.Context(), rc, req)
	if err != nil {
		writeErr(w, api.FormatAnthropic, meta, err)
		return
	}
	meta.Headers(w.Header())
	meta.CostHeaders(w.Header())
	w.Header().Set("Content-Type", "application/json")
	if len(resp.RawAnthropic) > 0 && meta.CacheStatus != gateway.CacheExactHit && meta.CacheStatus != gateway.CacheSemanticHit {
		// Anthropic upstream: forward its body unchanged except for the model name.
		var m map[string]json.RawMessage
		if json.Unmarshal(resp.RawAnthropic, &m) == nil {
			m["model"], _ = json.Marshal(meta.Deployment)
			b, _ := json.Marshal(m)
			w.Write(b)
			return
		}
	}
	out := translate.ResponseToAnthropic(resp, meta.Deployment)
	b, _ := json.Marshal(out)
	w.Write(b)
}

// streamMessages writes an Anthropic event stream. Events from an Anthropic-format
// upstream are replayed verbatim; anything else is encoded from canonical chunks.
func (s *Server) streamMessages(w http.ResponseWriter, r *http.Request, rc *gateway.RequestContext, req *api.ChatRequest) {
	cs, err := s.Engine.ChatStream(r.Context(), rc, req)
	if err != nil {
		writeErr(w, api.FormatAnthropic, cs.Meta, err)
		return
	}
	h := w.Header()
	cs.Meta.Headers(h)
	for _, t := range gateway.CostTrailerNames {
		h.Add("Trailer", t)
	}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	enc := translate.NewStreamEncoder(rc.ID, cs.Meta.Deployment, tokens.Prompt(req))
	var clientErr error
	write := func(name string, data []byte) bool {
		if _, err := fmt.Fprint(w, translate.SSE(name, data)); err != nil {
			clientErr = err
			return false
		}
		if fl != nil {
			fl.Flush()
		}
		return true
	}
	writeEvents := func(evs []translate.Event) bool {
		for _, ev := range evs {
			b, _ := json.Marshal(ev.Data)
			if !write(ev.Name, b) {
				return false
			}
		}
		return true
	}
	raw := false
	sawStop := false
	for {
		ch, err := cs.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if r.Context().Err() != nil {
				clientErr = r.Context().Err()
				break
			}
			b, _ := json.Marshal(translate.Error("upstream stream failed: " + err.Error()).Data)
			write("error", b)
			break
		}
		if len(ch.Raw) > 0 {
			raw = true
			for _, ev := range ch.Raw {
				if ev.Event == "message_stop" {
					sawStop = true
				}
				if !write(ev.Event, translate.RewriteModel(ev, cs.Meta.Deployment)) {
					break
				}
			}
			continue
		}
		if !writeEvents(enc.Encode(ch)) {
			break
		}
	}
	if clientErr == nil && r.Context().Err() != nil {
		clientErr = r.Context().Err()
	}
	meta := cs.Close(clientErr)
	if clientErr != nil {
		return
	}
	if !raw && cs.FailErr == nil {
		writeEvents(enc.Finish(meta.Usage.TotalOutput()))
	} else if raw && !sawStop && cs.FailErr == nil {
		writeEvents(enc.Finish(meta.Usage.TotalOutput()))
	}
	meta.CostHeaders(h)
	if fl != nil {
		fl.Flush()
	}
}

// handleCountTokens estimates input tokens for an Anthropic request (Claude Code calls
// this before sending large prompts).
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if _, aerr := s.Authenticate(r, "messages"); aerr != nil {
		api.WriteError(w, api.FormatAnthropic, aerr)
		return
	}
	body, aerr := readBody(w, r, s.Engine.Snapshot().Config.Server.MaxBodyBytes)
	if aerr != nil {
		api.WriteError(w, api.FormatAnthropic, aerr)
		return
	}
	var ar api.AnthropicRequest
	if err := json.Unmarshal(body, &ar); err != nil {
		api.WriteError(w, api.FormatAnthropic, api.NewError(api.CodeInvalidRequest, "invalid JSON body"))
		return
	}
	ar.MaxTokens = 1
	req, err := translate.AnthropicToCanonical(&ar)
	if err != nil {
		api.WriteError(w, api.FormatAnthropic, api.NewError(api.CodeInvalidRequest, "%v", err))
		return
	}
	w.Header().Set("X-Gw-Usage-Estimated", "true")
	writeJSON(w, http.StatusOK, map[string]int{"input_tokens": tokens.Prompt(req)})
}

// anthropicModels renders the model list in Anthropic's shape.
func anthropicModels(l *api.ModelList) map[string]any {
	data := make([]map[string]any, 0, len(l.Data))
	for _, m := range l.Data {
		data = append(data, map[string]any{"type": "model", "id": m.ID, "display_name": m.ID,
			"created_at": time.Unix(m.Created, 0).UTC().Format(time.RFC3339)})
	}
	out := map[string]any{"data": data, "has_more": false}
	if len(data) > 0 {
		out["first_id"], out["last_id"] = data[0]["id"], data[len(data)-1]["id"]
	}
	return out
}
