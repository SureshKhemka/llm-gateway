package translate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SureshKhemka/llm-gateway/internal/api"
)

// StreamDecoder turns Anthropic stream events into canonical chunks.
type StreamDecoder struct {
	ID, Model string
	Created   int64
	toolIdx   map[int]int // content block index -> tool call index
	nextTool  int
	usage     api.AnthropicUsage
	stop      string
}

func NewStreamDecoder(created int64) *StreamDecoder {
	return &StreamDecoder{Created: created, toolIdx: map[int]int{}}
}

func (d *StreamDecoder) chunk(delta api.Delta, finish *string) *api.ChatChunk {
	return &api.ChatChunk{ID: d.ID, Object: "chat.completion.chunk", Created: d.Created, Model: d.Model,
		Choices: []api.ChunkChoice{{Delta: delta, FinishReason: finish}}}
}

// Decode handles one event. It returns the chunks it produced (possibly none) and an
// error for upstream error events.
func (d *StreamDecoder) Decode(event string, data []byte) ([]*api.ChatChunk, error) {
	var ev struct {
		Type    string                 `json:"type"`
		Message *api.AnthropicResponse `json:"message"`
		Index   int                    `json:"index"`
		Block   *api.AnthropicBlock    `json:"content_block"`
		Delta   json.RawMessage        `json:"delta"`
		Usage   *api.AnthropicUsage    `json:"usage"`
		Error   *struct {
			Type, Message string
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, fmt.Errorf("bad anthropic event: %w", err)
	}
	if ev.Type == "" {
		ev.Type = event
	}
	raw := []api.RawEvent{{Event: ev.Type, Data: append(json.RawMessage(nil), data...)}}
	emit := func(c *api.ChatChunk) []*api.ChatChunk {
		c.Raw = raw
		return []*api.ChatChunk{c}
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			d.ID, d.Model = ev.Message.ID, ev.Message.Model
			d.usage = ev.Message.Usage
		}
		empty := ""
		return emit(d.chunk(api.Delta{Role: "assistant", Content: &empty}, nil)), nil
	case "content_block_start":
		if ev.Block != nil && ev.Block.Type == "tool_use" {
			idx := d.nextTool
			d.toolIdx[ev.Index] = idx
			d.nextTool++
			return emit(d.chunk(api.Delta{ToolCalls: []api.ToolCall{{Index: &idx, ID: ev.Block.ID, Type: "function",
				Function: api.FunctionCall{Name: ev.Block.Name}}}}, nil)), nil
		}
		return emit(d.chunk(api.Delta{}, nil)), nil
	case "content_block_delta":
		var delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			Thinking    string `json:"thinking"`
		}
		_ = json.Unmarshal(ev.Delta, &delta)
		switch delta.Type {
		case "text_delta":
			t := delta.Text
			return emit(d.chunk(api.Delta{Content: &t}, nil)), nil
		case "input_json_delta":
			idx := d.toolIdx[ev.Index]
			return emit(d.chunk(api.Delta{ToolCalls: []api.ToolCall{{Index: &idx, Function: api.FunctionCall{Arguments: delta.PartialJSON}}}}, nil)), nil
		case "thinking_delta":
			t := delta.Thinking
			return emit(d.chunk(api.Delta{ReasoningContent: &t}, nil)), nil
		}
		return emit(d.chunk(api.Delta{}, nil)), nil
	case "content_block_stop", "ping":
		return emit(d.chunk(api.Delta{}, nil)), nil
	case "message_delta":
		var delta struct {
			StopReason string `json:"stop_reason"`
		}
		_ = json.Unmarshal(ev.Delta, &delta)
		d.stop = delta.StopReason
		if ev.Usage != nil {
			d.usage.OutputTokens = ev.Usage.OutputTokens
			if ev.Usage.InputTokens > 0 {
				d.usage.InputTokens = ev.Usage.InputTokens
			}
		}
		f := FinishFromAnthropic(d.stop)
		c := d.chunk(api.Delta{}, &f)
		return emit(c), nil
	case "message_stop":
		c := &api.ChatChunk{ID: d.ID, Object: "chat.completion.chunk", Created: d.Created, Model: d.Model, Choices: []api.ChunkChoice{},
			Usage: UsageFromAnthropic(d.usage), Raw: raw}
		return []*api.ChatChunk{c}, nil
	case "error":
		msg := "upstream stream error"
		if ev.Error != nil {
			msg = ev.Error.Type + ": " + ev.Error.Message
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return nil, nil
}

// StreamEncoder renders canonical chunks as Anthropic stream events for an
// Anthropic-format client. Chunks carrying raw Anthropic events are replayed verbatim.
type StreamEncoder struct {
	ID, Model    string
	InputTokens  int
	started      bool
	blockOpen    bool
	blockType    string
	blockIndex   int
	toolBlocks   map[int]int // tool call index -> content block index
	OutputTokens int
	stop         string
}

func NewStreamEncoder(id, model string, inputTokens int) *StreamEncoder {
	return &StreamEncoder{ID: anthropicID(id), Model: model, InputTokens: inputTokens, toolBlocks: map[int]int{}, blockIndex: -1}
}

// Event is one SSE event to write.
type Event struct {
	Name string
	Data any
}

func (e *StreamEncoder) start() []Event {
	if e.started {
		return nil
	}
	e.started = true
	return []Event{{"message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": e.ID, "type": "message", "role": "assistant", "model": e.Model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": e.InputTokens, "output_tokens": 0}}}}}
}

func (e *StreamEncoder) closeBlock() []Event {
	if !e.blockOpen {
		return nil
	}
	e.blockOpen = false
	return []Event{{"content_block_stop", map[string]any{"type": "content_block_stop", "index": e.blockIndex}}}
}

func (e *StreamEncoder) open(typ string, block map[string]any) []Event {
	evs := e.closeBlock()
	e.blockIndex++
	e.blockOpen, e.blockType = true, typ
	return append(evs, Event{"content_block_start", map[string]any{"type": "content_block_start", "index": e.blockIndex, "content_block": block}})
}

// Encode converts one canonical chunk.
func (e *StreamEncoder) Encode(ch *api.ChatChunk) []Event {
	evs := e.start()
	for _, c := range ch.Choices {
		dl := c.Delta
		if dl.ReasoningContent != nil && *dl.ReasoningContent != "" {
			if !e.blockOpen || e.blockType != "thinking" {
				evs = append(evs, e.open("thinking", map[string]any{"type": "thinking", "thinking": ""})...)
			}
			evs = append(evs, Event{"content_block_delta", map[string]any{"type": "content_block_delta", "index": e.blockIndex,
				"delta": map[string]any{"type": "thinking_delta", "thinking": *dl.ReasoningContent}}})
		}
		if dl.Content != nil && *dl.Content != "" {
			if !e.blockOpen || e.blockType != "text" {
				evs = append(evs, e.open("text", map[string]any{"type": "text", "text": ""})...)
			}
			evs = append(evs, Event{"content_block_delta", map[string]any{"type": "content_block_delta", "index": e.blockIndex,
				"delta": map[string]any{"type": "text_delta", "text": *dl.Content}}})
		}
		for _, tc := range dl.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			if _, ok := e.toolBlocks[idx]; !ok || tc.ID != "" {
				evs = append(evs, e.open("tool_use", map[string]any{"type": "tool_use", "id": toolID(tc.ID), "name": tc.Function.Name, "input": map[string]any{}})...)
				e.toolBlocks[idx] = e.blockIndex
			}
			if tc.Function.Arguments != "" {
				evs = append(evs, Event{"content_block_delta", map[string]any{"type": "content_block_delta", "index": e.toolBlocks[idx],
					"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments}}})
			}
		}
		if c.FinishReason != nil {
			e.stop = StopToAnthropic(*c.FinishReason)
		}
	}
	return evs
}

// Finish closes the message with usage.
func (e *StreamEncoder) Finish(outputTokens int) []Event {
	evs := e.start()
	evs = append(evs, e.closeBlock()...)
	if e.stop == "" {
		e.stop = "end_turn"
	}
	evs = append(evs, Event{"message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": e.stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": outputTokens}}})
	return append(evs, Event{"message_stop", map[string]any{"type": "message_stop"}})
}

// Error renders a mid-stream failure as an Anthropic error event [RES-04].
func Error(message string) Event {
	return Event{"error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": message}}}
}

// RewriteModel replaces the model in a raw message_start event (the client sees the
// gateway's deployment ID).
func RewriteModel(ev api.RawEvent, model string) []byte {
	if ev.Event != "message_start" {
		return ev.Data
	}
	var m map[string]any
	if json.Unmarshal(ev.Data, &m) != nil {
		return ev.Data
	}
	if msg, ok := m["message"].(map[string]any); ok {
		msg["model"] = model
	}
	b, _ := json.Marshal(m)
	return b
}

// SSE formats an event for the wire.
func SSE(name string, data []byte) string {
	return "event: " + name + "\ndata: " + strings.TrimSpace(string(data)) + "\n\n"
}
