package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/catalog"
	"github.com/SureshKhemka/llm-gateway/internal/translate"
)

// Anthropic talks to the Anthropic Messages API, or any server speaking it (LM Studio's
// Anthropic-compatible endpoint, the simulator). It is a protocol adapter and works at
// the wire level: requests that arrived in Anthropic format are forwarded verbatim
// (unknown fields included) and upstream events are kept for verbatim replay [API-06].
type Anthropic struct {
	id      string
	baseURL string
	apiKey  string
	headers map[string]string
	client  *http.Client
	// DefaultMaxTokens fills max_tokens (required by the API) when a request has none.
	DefaultMaxTokens int
}

// AnthropicVersion is sent unless the provider's headers override it.
const AnthropicVersion = "2023-06-01"

func NewAnthropic(id, baseURL, apiKey string, headers map[string]string, connectTimeout time.Duration) *Anthropic {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com/v1"
	}
	if connectTimeout == 0 {
		connectTimeout = 5 * time.Second
	}
	return &Anthropic{id: id, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, headers: headers,
		client: &http.Client{Transport: NewTransport(connectTimeout)}, DefaultMaxTokens: 4096}
}

func (a *Anthropic) ID() string { return a.id }

func (a *Anthropic) request(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", AnthropicVersion)
	if a.apiKey != "" {
		req.Header.Set("x-api-key", a.apiKey)
	}
	for k, v := range a.headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

func (a *Anthropic) do(req *http.Request) (*http.Response, error) {
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, AsUpstream(err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		ue := ClassifyHTTP(resp.StatusCode, resp.Header, anthropicErrorMessage(b))
		if resp.StatusCode == 529 { // overloaded
			ue.Kind = KindServer
		}
		return nil, ue
	}
	return resp, nil
}

func anthropicErrorMessage(b []byte) string {
	var e struct {
		Error struct{ Type, Message string } `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		return e.Error.Type + ": " + e.Error.Message
	}
	return string(b)
}

func (a *Anthropic) body(d *catalog.Deployment, req *api.ChatRequest, stream bool) (*api.AnthropicRequest, error) {
	r := *req
	r.Stream = stream
	out, err := translate.CanonicalToAnthropic(&r, d.Model, a.DefaultMaxTokens)
	if err != nil {
		return nil, &UpstreamError{Kind: KindClient, Status: 400, Message: err.Error()}
	}
	// Gateway routing hints never go upstream.
	if out.Metadata != nil {
		md := map[string]any{}
		for k, v := range out.Metadata {
			if k != "gateway" {
				md[k] = v
			}
		}
		out.Metadata = md
	}
	return out, nil
}

func (a *Anthropic) Chat(ctx context.Context, d *catalog.Deployment, req *api.ChatRequest, to catalog.Timeouts) (*api.ChatResponse, error) {
	ctx, cancel := withTotal(ctx, to)
	defer cancel()
	if to.TTFT > 0 && to.Total == 0 {
		var c2 context.CancelFunc
		ctx, c2 = context.WithTimeout(ctx, to.TTFT)
		defer c2()
	}
	body, err := a.body(d, req, false)
	if err != nil {
		return nil, err
	}
	hr, err := a.request(ctx, http.MethodPost, "/messages", body)
	if err != nil {
		return nil, err
	}
	resp, err := a.do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, AsUpstreamOrProtocol(err, "reading response")
	}
	var ar api.AnthropicResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return nil, &UpstreamError{Kind: KindProtocol, Message: "invalid JSON response: " + err.Error()}
	}
	out := translate.ResponseFromAnthropic(&ar, raw)
	out.Created = time.Now().Unix()
	return out, nil
}

func (a *Anthropic) ChatStream(ctx context.Context, d *catalog.Deployment, req *api.ChatRequest, to catalog.Timeouts) (Stream, error) {
	ctx, cancel := withTotal(ctx, to)
	body, err := a.body(d, req, true)
	if err != nil {
		cancel()
		return nil, err
	}
	hr, err := a.request(ctx, http.MethodPost, "/messages", body)
	if err != nil {
		cancel()
		return nil, err
	}
	w := newWatchdog(cancel, to.TTFT, to.Idle)
	resp, err := a.do(hr)
	if err != nil {
		w.stop()
		cancel()
		if w.fired.Load() {
			return nil, &UpstreamError{Kind: KindTimeout, Message: "time to first token exceeded"}
		}
		return nil, err
	}
	return &anthropicStream{body: resp.Body, rd: bufio.NewReaderSize(resp.Body, 64<<10), cancel: cancel, w: w,
		dec: translate.NewStreamDecoder(time.Now().Unix())}, nil
}

type anthropicStream struct {
	body    io.ReadCloser
	rd      *bufio.Reader
	cancel  context.CancelFunc
	w       *watchdog
	dec     *translate.StreamDecoder
	pending []*api.ChatChunk
	done    bool
}

func (s *anthropicStream) Recv() (*api.ChatChunk, error) {
	for {
		if len(s.pending) > 0 {
			c := s.pending[0]
			s.pending = s.pending[1:]
			return c, nil
		}
		if s.done {
			return nil, io.EOF
		}
		event, data, err := s.readEvent()
		if err != nil {
			return nil, err
		}
		s.w.tick()
		chunks, derr := s.dec.Decode(event, data)
		if derr != nil {
			return nil, &UpstreamError{Kind: KindServer, Message: derr.Error()}
		}
		if event == "message_stop" {
			s.done = true
		}
		s.pending = append(s.pending, chunks...)
	}
}

// readEvent reads one "event:/data:" SSE frame.
func (s *anthropicStream) readEvent() (string, []byte, error) {
	var event string
	var data bytes.Buffer
	for {
		line, err := s.rd.ReadString('\n')
		if err != nil {
			if s.w.fired.Load() {
				if s.w.first.Load() {
					return "", nil, &UpstreamError{Kind: KindTimeout, Message: "inter-token idle timeout"}
				}
				return "", nil, &UpstreamError{Kind: KindTimeout, Message: "time to first token exceeded"}
			}
			if errors.Is(err, io.EOF) {
				return "", nil, &UpstreamError{Kind: KindProtocol, Message: "stream ended unexpectedly"}
			}
			return "", nil, AsUpstream(err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if data.Len() > 0 {
				return event, data.Bytes(), nil
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
}

func (s *anthropicStream) Close() error {
	s.w.stop()
	s.cancel()
	return s.body.Close()
}

func (a *Anthropic) Embeddings(context.Context, *catalog.Deployment, *api.EmbeddingsRequest, catalog.Timeouts) (*api.EmbeddingsResponse, error) {
	return nil, &UpstreamError{Kind: KindClient, Status: 400, Message: "the Anthropic API has no embeddings endpoint"}
}

// ListModels lists models via GET /models (Anthropic API and LM Studio both serve it).
func (a *Anthropic) ListModels(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	hr, err := a.request(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, AsUpstreamOrProtocol(err, "invalid model list")
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}
