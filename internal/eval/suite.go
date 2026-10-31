package eval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
)

// Item is one labelled eval prompt [EVL-01]. It is compatible with the workload corpus.
type Item struct {
	ID         string           `json:"id"`
	Category   string           `json:"category"`
	Difficulty int              `json:"difficulty"`
	System     string           `json:"system,omitempty"`
	Prompt     string           `json:"prompt,omitempty"`
	Messages   []map[string]any `json:"messages,omitempty"`
	Images     []string         `json:"images,omitempty"`
	Tools      json.RawMessage  `json:"tools,omitempty"`
	Schema     json.RawMessage  `json:"schema,omitempty"`
	Reference  string           `json:"reference,omitempty"`
	Rubric     string           `json:"rubric,omitempty"`
	Grader     string           `json:"grader"`
	Group      string           `json:"group,omitempty"`
	Paraphrase bool             `json:"paraphrase,omitempty"`
	MaxTokens  int              `json:"max_tokens,omitempty"`
}

// Suite is a named, versioned set of items.
type Suite struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Items   []Item    `json:"items"`
	Created time.Time `json:"created"`
}

var knownGraders = map[string]bool{"exact": true, "contains": true, "regex": true, "json_schema": true, "tool_call": true,
	"non_empty": true, "embedding": true, "llm_judge": true, "webhook": true}

// ImportJSONL reads items from JSONL, validating each line [EVL-01].
func ImportJSONL(r io.Reader) ([]Item, error) {
	var items []Item
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4<<20), 64<<20)
	line := 0
	seen := map[string]bool{}
	for sc.Scan() {
		line++
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		var it Item
		if err := json.Unmarshal(b, &it); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if it.ID == "" {
			it.ID = fmt.Sprintf("item-%04d", line)
		}
		if seen[it.ID] {
			return nil, fmt.Errorf("line %d: duplicate id %q", line, it.ID)
		}
		seen[it.ID] = true
		if it.Prompt == "" && len(it.Messages) == 0 {
			return nil, fmt.Errorf("line %d: prompt or messages required", line)
		}
		if it.Grader == "" {
			it.Grader = "contains"
		}
		if !knownGraders[it.Grader] {
			return nil, fmt.Errorf("line %d: unknown grader %q", line, it.Grader)
		}
		if it.Difficulty < 0 || it.Difficulty > 3 {
			return nil, fmt.Errorf("line %d: difficulty must be 0–3", line)
		}
		items = append(items, it)
	}
	return items, sc.Err()
}

// ChatMessages renders the item as OpenAI messages.
func (it *Item) ChatMessages() []map[string]any {
	if len(it.Messages) > 0 {
		return it.Messages
	}
	var msgs []map[string]any
	if it.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": it.System})
	}
	if len(it.Images) > 0 {
		parts := []map[string]any{{"type": "text", "text": it.Prompt}}
		for _, img := range it.Images {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": img}})
		}
		return append(msgs, map[string]any{"role": "user", "content": parts})
	}
	return append(msgs, map[string]any{"role": "user", "content": it.Prompt})
}

// PromptText is the item's last user text (for judges and embeddings).
func (it *Item) PromptText() string {
	if it.Prompt != "" {
		return it.Prompt
	}
	for i := len(it.Messages) - 1; i >= 0; i-- {
		if it.Messages[i]["role"] == "user" {
			if s, ok := it.Messages[i]["content"].(string); ok {
				return s
			}
		}
	}
	return ""
}

// ---------- statistics [EVL-05] ----------

// Mean of xs.
func Mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// BootstrapCI returns the 95% percentile bootstrap interval of the mean (deterministic).
func BootstrapCI(xs []float64, resamples int, seed uint64) (lo, hi float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	if resamples <= 0 {
		resamples = 1000
	}
	r := rand.New(rand.NewPCG(seed, 99))
	means := make([]float64, resamples)
	for b := range means {
		var s float64
		for range xs {
			s += xs[r.IntN(len(xs))]
		}
		means[b] = s / float64(len(xs))
	}
	sort.Float64s(means)
	return means[int(0.025*float64(resamples))], means[int(math.Min(float64(resamples-1), 0.975*float64(resamples)))]
}

// ParetoPoint is one target's position in quality-versus-cost space.
type ParetoPoint struct {
	Target   string  `json:"target"`
	CostUSD  float64 `json:"cost_usd"`
	Quality  float64 `json:"quality"`
	Frontier bool    `json:"frontier"`
}

// Pareto marks points not dominated by any other (cheaper-or-equal and better-or-equal,
// strictly better in one).
func Pareto(points []ParetoPoint) []ParetoPoint {
	out := append([]ParetoPoint(nil), points...)
	for i := range out {
		out[i].Frontier = true
		for j := range out {
			if i == j {
				continue
			}
			if out[j].CostUSD <= out[i].CostUSD && out[j].Quality >= out[i].Quality &&
				(out[j].CostUSD < out[i].CostUSD || out[j].Quality > out[i].Quality) {
				out[i].Frontier = false
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CostUSD < out[j].CostUSD })
	return out
}

// ParetoSVG renders the quality-versus-cost chart (console and reports) [EVL-05].
func ParetoSVG(points []ParetoPoint) string {
	const w, h, pad = 640, 380, 56
	if len(points) == 0 {
		return `<svg xmlns="http://www.w3.org/2000/svg" width="640" height="80"><text x="10" y="40">no data</text></svg>`
	}
	maxC, minQ := 0.0, 100.0
	for _, p := range points {
		maxC = math.Max(maxC, p.CostUSD)
		minQ = math.Min(minQ, p.Quality)
	}
	if maxC == 0 {
		maxC = 1
	}
	minQ = math.Max(0, math.Floor(minQ/10)*10-5)
	x := func(c float64) float64 { return pad + (w-2*pad)*c/maxC }
	y := func(q float64) float64 { return h - pad - (h-2*pad)*(q-minQ)/(100-minQ) }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" role="img" aria-label="Quality versus cost">`, w, h)
	b.WriteString(`<style>text{font:12px system-ui,sans-serif;fill:currentColor}.f{fill:#2563eb}.d{fill:#94a3b8}.l{stroke:#2563eb;stroke-width:2;fill:none}.a{stroke:currentColor;stroke-opacity:.4}</style>`)
	fmt.Fprintf(&b, `<line class="a" x1="%d" y1="%d" x2="%d" y2="%d"/><line class="a" x1="%d" y1="%d" x2="%d" y2="%d"/>`, pad, h-pad, w-pad, h-pad, pad, pad, pad, h-pad)
	fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="middle">cost (USD) →</text><text x="14" y="%d" transform="rotate(-90 14 %d)" text-anchor="middle">quality →</text>`, w/2, h-14, h/2, h/2)
	fmt.Fprintf(&b, `<text x="%d" y="%d">0</text><text x="%d" y="%d" text-anchor="end">$%.4f</text>`, pad, h-pad+16, w-pad, h-pad+16, maxC)
	fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="end">%.0f</text><text x="%d" y="%d" text-anchor="end">100</text>`, pad-6, h-pad, minQ, pad-6, pad+4)
	var path []string
	for _, p := range points {
		if p.Frontier {
			path = append(path, fmt.Sprintf("%.1f,%.1f", x(p.CostUSD), y(p.Quality)))
		}
	}
	if len(path) > 1 {
		fmt.Fprintf(&b, `<polyline class="l" points="%s"/>`, strings.Join(path, " "))
	}
	for _, p := range points {
		cls := "d"
		if p.Frontier {
			cls = "f"
		}
		fmt.Fprintf(&b, `<circle class="%s" cx="%.1f" cy="%.1f" r="5"><title>%s: $%.5f, %.1f</title></circle>`, cls, x(p.CostUSD), y(p.Quality), escape(p.Target), p.CostUSD, p.Quality)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f">%s</text>`, x(p.CostUSD)+7, y(p.Quality)-7, escape(p.Target))
	}
	b.WriteString(`</svg>`)
	return b.String()
}

func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
