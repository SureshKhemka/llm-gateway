package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/eval"
)

var refusalRe = regexp.MustCompile(`(?i)^\W*(i'?m sorry|i am sorry|sorry, (but )?i (can(no|')t|am unable)|i cannot (help|assist|comply)|i can'?t (help|assist)|i am unable to|as an ai\b)`)

// validate runs the cascade validators against a response [RTE-06]. It returns the
// first failing validator, or "".
func (e *Engine) validate(ctx context.Context, s *Snapshot, rc *RequestContext, pol *config.Policy, req *api.ChatRequest, resp *api.ChatResponse, overhead *float64) string {
	if len(resp.Choices) == 0 {
		return "non_empty"
	}
	ch := resp.Choices[0]
	text := ch.Message.Content.PlainText()
	for _, v := range pol.Cascade.Validators {
		switch v {
		case "json_schema":
			if rf := req.ResponseFormat; rf != nil && len(ch.Message.ToolCalls) == 0 {
				switch rf.Type {
				case "json_schema":
					var schema json.RawMessage
					if rf.JSONSchema != nil {
						schema = rf.JSONSchema.Schema
					}
					if !eval.ValidJSONSchema(schema, text) {
						return "json_schema"
					}
				case "json_object":
					if !eval.ValidJSONSchema(nil, text) {
						return "json_schema"
					}
				}
			}
		case "non_empty":
			if strings.TrimSpace(text) == "" && len(ch.Message.ToolCalls) == 0 {
				return "non_empty"
			}
		case "not_truncated":
			if ch.FinishReason == "length" {
				return "not_truncated"
			}
		case "no_refusal":
			if ch.Message.Refusal != "" || ch.FinishReason == "content_filter" || refusalRe.MatchString(text) {
				return "no_refusal"
			}
		case "judge":
			if pol.Cascade.Judge == "" {
				continue
			}
			score, c, err := e.judgeScore(ctx, s, rc, pol.Cascade.Judge, lastUserText(req), text)
			*overhead += c
			min := pol.Cascade.JudgeMinScore
			if min == 0 {
				min = 0.6
			}
			if err == nil && score < min {
				return "judge"
			}
		}
	}
	return ""
}

const judgeSystem = "You are a grader. Score how well RESPONSE answers REQUEST on a scale from 0 (useless) to 10 (fully correct). Reply with a single integer."

var firstInt = regexp.MustCompile(`\b(10|[0-9])\b`)

// judgeScore asks the judge deployment to score an answer in [0,1].
func (e *Engine) judgeScore(ctx context.Context, s *Snapshot, rc *RequestContext, judge, request, response string) (float64, float64, error) {
	out, c, _, err := e.chatInternal(ctx, s, rc, judge, judgeSystem, "REQUEST:\n"+request+"\n\nRESPONSE:\n"+response, 4)
	if err != nil {
		return 0, c, err
	}
	m := firstInt.FindString(out)
	if m == "" {
		return 0, c, fmt.Errorf("judge reply %q has no score", out)
	}
	n, _ := strconv.Atoi(m)
	return float64(n) / 10, c, nil
}

const classifierSystem = "You are a difficulty classifier for an LLM router. Given a user request, reply with a single digit: " +
	"0 = trivial (greeting, FAQ lookup), 1 = simple (extraction, classification, short answers), " +
	"2 = moderate (code, summarisation, multi-step arithmetic), 3 = hard (complex reasoning, debugging, design). Reply with the digit only."

// classifierEstimator asks a small model for the tier [RTE-03]. Its cost and latency
// are charged to the request and reported in the decision record [RTE-16].
func (e *Engine) classifierEstimator(s *Snapshot, rc *RequestContext, req *api.ChatRequest, pol *config.Policy, dec *Decision) (Difficulty, bool) {
	dep := pol.Difficulty.Classifier
	text := lastUserText(req)
	if len(text) > 4000 {
		text = text[:2000] + "\n…\n" + text[len(text)-2000:]
	}
	out, c, ms, err := e.chatInternal(context.Background(), s, rc, dep, classifierSystem, text, 2)
	dec.ClassifierCostUSD, dec.ClassifierMs = c, ms
	rc.overheadUSD += c
	if err != nil {
		return Difficulty{}, false
	}
	m := regexp.MustCompile(`[0-3]`).FindString(out)
	if m == "" {
		return Difficulty{}, false
	}
	tier, _ := strconv.Atoi(m)
	rules := RuleDifficulty(req, pol.Difficulty.Thresholds)
	// Hard requirements the classifier cannot see (tools, images) still lift the tier.
	if rules.Signals["tools"] > 0 || rules.Signals["vision"] > 0 {
		tier = max(tier, 1)
	}
	return Difficulty{Score: float64(tier) / 3, Tier: tier, Signals: map[string]float64{"classifier": float64(tier), "classifier_ms": ms}}, true
}
