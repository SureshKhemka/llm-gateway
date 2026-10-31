package provider

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/catalog"
)

// Anthropic talks to the Anthropic Messages API (or any server speaking it, such as
// LM Studio's Anthropic-compatible endpoint). Translation is implemented with M6.
type Anthropic struct {
	id      string
	baseURL string
	apiKey  string
	headers map[string]string
	client  *http.Client
}

func NewAnthropic(id, baseURL, apiKey string, headers map[string]string, connectTimeout time.Duration) *Anthropic {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com/v1"
	}
	if connectTimeout == 0 {
		connectTimeout = 5 * time.Second
	}
	return &Anthropic{id: id, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, headers: headers,
		client: &http.Client{Transport: NewTransport(connectTimeout)}}
}

func (a *Anthropic) ID() string { return a.id }

var errAnthropicPending = &UpstreamError{Kind: KindClient, Status: 501, Message: "anthropic provider arrives in M6"}

func (a *Anthropic) Chat(context.Context, *catalog.Deployment, *api.ChatRequest, catalog.Timeouts) (*api.ChatResponse, error) {
	return nil, errAnthropicPending
}

func (a *Anthropic) ChatStream(context.Context, *catalog.Deployment, *api.ChatRequest, catalog.Timeouts) (Stream, error) {
	return nil, errAnthropicPending
}

func (a *Anthropic) Embeddings(context.Context, *catalog.Deployment, *api.EmbeddingsRequest, catalog.Timeouts) (*api.EmbeddingsResponse, error) {
	return nil, &UpstreamError{Kind: KindClient, Status: 400, Message: "the Anthropic API has no embeddings endpoint"}
}

func (a *Anthropic) ListModels(context.Context) ([]string, error) { return nil, nil }
