package evalrun

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/SureshKhemka/llm-gateway/internal/cache"
	"github.com/SureshKhemka/llm-gateway/internal/eval"
	"github.com/SureshKhemka/llm-gateway/internal/gateway"
)

type CategorySummary struct {
	Items   int     `json:"items"`
	Quality float64 `json:"quality"`
	CostUSD float64 `json:"cost_usd"`
}

type Delta struct {
	Quality float64 `json:"quality_points"`
	Lo      float64 `json:"ci_low"`
	Hi      float64 `json:"ci_high"`
	CostPct float64 `json:"cost_change_pct"`
}

type TargetSummary struct {
	Target     string                     `json:"target"`
	Items      int                        `json:"items"`
	Errors     int                        `json:"errors"`
	Quality    float64                    `json:"quality"`
	QualityLo  float64                    `json:"quality_ci_low"`
	QualityHi  float64                    `json:"quality_ci_high"`
	CostUSD    float64                    `json:"cost_usd"`
	P50Ms      float64                    `json:"p50_ms"`
	P95Ms      float64                    `json:"p95_ms"`
	ByCategory map[string]CategorySummary `json:"by_category"`
	VsBaseline *Delta                     `json:"vs_baseline,omitempty"`
}

// Report is the run report [EVL-05, EVL-06].
type Report struct {
	Baseline  string                        `json:"baseline,omitempty"`
	Targets   []TargetSummary               `json:"targets"`
	Pareto    []eval.ParetoPoint            `json:"pareto"`
	Confusion map[string]map[string]int     `json:"difficulty_confusion,omitempty"` // "label→estimated" counts per policy target
	Accuracy  map[string]float64            `json:"estimator_accuracy,omitempty"`
	ByCat     map[string]map[string]float64 `json:"quality_by_category"`
}

func byTarget(run *Run) map[string][]ItemResult {
	out := map[string][]ItemResult{}
	for _, r := range run.Results {
		out[r.Target] = append(out[r.Target], r)
	}
	return out
}

// BuildReport summarises a run.
func BuildReport(run *Run) *Report {
	rep := &Report{Baseline: run.Baseline, Confusion: map[string]map[string]int{}, Accuracy: map[string]float64{}, ByCat: map[string]map[string]float64{}}
	groups := byTarget(run)
	labels := make([]string, 0, len(groups))
	for l := range groups {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	scoreOf := map[string]map[string]float64{}
	for _, l := range labels {
		rs := groups[l]
		ts := TargetSummary{Target: l, Items: len(rs), ByCategory: map[string]CategorySummary{}}
		scores := make([]float64, 0, len(rs))
		lats := make([]float64, 0, len(rs))
		scoreOf[l] = map[string]float64{}
		policy := len(l) > 7 && l[:7] == "policy:"
		conf := map[string]int{}
		right := 0
		for _, r := range rs {
			if r.Error != "" {
				ts.Errors++
			}
			scores = append(scores, 100*r.Score)
			lats = append(lats, r.LatencyMs)
			ts.CostUSD += r.CostUSD
			scoreOf[l][r.ItemID] = 100 * r.Score
			c := ts.ByCategory[r.Category]
			c.Items++
			c.Quality += 100 * r.Score
			c.CostUSD += r.CostUSD
			ts.ByCategory[r.Category] = c
			if policy {
				conf[fmt.Sprintf("%d→%d", r.Difficulty, r.EstTier)]++
				if r.EstTier == r.Difficulty {
					right++
				}
			}
		}
		for k, c := range ts.ByCategory {
			c.Quality /= float64(c.Items)
			ts.ByCategory[k] = c
			if rep.ByCat[k] == nil {
				rep.ByCat[k] = map[string]float64{}
			}
			rep.ByCat[k][l] = c.Quality
		}
		ts.Quality = eval.Mean(scores)
		ts.QualityLo, ts.QualityHi = eval.BootstrapCI(scores, 1000, 7)
		sort.Float64s(lats)
		if n := len(lats); n > 0 {
			ts.P50Ms, ts.P95Ms = lats[n/2], lats[min(n-1, n*95/100)]
		}
		if policy {
			rep.Confusion[l] = conf
			rep.Accuracy[l] = float64(right) / float64(max(1, len(rs)))
		}
		rep.Targets = append(rep.Targets, ts)
		rep.Pareto = append(rep.Pareto, eval.ParetoPoint{Target: l, CostUSD: ts.CostUSD, Quality: ts.Quality})
	}
	rep.Pareto = eval.Pareto(rep.Pareto)
	if b, ok := scoreOf[run.Baseline]; ok {
		var bcost float64
		for _, t := range rep.Targets {
			if t.Target == run.Baseline {
				bcost = t.CostUSD
			}
		}
		for i := range rep.Targets {
			t := &rep.Targets[i]
			if t.Target == run.Baseline {
				continue
			}
			var diffs []float64
			for item, s := range scoreOf[t.Target] {
				if bs, ok := b[item]; ok {
					diffs = append(diffs, s-bs)
				}
			}
			d := &Delta{Quality: eval.Mean(diffs)}
			d.Lo, d.Hi = eval.BootstrapCI(diffs, 1000, 11)
			if bcost > 0 {
				d.CostPct = 100 * (t.CostUSD - bcost) / bcost
			}
			t.VsBaseline = d
		}
	}
	return rep
}

// Recommendation is a suggested policy change from a run [EVL-07].
type Recommendation struct {
	Policy            string    `json:"policy"`
	TargetQuality     float64   `json:"target_quality"`
	Current           []float64 `json:"current_thresholds"`
	CurrentQuality    float64   `json:"current_predicted_quality"`
	CurrentCostUSD    float64   `json:"current_predicted_cost_usd"`
	Thresholds        []float64 `json:"recommended_thresholds"`
	PredictedQuality  float64   `json:"predicted_quality"`
	PredictedCostUSD  float64   `json:"predicted_cost_usd"`
	SemanticThreshold *float64  `json:"semantic_threshold,omitempty"`
	SemanticRecall    float64   `json:"semantic_recall,omitempty"`
	Note              string    `json:"note,omitempty"`
}

type tierOutcome struct{ score, cost float64 }

// Recommend grid-searches difficulty thresholds that minimise predicted cost while the
// predicted quality stays at or above targetQuality. It needs the run to contain one or
// more deployments per tier (direct targets) and a rule-based policy target that records
// each item's difficulty score.
func Recommend(run *Run, s *gateway.Snapshot, policy string, current []float64, targetQuality float64) (*Recommendation, error) {
	groups := byTarget(run)
	// Per tier, the cheapest deployment (by total cost) among direct targets.
	type depAgg struct {
		label string
		tier  int
		cost  float64
	}
	best := map[int]depAgg{}
	for label, rs := range groups {
		d := s.Catalog.Deployments[label]
		if d == nil {
			continue
		}
		var c float64
		for _, r := range rs {
			c += r.CostUSD
		}
		if cur, ok := best[d.Tier]; !ok || c < cur.cost {
			best[d.Tier] = depAgg{label, d.Tier, c}
		}
	}
	if len(best) < 2 {
		return nil, fmt.Errorf("the run needs direct deployment targets on at least two tiers")
	}
	// outcome[item][tier]
	outcome := map[string]map[int]tierOutcome{}
	for tier, agg := range best {
		for _, r := range groups[agg.label] {
			if r.Error != "" {
				continue
			}
			if outcome[r.ItemID] == nil {
				outcome[r.ItemID] = map[int]tierOutcome{}
			}
			outcome[r.ItemID][tier] = tierOutcome{100 * r.Score, r.CostUSD}
		}
	}
	scores := map[string]float64{}
	for label, rs := range groups {
		if len(label) > 7 && label[:7] == "policy:" {
			for _, r := range rs {
				scores[r.ItemID] = r.DiffScore
			}
			break
		}
	}
	if len(scores) == 0 {
		return nil, fmt.Errorf("the run needs a policy target to record difficulty scores")
	}
	predict := func(th []float64) (float64, float64) {
		var q, c float64
		n := 0
		for item, sc := range scores {
			outs := outcome[item]
			if len(outs) == 0 {
				continue
			}
			tier := gateway.TierFor(sc, th)
			var o tierOutcome
			found := false
			for t := tier; t <= 3; t++ { // not servable at that tier: the router goes up
				if v, ok := outs[t]; ok {
					o, found = v, true
					break
				}
			}
			if !found {
				for t := tier - 1; t >= 0 && !found; t-- {
					if v, ok := outs[t]; ok {
						o, found = v, true
					}
				}
			}
			q += o.score
			c += o.cost
			n++
		}
		if n == 0 {
			return 0, 0
		}
		return q / float64(n), c
	}
	rec := &Recommendation{Policy: policy, TargetQuality: targetQuality, Current: current}
	rec.CurrentQuality, rec.CurrentCostUSD = predict(current)
	bestCost := math.Inf(1)
	grid := []float64{}
	for v := 0.05; v < 0.96; v += 0.05 {
		grid = append(grid, math.Round(v*100)/100)
	}
	for i, a := range grid {
		for j := i + 1; j < len(grid); j++ {
			for k := j + 1; k < len(grid); k++ {
				th := []float64{a, grid[j], grid[k]}
				q, c := predict(th)
				if q+1e-9 >= targetQuality && (c < bestCost-1e-12 || c == bestCost && q > rec.PredictedQuality) {
					bestCost = c
					rec.Thresholds, rec.PredictedQuality, rec.PredictedCostUSD = th, q, c
				}
			}
		}
	}
	if rec.Thresholds == nil {
		rec.Thresholds, rec.PredictedQuality, rec.PredictedCostUSD = current, rec.CurrentQuality, rec.CurrentCostUSD
		rec.Note = "no threshold setting reaches the target quality; keeping the current thresholds"
	}
	return rec, nil
}

// RecommendSemanticThreshold picks the lowest cosine threshold at which no two
// different-answer prompts sharing salient entities would collide, and reports recall
// on paraphrase pairs [EVL-07, CCH-02].
func RecommendSemanticThreshold(ctx context.Context, e *gateway.Engine, embedder string, items []eval.Item) (float64, float64, error) {
	var texts []string
	for _, it := range items {
		texts = append(texts, it.PromptText())
	}
	vecs, err := e.EmbedForEval(ctx, embedder, texts)
	if err != nil {
		return 0, 0, err
	}
	group := func(it eval.Item) string {
		if it.Group != "" {
			return it.Group
		}
		return it.ID
	}
	var pos []float64
	maxNeg := 0.0
	for i := range items {
		for j := i + 1; j < len(items); j++ {
			if cache.Salient(texts[i]) != cache.Salient(texts[j]) {
				continue // the entity guard already separates them
			}
			sim := cache.Cosine(vecs[i], vecs[j])
			if group(items[i]) == group(items[j]) {
				pos = append(pos, sim)
			} else if items[i].Reference != items[j].Reference {
				maxNeg = math.Max(maxNeg, sim)
			}
		}
	}
	th := math.Min(0.99, math.Max(0.5, math.Ceil((maxNeg+0.005)*100)/100))
	hit := 0
	for _, p := range pos {
		if p >= th {
			hit++
		}
	}
	recall := 0.0
	if len(pos) > 0 {
		recall = float64(hit) / float64(len(pos))
	}
	return th, recall, nil
}

// QualityOf returns a target's quality in a finished run, if present.
func QualityOf(run *Run, label string) (float64, bool) {
	if run.Report == nil {
		return 0, false
	}
	for _, t := range run.Report.Targets {
		if t.Target == label {
			return t.Quality, true
		}
	}
	return 0, false
}
