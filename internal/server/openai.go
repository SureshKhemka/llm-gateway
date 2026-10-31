package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/gateway"
)

// prepareChat authenticates and parses an OpenAI chat request.
func (s *Server) prepareChat(w http.ResponseWriter, r *http.Request, endpoint string) (*gateway.RequestContext, *api.ChatRequest, *api.Error) {
	rc := s.newRC(r, endpoint, api.FormatOpenAI)
	p, aerr := s.Authenticate(r, endpoint)
	if aerr != nil {
		return rc, nil, aerr
	}
	rc.Principal = p
	body, aerr := readBody(w, r, s.Engine.Snapshot().Config.Server.MaxBodyBytes)
	if aerr != nil {
		return rc, nil, aerr
	}
	var req api.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return rc, nil, api.NewError(api.CodeInvalidRequest, "invalid JSON body: %v", err)
	}
	if len(req.Messages) == 0 {
		return rc, nil, api.NewError(api.CodeInvalidRequest, "messages must not be empty")
	}
	hints, herr := ParseHints(r.Header, req.Metadata)
	if herr != nil {
		return rc, nil, herr
	}
	if hints.EndUser == "" {
		hints.EndUser = req.User
	}
	rc.Hints = hints
	return rc, &req, nil
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	rc, req, aerr := s.prepareChat(w, r, "chat")
	if aerr != nil {
		w.Header().Set("X-Request-Id", rc.ID)
		api.WriteError(w, api.FormatOpenAI, aerr)
		return
	}
	if req.Stream {
		s.streamChat(w, r, rc, req)
		return
	}
	resp, meta, err := s.Engine.Chat(r.Context(), rc, req)
	if err != nil {
		writeErr(w, api.FormatOpenAI, meta, err)
		return
	}
	meta.Headers(w.Header())
	meta.CostHeaders(w.Header())
	writeJSON(w, http.StatusOK, resp)
}

// streamChat writes an OpenAI SSE stream. Cost fields arrive as HTTP trailers and, when
// the client asked for usage, on the final usage chunk [API-09, API-13].
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, rc *gateway.RequestContext, req *api.ChatRequest) {
	cs, err := s.Engine.ChatStream(r.Context(), rc, req)
	if err != nil {
		writeErr(w, api.FormatOpenAI, cs.Meta, err)
		return
	}
	wantUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
	h := w.Header()
	cs.Meta.Headers(h)
	for _, t := range gateway.CostTrailerNames {
		h.Add("Trailer", t)
	}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)

	var id, model string
	var created int64
	var clientErr error
	write := func(v any) bool {
		b, _ := json.Marshal(v)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			clientErr = err
			return false
		}
		if fl != nil {
			fl.Flush()
		}
		return true
	}
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
			// A failure after the first token ends the stream with a well-formed error
			// event; models are never switched mid-stream [RES-04].
			write(map[string]any{"error": map[string]any{
				"message": "upstream stream failed: " + err.Error(), "type": "api_error", "code": api.CodeUpstreamUnavailable,
			}})
			break
		}
		id, model, created = ch.ID, ch.Model, ch.Created
		if !write(ch) {
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
	if wantUsage {
		u := meta.Usage
		write(api.ChatChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model, Choices: []api.ChunkChoice{},
			Usage: &api.Usage{PromptTokens: u.TotalInput(), CompletionTokens: u.TotalOutput(), TotalTokens: u.TotalInput() + u.TotalOutput(),
				PromptTokensDetails: &api.PromptTokensDetails{CachedTokens: u.CachedInput}, CompletionTokensDetails: &api.CompletionTokensDetails{ReasoningTokens: u.Reasoning}},
			Gateway: meta.Summary()})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	meta.CostHeaders(h)
	if fl != nil {
		fl.Flush()
	}
}

func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	rc := s.newRC(r, "embeddings", api.FormatOpenAI)
	p, aerr := s.Authenticate(r, "embeddings")
	if aerr != nil {
		api.WriteError(w, api.FormatOpenAI, aerr)
		return
	}
	rc.Principal = p
	body, aerr := readBody(w, r, s.Engine.Snapshot().Config.Server.MaxBodyBytes)
	if aerr != nil {
		api.WriteError(w, api.FormatOpenAI, aerr)
		return
	}
	var req api.EmbeddingsRequest
	if err := json.Unmarshal(body, &req); err != nil || len(req.Input) == 0 {
		api.WriteError(w, api.FormatOpenAI, api.NewError(api.CodeInvalidRequest, "invalid embeddings request"))
		return
	}
	hints, herr := ParseHints(r.Header, nil)
	if herr != nil {
		api.WriteError(w, api.FormatOpenAI, herr)
		return
	}
	if hints.EndUser == "" {
		hints.EndUser = req.User
	}
	rc.Hints = hints
	resp, meta, err := s.Engine.Embeddings(r.Context(), rc, &req)
	if err != nil {
		writeErr(w, api.FormatOpenAI, meta, err)
		return
	}
	meta.Headers(w.Header())
	meta.CostHeaders(w.Header())
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	rc := s.newRC(r, "models", formatOf(r))
	p, aerr := s.Authenticate(r, "models")
	if aerr != nil {
		api.WriteError(w, api.FormatOpenAI, aerr)
		return
	}
	rc.Principal = p
	writeJSON(w, http.StatusOK, s.Engine.Models(rc))
}
