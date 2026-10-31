// Package api holds the client- and provider-facing wire types. The OpenAI Chat
// Completions shape doubles as the gateway's canonical request format.
package api

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
)

// Content is a message's content: either a plain string or a list of parts.
type Content struct {
	Text  string
	Parts []ContentPart
	Null  bool
}

type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
	// InputAudio and other part types are passed through untouched.
	Extra map[string]json.RawMessage `json:"-"`
}

type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

func TextContent(s string) Content { return Content{Text: s} }

func (c Content) MarshalJSON() ([]byte, error) {
	switch {
	case c.Parts != nil:
		return json.Marshal(c.Parts)
	case c.Null:
		return []byte("null"), nil
	default:
		return json.Marshal(c.Text)
	}
}

func (c *Content) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) == 0 || string(b) == "null":
		*c = Content{Null: true}
		return nil
	case b[0] == '"':
		*c = Content{}
		return json.Unmarshal(b, &c.Text)
	default:
		*c = Content{}
		return json.Unmarshal(b, &c.Parts)
	}
}

// PlainText concatenates all text in the content.
func (c Content) PlainText() string {
	if c.Parts == nil {
		return c.Text
	}
	var sb strings.Builder
	for _, p := range c.Parts {
		if p.Type == "text" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

func (c Content) HasImage() bool {
	for _, p := range c.Parts {
		if p.Type == "image_url" {
			return true
		}
	}
	return false
}

func (p ContentPart) MarshalJSON() ([]byte, error) {
	type alias ContentPart
	return marshalWithExtra(alias(p), p.Extra)
}

func (p *ContentPart) UnmarshalJSON(b []byte) error {
	type alias ContentPart
	var a alias
	extra, err := unmarshalWithExtra(b, &a)
	if err != nil {
		return err
	}
	*p = ContentPart(a)
	p.Extra = extra
	return nil
}

type Message struct {
	Role       string     `json:"role"`
	Content    Content    `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	// Refusal and reasoning content as some providers return them.
	Refusal          string `json:"refusal,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type ToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type ResponseFormat struct {
	Type       string      `json:"type"` // text | json_object | json_schema
	JSONSchema *JSONSchema `json:"json_schema,omitempty"`
}

type JSONSchema struct {
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Strict *bool           `json:"strict,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// StringList accepts a string or an array of strings.
type StringList []string

func (s *StringList) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var one string
		if err := json.Unmarshal(b, &one); err != nil {
			return err
		}
		*s = StringList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

type ChatRequest struct {
	Model               string          `json:"model"`
	Messages            []Message       `json:"messages"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Stop                StringList      `json:"stop,omitempty"`
	N                   *int            `json:"n,omitempty"`
	Seed                *int64          `json:"seed,omitempty"`
	PresencePenalty     *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64        `json:"frequency_penalty,omitempty"`
	Tools               []Tool          `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	ResponseFormat      *ResponseFormat `json:"response_format,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	User                string          `json:"user,omitempty"`
	Metadata            map[string]any  `json:"metadata,omitempty"`

	// Extra holds fields the gateway does not model; they are forwarded verbatim to
	// OpenAI-format upstreams.
	Extra map[string]json.RawMessage `json:"-"`
	// Anthropic is the original body when the client spoke the Anthropic format, so an
	// Anthropic-format upstream receives it without translation loss [API-06].
	Anthropic *AnthropicRequest `json:"-"`
}

func (r ChatRequest) MarshalJSON() ([]byte, error) {
	type alias ChatRequest
	return marshalWithExtra(alias(r), r.Extra)
}

func (r *ChatRequest) UnmarshalJSON(b []byte) error {
	type alias ChatRequest
	var a alias
	extra, err := unmarshalWithExtra(b, &a)
	if err != nil {
		return err
	}
	*r = ChatRequest(a)
	r.Extra = extra
	return nil
}

// Clone returns a deep copy (via JSON) so attempts can mutate their request freely.
func (r *ChatRequest) Clone() *ChatRequest {
	b, _ := json.Marshal(r)
	var out ChatRequest
	_ = json.Unmarshal(b, &out)
	out.Anthropic = r.Anthropic
	return &out
}

// MaxOutput returns the requested output cap, or def when none was given.
func (r *ChatRequest) MaxOutput(def int) int {
	if r.MaxCompletionTokens != nil && *r.MaxCompletionTokens > 0 {
		return *r.MaxCompletionTokens
	}
	if r.MaxTokens != nil && *r.MaxTokens > 0 {
		return *r.MaxTokens
	}
	return def
}

func (r *ChatRequest) HasImages() bool {
	for _, m := range r.Messages {
		if m.Content.HasImage() {
			return true
		}
	}
	return false
}

func (r *ChatRequest) WantsJSONSchema() bool {
	return r.ResponseFormat != nil && r.ResponseFormat.Type == "json_schema"
}

type Usage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

func (u *Usage) Cached() int {
	if u == nil || u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CachedTokens
}

func (u *Usage) Reasoning() int {
	if u == nil || u.CompletionTokensDetails == nil {
		return 0
	}
	return u.CompletionTokensDetails.ReasoningTokens
}

type ChatResponse struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	Choices           []Choice `json:"choices"`
	Usage             *Usage   `json:"usage,omitempty"`
	SystemFingerprint string   `json:"system_fingerprint,omitempty"`
	// RawAnthropic is the upstream body when an Anthropic-format provider produced it.
	RawAnthropic json.RawMessage `json:"-"`
}

type Choice struct {
	Index        int             `json:"index"`
	Message      Message         `json:"message"`
	FinishReason string          `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}

type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
	// Gateway carries per-request metadata on the final chunk of a gateway stream.
	Gateway map[string]any `json:"x_gateway,omitempty"`
	// Raw holds the Anthropic-format upstream events this chunk came from, replayed
	// verbatim to Anthropic-format clients.
	Raw []RawEvent `json:"-"`
}

// RawEvent is one upstream SSE event.
type RawEvent struct {
	Event string
	Data  json.RawMessage
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type Delta struct {
	Role             string     `json:"role,omitempty"`
	Content          *string    `json:"content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	Refusal          *string    `json:"refusal,omitempty"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
}

type EmbeddingsRequest struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	EncodingFormat string          `json:"encoding_format,omitempty"`
	Dimensions     *int            `json:"dimensions,omitempty"`
	User           string          `json:"user,omitempty"`
}

// Inputs returns the input as strings (token-array inputs are not supported).
func (r *EmbeddingsRequest) Inputs() ([]string, error) {
	var one string
	if err := json.Unmarshal(r.Input, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(r.Input, &many); err != nil {
		return nil, err
	}
	return many, nil
}

type EmbeddingsResponse struct {
	Object string      `json:"object"`
	Data   []Embedding `json:"data"`
	Model  string      `json:"model"`
	Usage  *Usage      `json:"usage,omitempty"`
}

type Embedding struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

type ModelList struct {
	Object string        `json:"object"`
	Data   []ModelObject `json:"data"`
}

type ModelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ---- helpers for preserving unknown JSON fields ----

var knownFieldsCache sync.Map // reflect.Type -> map[string]bool

func knownFieldsFor[T any]() map[string]bool {
	var zero T
	return knownFields(reflect.TypeOf(zero))
}

func knownFields(t reflect.Type) map[string]bool {
	if m, ok := knownFieldsCache.Load(t); ok {
		return m.(map[string]bool)
	}
	m := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name != "" && name != "-" {
			m[name] = true
		}
	}
	knownFieldsCache.Store(t, m)
	return m
}

func init() {
	// Populate eagerly so the cache is read-only (and race-free) at runtime.
	type crAlias ChatRequest
	type cpAlias ContentPart
	knownFields(reflect.TypeOf(crAlias{}))
	knownFields(reflect.TypeOf(cpAlias{}))
}

func unmarshalWithExtra(b []byte, v any) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(b, v); err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	known := knownFields(reflect.TypeOf(v).Elem())
	var extra map[string]json.RawMessage
	for k, raw := range all {
		if !known[k] {
			if extra == nil {
				extra = map[string]json.RawMessage{}
			}
			extra[k] = raw
		}
	}
	return extra, nil
}

func marshalWithExtra(v any, extra map[string]json.RawMessage) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil || len(extra) == 0 {
		return b, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for k, raw := range extra {
		if _, exists := m[k]; !exists {
			m[k] = raw
		}
	}
	return json.Marshal(m)
}
