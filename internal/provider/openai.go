package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/catalog"
)

// OpenAI talks to any OpenAI-compatible server: LM Studio, vLLM, Ollama, OpenRouter,
// OpenAI [PRV-01].
type OpenAI struct {
	id      string
	baseURL string
	apiKey  string
	headers map[string]string
	client  *http.Client
}

// NewOpenAI builds an adapter. connectTimeout bounds TCP+TLS setup [RES-06].
func NewOpenAI(id, baseURL, apiKey string, headers map[string]string, connectTimeout time.Duration) *OpenAI {
	if connectTimeout == 0 {
		connectTimeout = 5 * time.Second
	}
	return &OpenAI{
		id: id, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, headers: headers,
		client: &http.Client{Transport: NewTransport(connectTimeout)},
	}
}

// NewTransport returns a pooled transport with a dial timeout.
func NewTransport(connectTimeout time.Duration) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = connectTimeout
	t.MaxIdleConns = 512
	t.MaxIdleConnsPerHost = 256
	t.IdleConnTimeout = 90 * time.Second
	return t
}

func (o *OpenAI) ID() string { return o.id }

func (o *OpenAI) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, o.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	return req, nil
}

func (o *OpenAI) do(req *http.Request) (*http.Response, error) {
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, AsUpstream(err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, ClassifyHTTP(resp.StatusCode, resp.Header, errorMessage(b))
	}
	return resp, nil
}

func errorMessage(b []byte) string {
	var e struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && len(e.Error) > 0 {
		var inner struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(e.Error, &inner) == nil && inner.Message != "" {
			return inner.Message
		}
		var s string
		if json.Unmarshal(e.Error, &s) == nil {
			return s
		}
	}
	return string(b)
}

func withTotal(ctx context.Context, to catalog.Timeouts) (context.Context, context.CancelFunc) {
	if to.Total > 0 {
		return context.WithTimeout(ctx, to.Total)
	}
	return context.WithCancel(ctx)
}

func (o *OpenAI) body(d *catalog.Deployment, req *api.ChatRequest, stream bool) *api.ChatRequest {
	out := req.Clone()
	out.Model = d.Model
	out.Stream = stream
	out.Metadata = nil // gateway hints never go upstream
	if stream {
		out.StreamOptions = &api.StreamOptions{IncludeUsage: true}
	} else {
		out.StreamOptions = nil
	}
	return out
}

func (o *OpenAI) Chat(ctx context.Context, d *catalog.Deployment, req *api.ChatRequest, to catalog.Timeouts) (*api.ChatResponse, error) {
	ctx, cancel := withTotal(ctx, to)
	defer cancel()
	// Non-streaming: the first byte only arrives with the full answer, so TTFT here
	// bounds time-to-response unless a total timeout is set.
	if to.TTFT > 0 && to.Total == 0 {
		var c2 context.CancelFunc
		ctx, c2 = context.WithTimeout(ctx, to.TTFT)
		defer c2()
	}
	hr, err := o.newRequest(ctx, http.MethodPost, "/chat/completions", o.body(d, req, false))
	if err != nil {
		return nil, err
	}
	resp, err := o.do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out api.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, AsUpstreamOrProtocol(err, "invalid JSON response")
	}
	if len(out.Choices) == 0 {
		return nil, &UpstreamError{Kind: KindProtocol, Message: "response has no choices"}
	}
	return &out, nil
}

// AsUpstreamOrProtocol maps a decode error: context errors keep their kind; anything
// else is a protocol error.
func AsUpstreamOrProtocol(err error, msg string) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return AsUpstream(err)
	}
	return &UpstreamError{Kind: KindProtocol, Message: msg + ": " + err.Error()}
}

func (o *OpenAI) ChatStream(ctx context.Context, d *catalog.Deployment, req *api.ChatRequest, to catalog.Timeouts) (Stream, error) {
	ctx, cancel := withTotal(ctx, to)
	hr, err := o.newRequest(ctx, http.MethodPost, "/chat/completions", o.body(d, req, true))
	if err != nil {
		cancel()
		return nil, err
	}
	w := newWatchdog(cancel, to.TTFT, to.Idle)
	resp, err := o.do(hr)
	if err != nil {
		w.stop()
		cancel()
		if w.fired.Load() {
			return nil, &UpstreamError{Kind: KindTimeout, Message: "time to first token exceeded"}
		}
		return nil, err
	}
	return &sseStream{body: resp.Body, rd: bufio.NewReaderSize(resp.Body, 64<<10), cancel: cancel, w: w}, nil
}

// watchdog cancels the request if no data arrives within TTFT (first chunk) or idle
// (between chunks) [RES-06].
type watchdog struct {
	mu    sync.Mutex
	timer *time.Timer
	idle  time.Duration
	fired atomic.Bool
	first atomic.Bool
}

func newWatchdog(cancel context.CancelFunc, ttft, idle time.Duration) *watchdog {
	w := &watchdog{idle: idle}
	if ttft > 0 {
		w.timer = time.AfterFunc(ttft, func() { w.fired.Store(true); cancel() })
	} else if idle > 0 {
		w.timer = time.AfterFunc(idle, func() { w.fired.Store(true); cancel() })
	}
	return w
}

func (w *watchdog) tick() {
	w.first.Store(true)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil && w.idle > 0 {
		w.timer.Reset(w.idle)
	} else if w.timer != nil {
		w.timer.Stop()
	}
}

func (w *watchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
	}
}

type sseStream struct {
	body   io.ReadCloser
	rd     *bufio.Reader
	cancel context.CancelFunc
	w      *watchdog
	done   bool
}

func (s *sseStream) Recv() (*api.ChatChunk, error) {
	if s.done {
		return nil, io.EOF
	}
	for {
		line, err := s.rd.ReadString('\n')
		if err != nil {
			if s.w.fired.Load() {
				if s.w.first.Load() {
					return nil, &UpstreamError{Kind: KindTimeout, Message: "inter-token idle timeout"}
				}
				return nil, &UpstreamError{Kind: KindTimeout, Message: "time to first token exceeded"}
			}
			if errors.Is(err, io.EOF) {
				// A stream that ends without [DONE] was cut off.
				return nil, &UpstreamError{Kind: KindProtocol, Message: "stream ended unexpectedly"}
			}
			return nil, AsUpstream(err)
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			s.done = true
			return nil, io.EOF
		}
		if data == "" {
			continue
		}
		s.w.tick()
		if strings.HasPrefix(data, `{"error"`) {
			return nil, &UpstreamError{Kind: KindServer, Message: errorMessage([]byte(data))}
		}
		var ch api.ChatChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return nil, &UpstreamError{Kind: KindProtocol, Message: fmt.Sprintf("bad stream chunk: %v", err)}
		}
		return &ch, nil
	}
}

func (s *sseStream) Close() error {
	s.w.stop()
	s.cancel()
	return s.body.Close()
}

func (o *OpenAI) Embeddings(ctx context.Context, d *catalog.Deployment, req *api.EmbeddingsRequest, to catalog.Timeouts) (*api.EmbeddingsResponse, error) {
	ctx, cancel := withTotal(ctx, to)
	defer cancel()
	body := *req
	body.Model = d.Model
	hr, err := o.newRequest(ctx, http.MethodPost, "/embeddings", body)
	if err != nil {
		return nil, err
	}
	resp, err := o.do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out api.EmbeddingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, AsUpstreamOrProtocol(err, "invalid embeddings response")
	}
	return &out, nil
}

// ListModels returns model IDs from GET /models [PRV-06].
func (o *OpenAI) ListModels(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	hr, err := o.newRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var list api.ModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, AsUpstreamOrProtocol(err, "invalid model list")
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}
