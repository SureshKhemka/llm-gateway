package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"sort"
	"sync"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/cache"
	"github.com/SureshKhemka/llm-gateway/internal/config"
)

// labelledExample is one prompt with a known difficulty tier.
type labelledExample struct {
	Prompt     string `json:"prompt"`
	Difficulty int    `json:"difficulty"`
	vec        []float32
}

type exampleSet struct {
	once     sync.Once
	examples []labelledExample
	err      error
}

var exampleSets sync.Map // file|embedder -> *exampleSet

// similarityEstimator scores difficulty by k-nearest labelled examples [RTE-04].
func (e *Engine) similarityEstimator(s *Snapshot, rc *RequestContext, req *api.ChatRequest, pol *config.Policy, dec *Decision) (Difficulty, bool) {
	file, emb := pol.Difficulty.Examples, pol.Difficulty.Embedder
	if emb == "" {
		emb = s.Config.Cache.Embedder
	}
	if file == "" || emb == "" {
		return Difficulty{}, false
	}
	v, _ := exampleSets.LoadOrStore(file+"|"+emb, &exampleSet{})
	set := v.(*exampleSet)
	set.once.Do(func() { set.examples, set.err = e.loadExamples(s, rc, file, emb) })
	if set.err != nil || len(set.examples) == 0 {
		return Difficulty{}, false
	}
	vecs, c, err := e.embed(context.Background(), s, rc, emb, []string{lastUserText(req)})
	rc.overheadUSD += c
	if err != nil || len(vecs) == 0 {
		return Difficulty{}, false
	}
	type scored struct {
		sim  float64
		tier int
	}
	sc := make([]scored, len(set.examples))
	for i, ex := range set.examples {
		sc[i] = scored{cache.Cosine(vecs[0], ex.vec), ex.Difficulty}
	}
	sort.Slice(sc, func(i, j int) bool { return sc[i].sim > sc[j].sim })
	k := min(5, len(sc))
	votes := map[int]float64{}
	for _, x := range sc[:k] {
		votes[x.tier] += max(0, x.sim)
	}
	best, bestW := 0, -1.0
	for t, w := range votes {
		if w > bestW || w == bestW && t > best {
			best, bestW = t, w
		}
	}
	rules := RuleDifficulty(req, pol.Difficulty.Thresholds)
	if rules.Signals["tools"] > 0 || rules.Signals["vision"] > 0 {
		best = max(best, 1)
	}
	return Difficulty{Score: float64(best) / 3, Tier: best, Signals: map[string]float64{"knn_tier": float64(best), "knn_top_similarity": sc[0].sim}}, true
}

func (e *Engine) loadExamples(s *Snapshot, rc *RequestContext, file, emb string) ([]labelledExample, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var exs []labelledExample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4<<20), 64<<20)
	for sc.Scan() {
		var ex labelledExample
		if json.Unmarshal(sc.Bytes(), &ex) == nil && ex.Prompt != "" {
			exs = append(exs, ex)
		}
	}
	for i := 0; i < len(exs); i += 64 {
		batch := exs[i:min(i+64, len(exs))]
		texts := make([]string, len(batch))
		for j, ex := range batch {
			texts[j] = ex.Prompt
		}
		vecs, _, err := e.embed(context.Background(), s, rc, emb, texts)
		if err != nil {
			return nil, err
		}
		for j := range batch {
			batch[j].vec = vecs[j]
		}
	}
	return exs, nil
}
