package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/catalog"
	"github.com/SureshKhemka/llm-gateway/internal/sim"
)

type contractEnv struct {
	sim     *sim.Instance
	srv     *httptest.Server
	mu      sync.Mutex
	headers http.Header
}

func newContractEnv(t *testing.T) *contractEnv {
	e := &contractEnv{sim: sim.New("sima", sim.Model{ID: "qwen-4b", Tier: 1}, sim.Model{ID: "nomic-embed", Tier: 0, Embeddings: true})}
	h := e.sim.Handler()
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.headers = r.Header.Clone()
		e.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *contractEnv) lastHeaders() http.Header {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.headers
}

func userReq(text string) *api.ChatRequest {
	return &api.ChatRequest{Model: "qwen-4b", Messages: []api.Message{{Role: "user", Content: api.TextContent(text)}}}
}

var dep = &catalog.Deployment{ID: "sima/qwen-4b", Provider: "sima", Model: "qwen-4b"}

// The same contract for every provider type: chat, streaming (with usage), model
// listing, credentials and version headers, error classification for retry/fallback
// (429 + Retry-After, 5xx, unknown model, timeouts, connection refused, mid-stream
// drops) and missing usage.
// Covers: NFR-21, PRV-01, PRV-02, RES-03, API-05
func TestNFR21_ProviderContract(t *testing.T) {
	types := map[string]func(base string) Provider{
		"openai": func(base string) Provider {
			return NewOpenAI("sima", base, "sk-test-key", map[string]string{"X-Extra": "1"}, time.Second)
		},
		"anthropic": func(base string) Provider {
			return NewAnthropic("sima", base, "sk-test-key", map[string]string{"X-Extra": "1"}, time.Second)
		},
	}
	for name, mk := range types {
		t.Run(name, func(t *testing.T) {
			e := newContractEnv(t)
			p := mk(e.srv.URL + "/v1")
			ctx := context.Background()
			to := catalog.Timeouts{Total: 5 * time.Second}

			resp, err := p.Chat(ctx, dep, userReq("hello contract"), to)
			if err != nil || len(resp.Choices) == 0 || resp.Choices[0].Message.Content.PlainText() == "" {
				t.Fatalf("chat: %v %+v", err, resp)
			}
			if resp.Usage == nil || resp.Usage.PromptTokens == 0 || resp.Usage.CompletionTokens == 0 {
				t.Errorf("usage: %+v", resp.Usage)
			}
			h := e.lastHeaders()
			if h.Get("X-Extra") != "1" {
				t.Error("configured headers not sent")
			}
			switch name {
			case "openai":
				if h.Get("Authorization") != "Bearer sk-test-key" {
					t.Errorf("auth header %q", h.Get("Authorization"))
				}
			case "anthropic":
				if h.Get("X-Api-Key") != "sk-test-key" || h.Get("Anthropic-Version") != "2023-06-01" {
					t.Errorf("anthropic headers: key %q version %q", h.Get("X-Api-Key"), h.Get("Anthropic-Version"))
				}
			}

			st, err := p.ChatStream(ctx, dep, userReq("stream the contract answer please"), to)
			if err != nil {
				t.Fatal(err)
			}
			var text strings.Builder
			var usage *api.Usage
			for {
				c, err := st.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("stream: %v", err)
				}
				for _, ch := range c.Choices {
					if ch.Delta.Content != nil {
						text.WriteString(*ch.Delta.Content)
					}
				}
				if c.Usage != nil {
					usage = c.Usage
				}
			}
			st.Close()
			if text.Len() == 0 || usage == nil || usage.CompletionTokens == 0 {
				t.Errorf("stream text %q usage %+v", text.String(), usage)
			}

			models, err := p.ListModels(ctx)
			if err != nil || !strings.Contains(strings.Join(models, ","), "qwen-4b") {
				t.Errorf("models: %v %v", models, err)
			}

			// Error classification.
			check := func(label string, f sim.Faults, req *api.ChatRequest, to catalog.Timeouts, want Kind) *UpstreamError {
				t.Helper()
				e.sim.SetFaults("", f)
				defer e.sim.ClearFaults()
				_, err := p.Chat(ctx, dep, req, to)
				ue := AsUpstream(err)
				if ue == nil || ue.Kind != want {
					t.Errorf("%s: got %v, want kind %s", label, err, want)
				}
				return ue
			}
			if ue := check("429", sim.Faults{RateLimit: true, RetryAfterSec: 3}, userReq("x"), to, KindRateLimited); ue != nil {
				if ue.RetryAfter != 3*time.Second || !ue.Retriable() {
					t.Errorf("429: retry-after %v retriable %v", ue.RetryAfter, ue.Retriable())
				}
			}
			if ue := check("500", sim.Faults{ErrorRate: 1}, userReq("x"), to, KindServer); ue != nil && (!ue.Retriable() || !ue.CountsAgainstHealth()) {
				t.Error("5xx must be retriable and count against health")
			}
			check("down", sim.Faults{Down: true}, userReq("x"), to, KindServer)
			check("timeout", sim.Faults{Timeout: true}, userReq("x"), catalog.Timeouts{TTFT: 200 * time.Millisecond}, KindTimeout)
			missing := userReq("x")
			missing.Model = "not-loaded"
			_, err = p.Chat(ctx, &catalog.Deployment{ID: "sima/not-loaded", Provider: "sima", Model: "not-loaded"}, missing, to)
			if ue := AsUpstream(err); ue == nil || ue.Kind != KindUnavailable {
				t.Errorf("unknown model: %v", err)
			}

			// Missing usage is not an error (the gateway estimates it).
			e.sim.SetFaults("", sim.Faults{OmitUsage: true})
			if r, err := p.Chat(ctx, dep, userReq("no usage"), to); err != nil || r.Usage != nil && r.Usage.CompletionTokens != 0 {
				t.Errorf("omitted usage: %v %+v", err, r)
			}
			e.sim.ClearFaults()

			// A stream cut mid-way surfaces as an error, not a clean end.
			e.sim.SetFaults("", sim.Faults{DropAfterTokens: 2})
			st, err = p.ChatStream(ctx, dep, userReq("a long answer that will be cut off part way through"), to)
			if err == nil {
				var serr error
				for serr == nil {
					_, serr = st.Recv()
				}
				st.Close()
				if errors.Is(serr, io.EOF) {
					t.Error("mid-stream drop reported as a clean end of stream")
				}
			}
			e.sim.ClearFaults()

			// Connection refused.
			dead := mk("http://127.0.0.1:1/v1")
			if _, err := dead.Chat(ctx, dep, userReq("x"), to); AsUpstream(err) == nil || AsUpstream(err).Kind != KindConnect {
				t.Errorf("connection refused: %v", err)
			}

			// Embeddings: OpenAI-type providers serve them; Anthropic says it can't.
			emb, err := p.Embeddings(ctx, &catalog.Deployment{ID: "sima/nomic-embed", Provider: "sima", Model: "nomic-embed"},
				&api.EmbeddingsRequest{Model: "nomic-embed", Input: []byte(`["a","b"]`)}, to)
			if name == "openai" && (err != nil || len(emb.Data) != 2) {
				t.Errorf("embeddings: %v", err)
			}
			if name == "anthropic" && err == nil {
				t.Error("anthropic embeddings should be refused")
			}
		})
	}
}
