// Package tokens estimates token counts when a provider omits usage [API-13].
// The estimate (≈4 characters or 0.75 words per token, whichever is larger) is within
// ~15% of BPE tokenizers on English and code; estimated counts are always flagged.
package tokens

import (
	"unicode/utf8"

	"github.com/SureshKhemka/llm-gateway/internal/api"
)

// Count estimates the tokens in s.
func Count(s string) int {
	if s == "" {
		return 0
	}
	byChars := (utf8.RuneCountInString(s) + 3) / 4
	byWords := (words(s)*4 + 2) / 3
	if byWords > byChars {
		return byWords
	}
	return byChars
}

// words counts whitespace-separated words without allocating.
func words(s string) int {
	n, in := 0, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		space := c == ' ' || c == '\n' || c == '\t' || c == '\r'
		if !space && !in {
			n++
		}
		in = !space
	}
	return n
}

const perMessageOverhead = 4

// imageTokens is a flat estimate per image part (OpenAI low-detail tiles are 85).
const imageTokens = 85

// Prompt estimates the prompt tokens of a chat request, including tool schemas.
func Prompt(r *api.ChatRequest) int {
	n := 3
	for _, m := range r.Messages {
		n += perMessageOverhead + Count(m.Content.PlainText())
		for _, p := range m.Content.Parts {
			if p.Type == "image_url" {
				n += imageTokens
			}
		}
		for _, tc := range m.ToolCalls {
			n += Count(tc.Function.Name) + Count(tc.Function.Arguments)
		}
	}
	for _, t := range r.Tools {
		n += Count(t.Function.Name) + Count(t.Function.Description) + Count(string(t.Function.Parameters))
	}
	if r.ResponseFormat != nil && r.ResponseFormat.JSONSchema != nil {
		n += Count(string(r.ResponseFormat.JSONSchema.Schema))
	}
	return n
}

// Completion estimates tokens in generated text and tool calls.
func Completion(text string, calls []api.ToolCall) int {
	n := Count(text)
	for _, c := range calls {
		n += Count(c.Function.Name) + Count(c.Function.Arguments)
	}
	return n
}
