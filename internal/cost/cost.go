// Package cost computes per-request cost from token usage and the price card in effect
// [CST-01, CST-02, CST-03].
package cost

import (
	"math"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/catalog"
)

// Usage is token usage split the way price cards are [CST-01].
type Usage struct {
	Input       int  `json:"input_tokens"`        // uncached input tokens
	CachedInput int  `json:"cached_input_tokens"` // input tokens served from provider cache
	Output      int  `json:"output_tokens"`       // visible output tokens
	Reasoning   int  `json:"reasoning_tokens"`    // reasoning tokens (subset billed at reasoning price)
	Estimated   bool `json:"estimated"`
}

func (u Usage) TotalInput() int  { return u.Input + u.CachedInput }
func (u Usage) TotalOutput() int { return u.Output + u.Reasoning }

func (u Usage) Add(o Usage) Usage {
	return Usage{
		Input: u.Input + o.Input, CachedInput: u.CachedInput + o.CachedInput,
		Output: u.Output + o.Output, Reasoning: u.Reasoning + o.Reasoning,
		Estimated: u.Estimated || o.Estimated,
	}
}

// Breakdown is a cost split by token class, in USD.
type Breakdown struct {
	Input       float64 `json:"input_usd"`
	CachedInput float64 `json:"cached_input_usd"`
	Output      float64 `json:"output_usd"`
	Reasoning   float64 `json:"reasoning_usd"`
}

func (b Breakdown) Total() float64 { return b.Input + b.CachedInput + b.Output + b.Reasoning }

// Of computes the cost of u on deployment d at time t.
func Of(d *catalog.Deployment, t time.Time, u Usage) Breakdown {
	p := d.PriceAt(t)
	return Breakdown{
		Input:       float64(u.Input) * p.Input / 1e6,
		CachedInput: float64(u.CachedInput) * p.CachedInput / 1e6,
		Output:      float64(u.Output) * p.Output / 1e6,
		Reasoning:   float64(u.Reasoning) * p.Reasoning / 1e6,
	}
}

// Estimate is the expected cost of a request before it runs: input at the input price
// (the cached share at the cached price, RTE-17) plus expected output.
func Estimate(d *catalog.Deployment, t time.Time, inputTokens, cachedShare, outputTokens int) float64 {
	p := d.PriceAt(t)
	uncached := inputTokens - cachedShare
	if uncached < 0 {
		uncached = 0
	}
	return (float64(uncached)*p.Input + float64(cachedShare)*p.CachedInput + float64(outputTokens)*p.Output) / 1e6
}

// WorstCase is the reservation amount for BUD-03: input tokens × input price plus max
// output tokens × the larger of output and reasoning prices.
func WorstCase(d *catalog.Deployment, t time.Time, inputTokens, maxOutput int) float64 {
	p := d.PriceAt(t)
	out := math.Max(p.Output, p.Reasoning)
	return (float64(inputTokens)*p.Input + float64(maxOutput)*out) / 1e6
}

// Nano converts USD to integer nano-dollars for exact budget arithmetic.
func Nano(usd float64) int64 { return int64(math.Round(usd * 1e9)) }

// FromNano converts nano-dollars to USD.
func FromNano(n int64) float64 { return float64(n) / 1e9 }
