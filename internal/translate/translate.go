// Package translate converts between the OpenAI Chat Completions format (the gateway's
// canonical format) and the Anthropic Messages format: requests, responses, stop
// reasons, usage and streaming events [API-06].
package translate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SureshKhemka/llm-gateway/internal/api"
)

// FinishFromAnthropic maps an Anthropic stop_reason to an OpenAI finish_reason.
func FinishFromAnthropic(stop string) string {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	case "", "end_turn", "stop_sequence", "pause_turn":
		return "stop"
	}
	return "stop"
}

// StopToAnthropic maps an OpenAI finish_reason to an Anthropic stop_reason.
func StopToAnthropic(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	}
	return "end_turn"
}

func textBlocks(c api.AnthropicContent) string {
	var parts []string
	for _, b := range c.AsBlocks() {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// AnthropicToCanonical converts an Anthropic request for an OpenAI-format upstream.
func AnthropicToCanonical(in *api.AnthropicRequest) (*api.ChatRequest, error) {
	out := &api.ChatRequest{Model: in.Model, Stream: in.Stream, Temperature: in.Temperature, TopP: in.TopP, Anthropic: in}
	if in.MaxTokens > 0 {
		mt := in.MaxTokens
		out.MaxTokens = &mt
	}
	if len(in.StopSequences) > 0 {
		out.Stop = in.StopSequences
	}
	if in.TopK != nil {
		b, _ := json.Marshal(*in.TopK)
		out.Extra = map[string]json.RawMessage{"top_k": b}
	}
	if uid, ok := in.Metadata["user_id"].(string); ok {
		out.User = uid
	}
	if sys := textBlocks(in.System); sys != "" {
		out.Messages = append(out.Messages, api.Message{Role: "system", Content: api.TextContent(sys)})
	}
	for _, m := range in.Messages {
		msgs, err := messageToCanonical(m)
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, msgs...)
	}
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "custom" {
			return nil, fmt.Errorf("server tool %q (%s) needs an Anthropic-format deployment", t.Name, t.Type)
		}
		params := t.InputSchema
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, api.Tool{Type: "function", Function: api.FunctionDef{Name: t.Name, Description: t.Description, Parameters: params}})
	}
	if tc := in.ToolChoice; tc != nil {
		switch tc.Type {
		case "auto":
			out.ToolChoice = json.RawMessage(`"auto"`)
		case "any":
			out.ToolChoice = json.RawMessage(`"required"`)
		case "none":
			out.ToolChoice = json.RawMessage(`"none"`)
		case "tool":
			b, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}})
			out.ToolChoice = b
		}
		if tc.DisableParallelToolUse {
			f := false
			out.ParallelToolCalls = &f
		}
	}
	if oc := in.OutputConfig; oc != nil {
		if oc.Format != nil && oc.Format.Type == "json_schema" {
			out.ResponseFormat = &api.ResponseFormat{Type: "json_schema", JSONSchema: &api.JSONSchema{Name: "output", Schema: oc.Format.Schema}}
		}
		out.ReasoningEffort = oc.Effort
	}
	return out, nil
}

func messageToCanonical(m api.AnthropicMessage) ([]api.Message, error) {
	blocks := m.Content.AsBlocks()
	if m.Role == "assistant" {
		msg := api.Message{Role: "assistant"}
		var text []string
		for _, b := range blocks {
			switch b.Type {
			case "text":
				text = append(text, b.Text)
			case "tool_use":
				args := string(b.Input)
				if args == "" {
					args = "{}"
				}
				msg.ToolCalls = append(msg.ToolCalls, api.ToolCall{ID: b.ID, Type: "function", Function: api.FunctionCall{Name: b.Name, Arguments: args}})
			case "thinking":
				msg.ReasoningContent += b.Thinking
			case "redacted_thinking":
			default:
				return nil, fmt.Errorf("assistant content block %q needs an Anthropic-format deployment", b.Type)
			}
		}
		if len(text) > 0 {
			msg.Content = api.TextContent(strings.Join(text, ""))
		} else {
			msg.Content = api.Content{Null: len(msg.ToolCalls) > 0}
		}
		return []api.Message{msg}, nil
	}
	// User turn: tool results become tool messages (which must directly follow the
	// assistant's tool calls), remaining content one user message.
	var out []api.Message
	var parts []api.ContentPart
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			content := ""
			if b.Content != nil {
				content = textBlocks(*b.Content)
			}
			if b.IsError {
				content = "Error: " + content
			}
			out = append(out, api.Message{Role: "tool", ToolCallID: b.ToolUseID, Content: api.TextContent(content)})
		case "text":
			parts = append(parts, api.ContentPart{Type: "text", Text: b.Text})
		case "image":
			if b.Source == nil {
				return nil, fmt.Errorf("image block without source")
			}
			url := b.Source.URL
			if b.Source.Type == "base64" {
				url = "data:" + b.Source.MediaType + ";base64," + b.Source.Data
			}
			parts = append(parts, api.ContentPart{Type: "image_url", ImageURL: &api.ImageURL{URL: url}})
		case "document":
			if b.Source != nil && b.Source.Type == "text" {
				parts = append(parts, api.ContentPart{Type: "text", Text: b.Source.Data})
				continue
			}
			return nil, fmt.Errorf("document blocks other than plain text need an Anthropic-format deployment")
		default:
			return nil, fmt.Errorf("user content block %q needs an Anthropic-format deployment", b.Type)
		}
	}
	if len(parts) > 0 {
		c := api.Content{Parts: parts}
		if len(parts) == 1 && parts[0].Type == "text" {
			c = api.TextContent(parts[0].Text)
		}
		out = append(out, api.Message{Role: "user", Content: c})
	}
	return out, nil
}

// CanonicalToAnthropic converts a canonical request for an Anthropic-format upstream.
// A request that arrived in Anthropic format is forwarded as-is (model replaced).
func CanonicalToAnthropic(in *api.ChatRequest, model string, defaultMax int) (*api.AnthropicRequest, error) {
	if in.Anthropic != nil {
		out := *in.Anthropic
		out.Model = model
		out.Stream = in.Stream
		if mt := in.MaxOutput(0); mt > 0 {
			out.MaxTokens = mt
		}
		delete(out.Extra, "metadata_gateway")
		return &out, nil
	}
	out := &api.AnthropicRequest{Model: model, Stream: in.Stream, Temperature: in.Temperature, TopP: in.TopP, StopSequences: in.Stop}
	out.MaxTokens = in.MaxOutput(defaultMax)
	if in.User != "" {
		out.Metadata = map[string]any{"user_id": in.User}
	}
	var system []string
	var msgs []api.AnthropicMessage
	add := func(role string, blocks ...api.AnthropicBlock) {
		if len(msgs) > 0 && msgs[len(msgs)-1].Role == role {
			msgs[len(msgs)-1].Content.Blocks = append(msgs[len(msgs)-1].Content.Blocks, blocks...)
			return
		}
		msgs = append(msgs, api.AnthropicMessage{Role: role, Content: api.AnthropicContent{Blocks: blocks}})
	}
	for _, m := range in.Messages {
		switch m.Role {
		case "system", "developer":
			system = append(system, m.Content.PlainText())
		case "user":
			var blocks []api.AnthropicBlock
			if m.Content.Parts == nil {
				blocks = append(blocks, api.AnthropicBlock{Type: "text", Text: m.Content.Text})
			}
			for _, p := range m.Content.Parts {
				switch p.Type {
				case "text":
					blocks = append(blocks, api.AnthropicBlock{Type: "text", Text: p.Text})
				case "image_url":
					src, err := imageSource(p.ImageURL)
					if err != nil {
						return nil, err
					}
					blocks = append(blocks, api.AnthropicBlock{Type: "image", Source: src})
				}
			}
			add("user", blocks...)
		case "assistant":
			var blocks []api.AnthropicBlock
			if t := m.Content.PlainText(); t != "" {
				blocks = append(blocks, api.AnthropicBlock{Type: "text", Text: t})
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Function.Arguments)
				if !json.Valid(input) || len(input) == 0 {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, api.AnthropicBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input})
			}
			if len(blocks) > 0 {
				add("assistant", blocks...)
			}
		case "tool":
			c := api.AnthropicContent{Text: m.Content.PlainText()}
			add("user", api.AnthropicBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: &c})
		}
	}
	if len(msgs) > 0 && msgs[0].Role != "user" {
		msgs = append([]api.AnthropicMessage{{Role: "user", Content: api.AnthropicContent{Text: "(continue)"}}}, msgs...)
	}
	out.Messages = msgs
	if rf := in.ResponseFormat; rf != nil {
		switch rf.Type {
		case "json_schema":
			if rf.JSONSchema != nil {
				out.OutputConfig = &api.OutputConfig{Format: &api.OutputFormat{Type: "json_schema", Schema: rf.JSONSchema.Schema}}
			}
		case "json_object":
			system = append(system, "Respond with a single JSON object and nothing else.")
		}
	}
	if in.ReasoningEffort != "" {
		if out.OutputConfig == nil {
			out.OutputConfig = &api.OutputConfig{}
		}
		out.OutputConfig.Effort = in.ReasoningEffort
	}
	if len(system) > 0 {
		out.System = api.AnthropicContent{Text: strings.Join(system, "\n\n")}
	}
	for _, t := range in.Tools {
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, api.AnthropicTool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema})
	}
	if len(in.ToolChoice) > 0 {
		var s string
		if json.Unmarshal(in.ToolChoice, &s) == nil {
			switch s {
			case "auto":
				out.ToolChoice = &api.AnthropicToolChoice{Type: "auto"}
			case "required":
				out.ToolChoice = &api.AnthropicToolChoice{Type: "any"}
			case "none":
				out.ToolChoice = &api.AnthropicToolChoice{Type: "none"}
			}
		} else {
			var obj struct {
				Function struct{ Name string } `json:"function"`
			}
			if json.Unmarshal(in.ToolChoice, &obj) == nil && obj.Function.Name != "" {
				out.ToolChoice = &api.AnthropicToolChoice{Type: "tool", Name: obj.Function.Name}
			}
		}
	}
	if in.ParallelToolCalls != nil && !*in.ParallelToolCalls {
		if out.ToolChoice == nil {
			out.ToolChoice = &api.AnthropicToolChoice{Type: "auto"}
		}
		out.ToolChoice.DisableParallelToolUse = true
	}
	return out, nil
}

func imageSource(u *api.ImageURL) (*api.AnthropicSource, error) {
	if u == nil {
		return nil, fmt.Errorf("image part without url")
	}
	if rest, ok := strings.CutPrefix(u.URL, "data:"); ok {
		meta, data, found := strings.Cut(rest, ",")
		if !found || !strings.HasSuffix(meta, ";base64") {
			return nil, fmt.Errorf("image data URL must be base64")
		}
		return &api.AnthropicSource{Type: "base64", MediaType: strings.TrimSuffix(meta, ";base64"), Data: data}, nil
	}
	return &api.AnthropicSource{Type: "url", URL: u.URL}, nil
}

// UsageFromAnthropic maps Anthropic usage to OpenAI usage (prompt tokens include
// cache reads and writes; cached tokens are the reads).
func UsageFromAnthropic(u api.AnthropicUsage) *api.Usage {
	prompt := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	return &api.Usage{PromptTokens: prompt, CompletionTokens: u.OutputTokens, TotalTokens: prompt + u.OutputTokens,
		PromptTokensDetails: &api.PromptTokensDetails{CachedTokens: u.CacheReadInputTokens}}
}

// ResponseFromAnthropic converts an Anthropic response to canonical form.
func ResponseFromAnthropic(r *api.AnthropicResponse, raw []byte) *api.ChatResponse {
	msg := api.Message{Role: "assistant"}
	var text []string
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			text = append(text, b.Text)
		case "tool_use":
			args := string(b.Input)
			if args == "" {
				args = "{}"
			}
			msg.ToolCalls = append(msg.ToolCalls, api.ToolCall{ID: b.ID, Type: "function", Function: api.FunctionCall{Name: b.Name, Arguments: args}})
		case "thinking":
			msg.ReasoningContent += b.Thinking
		}
	}
	msg.Content = api.TextContent(strings.Join(text, ""))
	if len(text) == 0 && len(msg.ToolCalls) > 0 {
		msg.Content = api.Content{Null: true}
	}
	finish := FinishFromAnthropic(r.StopReason)
	if finish == "content_filter" {
		msg.Refusal = strings.Join(text, "")
	}
	return &api.ChatResponse{ID: r.ID, Object: "chat.completion", Model: r.Model, RawAnthropic: raw,
		Choices: []api.Choice{{Message: msg, FinishReason: finish}}, Usage: UsageFromAnthropic(r.Usage)}
}

// ResponseToAnthropic converts a canonical response for an Anthropic-format client.
func ResponseToAnthropic(r *api.ChatResponse, model string) *api.AnthropicResponse {
	out := &api.AnthropicResponse{ID: anthropicID(r.ID), Type: "message", Role: "assistant", Model: model, Content: []api.AnthropicBlock{}}
	if len(r.Choices) > 0 {
		m := r.Choices[0].Message
		if m.ReasoningContent != "" {
			out.Content = append(out.Content, api.AnthropicBlock{Type: "thinking", Thinking: m.ReasoningContent})
		}
		if t := m.Content.PlainText(); t != "" {
			out.Content = append(out.Content, api.AnthropicBlock{Type: "text", Text: t})
		}
		for _, tc := range m.ToolCalls {
			input := json.RawMessage(tc.Function.Arguments)
			if !json.Valid(input) || len(input) == 0 {
				input = json.RawMessage("{}")
			}
			out.Content = append(out.Content, api.AnthropicBlock{Type: "tool_use", ID: toolID(tc.ID), Name: tc.Function.Name, Input: input})
		}
		out.StopReason = StopToAnthropic(r.Choices[0].FinishReason)
	}
	if r.Usage != nil {
		cached := r.Usage.Cached()
		out.Usage = api.AnthropicUsage{InputTokens: r.Usage.PromptTokens - cached, OutputTokens: r.Usage.CompletionTokens, CacheReadInputTokens: cached}
	}
	return out
}

func anthropicID(id string) string {
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	return "msg_" + strings.TrimPrefix(strings.TrimPrefix(id, "chatcmpl-"), "req_")
}

// toolID keeps tool IDs usable on both sides (Anthropic accepts any [a-zA-Z0-9_-]).
func toolID(id string) string {
	if id == "" {
		return "toolu_gw"
	}
	return id
}
