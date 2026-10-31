package workload

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/eval"
)

// Tenant is one tenant in the replay mix.
type Tenant struct {
	Key    string
	Weight int
}

// ReplayOptions control a corpus replay.
type ReplayOptions struct {
	BaseURL     string
	Model       string
	Tenants     []Tenant
	RPS         float64 // 0 = as fast as Concurrency allows
	Concurrency int
	RepeatRate  float64 // share of extra exact repeats appended to the plan
	Seed        uint64
	Headers     map[string]string
	Client      *http.Client
	Stream      bool
}

// Result is one replayed request.
type Result struct {
	ItemID      string  `json:"item_id"`
	Category    string  `json:"category"`
	Difficulty  int     `json:"difficulty"`
	Repeat      bool    `json:"repeat,omitempty"`
	Paraphrase  bool    `json:"paraphrase,omitempty"`
	Status      int     `json:"status"`
	ErrorCode   string  `json:"error_code,omitempty"`
	RequestID   string  `json:"request_id"`
	Served      string  `json:"served_model"`
	RouteReason string  `json:"route_reason"`
	Cache       string  `json:"cache"`
	CostUSD     float64 `json:"cost_usd"`
	BaselineUSD float64 `json:"baseline_cost_usd"`
	InputTokens int     `json:"input_tokens"`
	OutTokens   int     `json:"output_tokens"`
	LatencyMs   float64 `json:"latency_ms"`
	Score       float64 `json:"score"`
	Fallbacks   int     `json:"fallbacks"`
}

// Summary aggregates a replay.
type Summary struct {
	Model       string                     `json:"model"`
	Requests    int                        `json:"requests"`
	Errors      int                        `json:"errors"`
	CostUSD     float64                    `json:"cost_usd"`
	BaselineUSD float64                    `json:"baseline_cost_usd"`
	Quality     float64                    `json:"quality"` // mean score × 100
	CacheHits   int                        `json:"cache_hits"`
	CacheRate   float64                    `json:"cache_hit_rate"`
	P50Ms       float64                    `json:"p50_ms"`
	P95Ms       float64                    `json:"p95_ms"`
	ByCategory  map[string]CategorySummary `json:"by_category"`
	ByModel     map[string]int             `json:"by_served_model"`
	ByTier      map[string]map[string]int  `json:"difficulty_vs_served"`
}

type CategorySummary struct {
	Requests int     `json:"requests"`
	Quality  float64 `json:"quality"`
	CostUSD  float64 `json:"cost_usd"`
}

// Plan returns the replay order: every item once plus RepeatRate × N exact repeats,
// shuffled deterministically.
func Plan(items []Item, repeatRate float64, seed uint64) []planned {
	r := rand.New(rand.NewPCG(seed, 7))
	out := make([]planned, 0, len(items))
	for i := range items {
		out = append(out, planned{idx: i})
	}
	extra := int(float64(len(items)) * repeatRate)
	for k := 0; k < extra; k++ {
		out = append(out, planned{idx: r.IntN(len(items)), repeat: true})
	}
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	// A repeat must come after its first occurrence to be a cache candidate.
	seen := map[int]bool{}
	for i := range out {
		if seen[out[i].idx] {
			out[i].repeat = true
		} else {
			out[i].repeat = false
			seen[out[i].idx] = true
		}
	}
	return out
}

type planned struct {
	idx    int
	repeat bool
}

// Replay sends the corpus to the gateway and grades every answer.
func Replay(ctx context.Context, items []Item, o ReplayOptions) ([]Result, Summary, error) {
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 10 * time.Minute}
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 8
	}
	if len(o.Tenants) == 0 {
		return nil, Summary{}, fmt.Errorf("at least one tenant key is required")
	}
	plan := Plan(items, o.RepeatRate, o.Seed)
	weights := 0
	for _, t := range o.Tenants {
		weights += max(1, t.Weight)
	}
	results := make([]Result, len(plan))
	var limiter <-chan time.Time
	if o.RPS > 0 {
		tk := time.NewTicker(time.Duration(float64(time.Second) / o.RPS))
		defer tk.Stop()
		limiter = tk.C
	}
	sem := make(chan struct{}, o.Concurrency)
	var wg sync.WaitGroup
	r := rand.New(rand.NewPCG(o.Seed, 11))
	for i, pl := range plan {
		if ctx.Err() != nil {
			break
		}
		if limiter != nil {
			<-limiter
		}
		pick := r.IntN(weights)
		key := o.Tenants[0].Key
		for _, t := range o.Tenants {
			pick -= max(1, t.Weight)
			if pick < 0 {
				key = t.Key
				break
			}
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, pl planned, key string) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = one(ctx, &items[pl.idx], pl.repeat, key, o)
		}(i, pl, key)
	}
	wg.Wait()
	return results, Summarize(o.Model, results), nil
}

func one(ctx context.Context, it *Item, repeat bool, key string, o ReplayOptions) Result {
	res := Result{ItemID: it.ID, Category: it.Category, Difficulty: it.Difficulty, Repeat: repeat, Paraphrase: it.Paraphrase}
	body := it.Body(o.Model)
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", o.BaseURL+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Gw-Tags", "replay,"+it.Category)
	for k, v := range o.Headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := o.Client.Do(req)
	if err != nil {
		res.Status, res.ErrorCode = 0, err.Error()
		return res
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	res.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
	res.Status = resp.StatusCode
	h := resp.Header
	res.RequestID, res.Served, res.RouteReason, res.Cache = h.Get("X-Request-Id"), h.Get("X-Gw-Served-Model"), h.Get("X-Gw-Route-Reason"), h.Get("X-Gw-Cache")
	res.CostUSD, _ = strconv.ParseFloat(h.Get("X-Gw-Cost-Usd"), 64)
	res.BaselineUSD, _ = strconv.ParseFloat(h.Get("X-Gw-Baseline-Cost-Usd"), 64)
	res.InputTokens, _ = strconv.Atoi(h.Get("X-Gw-Input-Tokens"))
	res.OutTokens, _ = strconv.Atoi(h.Get("X-Gw-Output-Tokens"))
	res.Fallbacks, _ = strconv.Atoi(h.Get("X-Gw-Fallback-Attempts"))
	if resp.StatusCode != 200 {
		res.ErrorCode = h.Get("X-Gw-Error-Code")
		return res
	}
	res.Score = GradeResponse(it, raw)
	return res
}

// GradeResponse scores a chat completion body against the item.
func GradeResponse(it *Item, body []byte) float64 {
	var cr struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					Function struct{ Name, Arguments string } `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &cr) != nil || len(cr.Choices) == 0 {
		return 0
	}
	m := cr.Choices[0].Message
	a := eval.Answer{}
	if m.Content != nil {
		a.Text = *m.Content
	}
	for _, tc := range m.ToolCalls {
		a.ToolNames = append(a.ToolNames, tc.Function.Name)
		a.ToolArgs = append(a.ToolArgs, tc.Function.Arguments)
	}
	s, _ := eval.GradeSimple(it.Grader, it.Reference, it.Schema, a)
	return s
}

// Summarize aggregates results.
func Summarize(model string, rs []Result) Summary {
	s := Summary{Model: model, ByCategory: map[string]CategorySummary{}, ByModel: map[string]int{}, ByTier: map[string]map[string]int{}}
	var lats []float64
	var scoreSum float64
	for _, r := range rs {
		s.Requests++
		if r.Status != 200 {
			s.Errors++
		}
		s.CostUSD += r.CostUSD
		s.BaselineUSD += r.BaselineUSD
		scoreSum += r.Score
		if r.Cache == "exact_hit" || r.Cache == "semantic_hit" {
			s.CacheHits++
		}
		lats = append(lats, r.LatencyMs)
		c := s.ByCategory[r.Category]
		c.Requests++
		c.Quality += r.Score
		c.CostUSD += r.CostUSD
		s.ByCategory[r.Category] = c
		s.ByModel[r.Served]++
		d := fmt.Sprintf("d%d", r.Difficulty)
		if s.ByTier[d] == nil {
			s.ByTier[d] = map[string]int{}
		}
		s.ByTier[d][r.Served]++
	}
	if s.Requests > 0 {
		s.Quality = 100 * scoreSum / float64(s.Requests)
		s.CacheRate = float64(s.CacheHits) / float64(s.Requests)
	}
	for k, c := range s.ByCategory {
		c.Quality = 100 * c.Quality / float64(c.Requests)
		s.ByCategory[k] = c
	}
	sort.Float64s(lats)
	if n := len(lats); n > 0 {
		s.P50Ms = lats[n/2]
		s.P95Ms = lats[min(n-1, n*95/100)]
	}
	return s
}
