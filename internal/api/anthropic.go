package api

import (
	"bytes"
	"encoding/json"
)

// Anthropic Messages API wire types (the subset the gateway translates; unknown fields
// survive in Extra and in the raw body for Anthropic-to-Anthropic pass-through).

type AnthropicRequest struct {
	Model         string               `json:"model"`
	MaxTokens     int                  `json:"max_tokens"`
	System        AnthropicContent     `json:"system,omitempty"`
	Messages      []AnthropicMessage   `json:"messages"`
	Stream        bool                 `json:"stream,omitempty"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	TopK          *int                 `json:"top_k,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Tools         []AnthropicTool      `json:"tools,omitempty"`
	ToolChoice    *AnthropicToolChoice `json:"tool_choice,omitempty"`
	Metadata      map[string]any       `json:"metadata,omitempty"`
	Thinking      json.RawMessage      `json:"thinking,omitempty"`
	OutputConfig  *OutputConfig        `json:"output_config,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

type OutputConfig struct {
	Effort string          `json:"effort,omitempty"`
	Format *OutputFormat   `json:"format,omitempty"`
	Extra  json.RawMessage `json:"-"`
}

type OutputFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

func (r AnthropicRequest) MarshalJSON() ([]byte, error) {
	type alias AnthropicRequest
	return marshalWithExtra(alias(r), r.Extra)
}

func (r *AnthropicRequest) UnmarshalJSON(b []byte) error {
	type alias AnthropicRequest
	var a alias
	extra, err := unmarshalWithExtra(b, &a)
	if err != nil {
		return err
	}
	*r = AnthropicRequest(a)
	r.Extra = extra
	return nil
}

// AnthropicContent is a string or a list of content blocks.
type AnthropicContent struct {
	Text   string
	Blocks []AnthropicBlock
	set    bool
}

func (c AnthropicContent) IsZero() bool { return !c.set && c.Text == "" && c.Blocks == nil }

func (c AnthropicContent) MarshalJSON() ([]byte, error) {
	if c.Blocks != nil {
		return json.Marshal(c.Blocks)
	}
	return json.Marshal(c.Text)
}

func (c *AnthropicContent) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	c.set = true
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &c.Text)
	}
	if string(b) == "null" {
		return nil
	}
	return json.Unmarshal(b, &c.Blocks)
}

// AsBlocks returns the content as blocks (a string becomes one text block).
func (c AnthropicContent) AsBlocks() []AnthropicBlock {
	if c.Blocks != nil {
		return c.Blocks
	}
	if c.Text == "" {
		return nil
	}
	return []AnthropicBlock{{Type: "text", Text: c.Text}}
}

type AnthropicMessage struct {
	Role    string           `json:"role"`
	Content AnthropicContent `json:"content"`
}

type AnthropicBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// image / document
	Source *AnthropicSource `json:"source,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string            `json:"tool_use_id,omitempty"`
	Content   *AnthropicContent `json:"content,omitempty"`
	IsError   bool              `json:"is_error,omitempty"`
	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

func (b AnthropicBlock) MarshalJSON() ([]byte, error) {
	type alias AnthropicBlock
	out, err := marshalWithExtra(alias(b), b.Extra)
	if err != nil {
		return nil, err
	}
	// tool_use blocks always carry input; Anthropic requires it.
	if b.Type == "tool_use" && len(b.Input) == 0 {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(out, &m)
		m["input"] = json.RawMessage("{}")
		return json.Marshal(m)
	}
	return out, nil
}

func (b *AnthropicBlock) UnmarshalJSON(raw []byte) error {
	type alias AnthropicBlock
	var a alias
	extra, err := unmarshalWithExtra(raw, &a)
	if err != nil {
		return err
	}
	*b = AnthropicBlock(a)
	b.Extra = extra
	return nil
}

type AnthropicSource struct {
	Type      string `json:"type"` // base64 | url | text
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type AnthropicTool struct {
	Type        string          `json:"type,omitempty"` // empty/custom for client tools; server tools have a versioned type
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

func (t AnthropicTool) MarshalJSON() ([]byte, error) {
	type alias AnthropicTool
	return marshalWithExtra(alias(t), t.Extra)
}

func (t *AnthropicTool) UnmarshalJSON(b []byte) error {
	type alias AnthropicTool
	var a alias
	extra, err := unmarshalWithExtra(b, &a)
	if err != nil {
		return err
	}
	*t = AnthropicTool(a)
	t.Extra = extra
	return nil
}

type AnthropicToolChoice struct {
	Type                   string `json:"type"` // auto | any | tool | none
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

type AnthropicResponse struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Model        string           `json:"model"`
	Content      []AnthropicBlock `json:"content"`
	StopReason   string           `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        AnthropicUsage   `json:"usage"`
}

type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

func init() {
	type ar AnthropicRequest
	type ab AnthropicBlock
	type at AnthropicTool
	_ = knownFieldsFor[ar]()
	_ = knownFieldsFor[ab]()
	_ = knownFieldsFor[at]()
}
