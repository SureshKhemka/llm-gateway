package gateway

import (
	"regexp"
	"strings"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/tokens"
)

// Difficulty is an estimate of how capable a model a request needs [RTE-03].
type Difficulty struct {
	Score   float64            `json:"score"`
	Tier    int                `json:"tier"`
	Signals map[string]float64 `json:"signals"`
}

// The cue patterns run on lowercased text (cheaper than (?i)); codeSyntax needs case.
var (
	codeSyntax   = regexp.MustCompile(`(?m)(\bdef \w+\(|\bfunc \w+\(|\bclass \w+[:({]|\bimport \w+|#include|\bSELECT\b.+\bFROM\b|=>|\);\s*$|\bpublic static\b|\bconsole\.log\(|\bfn \w+\()`)
	codeAsk      = regexp.MustCompile(`\b(write|implement|refactor|debug|fix|optimi[sz]e|review|port|unit tests?)\b.{0,40}\b(code|function|class|script|program|query|bug|module|method|endpoint|regex|algorithm|tests?)\b`)
	reasonCue    = regexp.MustCompile(`\b(step[- ]by[- ]step|prove|proof|derive|explain why|reason through|trade-?offs?|analy[sz]e|evaluate|justify|compare and contrast|design an?|architect|plan (a|an|the)|how many|calculate|compute|what time|work out|solve)\b`)
	multiStep    = regexp.MustCompile(`(\bthen\b|\bafter that\b|\bnext\b|\bfinally\b|^\s*\d+[.)]\s|\bfirst\b.*\bsecond\b)`)
	numbers      = regexp.MustCompile(`\b\d+(\.\d+)?\b`)
	wordProblem  = regexp.MustCompile(`\b(how (much|many|long|far|old)|(at )?what time|total (cost|price|time|distance)|change (do|will)|per (hour|day|km|kg|month)|percent(age)?|average|ratio|km/h|interest rate)\b`)
	taskCue      = regexp.MustCompile(`\b(extract|classify|categori[sz]e|label|tag|summari[sz]e|translate|rewrite|convert|list|parse|identify|detect|sentiment)\b`)
	greetingOnly = regexp.MustCompile(`^\W*(hi|hello|hey|thanks|thank you|good (morning|afternoon|evening)|bye|ok(ay)?|cool|great|cheers)\b[\w\s!,.'?]{0,40}$`)
)

const scanWindow = 4096

// RuleDifficulty scores a request from generic signals: prompt length, code, reasoning
// cues, multi-step structure, tools, structured output and conversation depth.
func RuleDifficulty(req *api.ChatRequest, thresholds []float64) Difficulty {
	sig := map[string]float64{}
	last := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			last = req.Messages[i].Content.PlainText()
			break
		}
	}
	pt := tokens.Prompt(req)
	hasFence := strings.Contains(last, "```")
	// Instructions sit at the start or end of a prompt; scanning only those windows keeps
	// rule-based routing fast on very long documents [RTE-16].
	if len(last) > 2*scanWindow {
		last = last[:scanWindow] + "\n" + last[len(last)-scanWindow:]
	}
	lower := strings.ToLower(last)
	switch {
	case pt > 6000:
		sig["length"] = 0.4
	case pt > 2000:
		sig["length"] = 0.3
	case pt > 600:
		sig["length"] = 0.2
	case pt > 150:
		sig["length"] = 0.1
	}
	if hasFence || codeSyntax.MatchString(last) {
		sig["code"] = 0.3
	}
	if codeAsk.MatchString(lower) {
		sig["code_task"] = 0.45
	}
	if n := len(reasonCue.FindAllString(lower, 4)); n > 0 {
		sig["reasoning"] = 0.2 + 0.08*float64(n-1)
	}
	if n := len(multiStep.FindAllString(lower, 6)); n >= 2 {
		sig["multi_step"] = 0.06 * float64(min(n, 5))
	}
	// Arithmetic word problems trip up small models even when short.
	if n := len(numbers.FindAllString(lower, 12)); n >= 2 && wordProblem.MatchString(lower) {
		sig["quantitative"] = 0.45
	}
	if taskCue.MatchString(lower) {
		sig["task"] = 0.25
	}
	if len(req.Tools) > 0 {
		sig["tools"] = 0.25
	}
	if req.WantsJSONSchema() {
		sig["structured_output"] = 0.15
	}
	if req.HasImages() {
		sig["vision"] = 0.25
	}
	turns := 0
	for _, m := range req.Messages {
		if m.Role == "user" || m.Role == "assistant" {
			turns++
		}
	}
	switch {
	case turns > 12:
		sig["depth"] = 0.2
	case turns > 6:
		sig["depth"] = 0.1
	}
	score := 0.0
	for _, v := range sig {
		score += v
	}
	// The greeting pattern allows at most ~60 characters, so longer text can't match.
	if t := strings.TrimSpace(lower); len(t) <= 80 && greetingOnly.MatchString(t) && len(req.Tools) == 0 && pt < 150 {
		sig["greeting"] = -score
		score = 0
	}
	if score > 1 {
		score = 1
	}
	return Difficulty{Score: score, Tier: TierFor(score, thresholds), Signals: sig}
}

// TierFor maps a score to a tier using ascending thresholds (tier i+1 starts at t[i]).
func TierFor(score float64, thresholds []float64) int {
	tier := 0
	for i, t := range thresholds {
		if score >= t {
			tier = i + 1
		}
	}
	return tier
}
