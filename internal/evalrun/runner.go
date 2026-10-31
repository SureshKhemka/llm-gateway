// Package evalrun executes eval suites through the gateway engine, grades results and
// builds reports, recommendations and promotion decisions [EVL-02..EVL-11].
package evalrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/cache"
	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/eval"
	"github.com/SureshKhemka/llm-gateway/internal/gateway"
	"github.com/SureshKhemka/llm-gateway/internal/governance"
	"github.com/SureshKhemka/llm-gateway/internal/netguard"
	"github.com/SureshKhemka/llm-gateway/internal/store"
)

// Target is what a run evaluates: a concrete deployment, or a policy version routed
// through the gateway.
type Target struct {
	Kind    string `json:"kind"` // deployment | policy
	Name    string `json:"name"`
	Version int    `json:"version,omitempty"`
}

func (t Target) Label() string {
	if t.Kind == "policy" {
		return fmt.Sprintf("policy:%s@%d", t.Name, t.Version)
	}
	return t.Name
}

// ItemResult is one item on one target.
type ItemResult struct {
	ItemID     string  `json:"item_id"`
	Category   string  `json:"category"`
	Difficulty int     `json:"difficulty"`
	Target     string  `json:"target"`
	Score      float64 `json:"score"`
	CostUSD    float64 `json:"cost_usd"`
	LatencyMs  float64 `json:"latency_ms"`
	Served     string  `json:"served_model"`
	ServedTier int     `json:"served_tier"`
	EstTier    int     `json:"est_tier"`
	DiffScore  float64 `json:"difficulty_score"`
	RequestID  string  `json:"request_id"`
	Error      string  `json:"error,omitempty"`
}

// Run is an eval run and its results.
type Run struct {
	ID          string       `json:"id"`
	Suite       string       `json:"suite"`
	Tenant      string       `json:"tenant"`
	Project     string       `json:"project"`
	Targets     []Target     `json:"targets"`
	Baseline    string       `json:"baseline,omitempty"` // target label to compare against
	Status      string       `json:"status"`             // running | done | failed
	Error       string       `json:"error,omitempty"`
	Started     time.Time    `json:"started"`
	Finished    *time.Time   `json:"finished,omitempty"`
	Results     []ItemResult `json:"results"`
	Report      *Report      `json:"report,omitempty"`
	Concurrency int          `json:"concurrency,omitempty"`
}

// Runner executes runs.
type Runner struct {
	Engine *gateway.Engine
	mu     sync.Mutex
}

const (
	DocSuite = "eval_suite"
	DocRun   = "eval_run"
)

func (r *Runner) SaveSuite(ctx context.Context, s *eval.Suite, tenant string) error {
	b, _ := json.Marshal(s)
	return r.Engine.Store.PutDoc(ctx, store.Doc{Kind: DocSuite, ID: s.ID, Tenant: tenant, Body: b})
}

func (r *Runner) Suite(ctx context.Context, id string) (*eval.Suite, error) {
	d, err := r.Engine.Store.GetDoc(ctx, DocSuite, id)
	if err != nil {
		return nil, err
	}
	var s eval.Suite
	return &s, json.Unmarshal(d.Body, &s)
}

func (r *Runner) Save(ctx context.Context, run *Run) error {
	r.mu.Lock()
	b, _ := json.Marshal(run)
	r.mu.Unlock()
	return r.Engine.Store.PutDoc(ctx, store.Doc{Kind: DocRun, ID: run.ID, Tenant: run.Tenant, Body: b})
}

func (r *Runner) Load(ctx context.Context, id string) (*Run, error) {
	d, err := r.Engine.Store.GetDoc(ctx, DocRun, id)
	if err != nil {
		return nil, err
	}
	var run Run
	return &run, json.Unmarshal(d.Body, &run)
}

// Execute runs every item on every target, grades the answers and builds the report.
// Requests carry the run ID, so cost lands on the eval budget and records are tagged
// [EVL-02, EVL-11].
func (r *Runner) Execute(ctx context.Context, run *Run, suite *eval.Suite) error {
	s := r.Engine.Snapshot()
	t := s.Tenants[run.Tenant]
	if t == nil {
		return fmt.Errorf("unknown tenant %q", run.Tenant)
	}
	var proj *config.Project
	if run.Project != "" {
		proj = s.Projects[run.Project]
	}
	principal := &governance.Principal{Tenant: t, Project: proj, KeyID: "eval:" + run.ID, KeyPrefix: "eval", Scopes: config.Scopes{DefaultTags: []string{"eval"}}}
	conc := run.Concurrency
	if conc <= 0 {
		conc = 8
	}
	type job struct {
		t  Target
		it *eval.Item
	}
	jobs := make(chan job)
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				res := r.one(ctx, run, principal, j.t, j.it)
				r.mu.Lock()
				run.Results = append(run.Results, res)
				r.mu.Unlock()
			}
		}()
	}
	for _, tg := range run.Targets {
		for i := range suite.Items {
			jobs <- job{tg, &suite.Items[i]}
		}
	}
	close(jobs)
	wg.Wait()
	r.Engine.Writer.Flush()
	sort.Slice(run.Results, func(i, j int) bool {
		if run.Results[i].Target != run.Results[j].Target {
			return run.Results[i].Target < run.Results[j].Target
		}
		return run.Results[i].ItemID < run.Results[j].ItemID
	})
	run.Report = BuildReport(run)
	now := time.Now().UTC()
	run.Finished = &now
	run.Status = "done"
	return r.Save(ctx, run)
}

func (r *Runner) policyFor(s *gateway.Snapshot, t Target) (*config.Policy, error) {
	for _, p := range s.AllPolicy[t.Name] {
		if t.Version == 0 || p.Version == t.Version {
			p := p
			return &p, nil
		}
	}
	return nil, fmt.Errorf("policy %s version %d not found", t.Name, t.Version)
}

func (r *Runner) one(ctx context.Context, run *Run, principal *governance.Principal, t Target, it *eval.Item) ItemResult {
	res := ItemResult{ItemID: it.ID, Category: it.Category, Difficulty: it.Difficulty, Target: t.Label()}
	s := r.Engine.Snapshot()
	body := map[string]any{"messages": it.ChatMessages(), "temperature": 0}
	if len(it.Tools) > 0 {
		body["tools"] = json.RawMessage(it.Tools)
	}
	if len(it.Schema) > 0 {
		body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "answer", "schema": json.RawMessage(it.Schema)}}
	}
	if it.MaxTokens > 0 {
		body["max_tokens"] = it.MaxTokens
	}
	rc := &gateway.RequestContext{ID: gateway.NewRequestID(), Start: time.Now(), Endpoint: "eval", Principal: principal,
		EvalRun: run.ID, Hints: gateway.Hints{Tags: []string{"eval:" + run.ID}, Cache: "bypass"}}
	switch t.Kind {
	case "deployment":
		body["model"] = t.Name
	default:
		pol, err := r.policyFor(s, t)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		body["model"] = "auto"
		rc.PolicyOverride = pol
	}
	raw, _ := json.Marshal(body)
	var req api.ChatRequest
	_ = json.Unmarshal(raw, &req)
	resp, meta, err := r.Engine.Chat(ctx, rc, &req)
	res.RequestID = rc.ID
	if meta != nil {
		res.CostUSD, res.LatencyMs, res.Served = meta.CostUSD, meta.LatencyMs, meta.Deployment
		res.DiffScore = meta.Difficulty
		res.EstTier = gateway.TierFor(meta.Difficulty, thresholdsOf(rc.PolicyOverride))
		if d := s.Catalog.Deployments[meta.Deployment]; d != nil {
			res.ServedTier = d.Tier
		}
	}
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Score = r.grade(ctx, s, it, resp)
	return res
}

func thresholdsOf(p *config.Policy) []float64 {
	if p == nil || len(p.Difficulty.Thresholds) == 0 {
		return []float64{0.2, 0.45, 0.7}
	}
	return p.Difficulty.Thresholds
}

// grade scores a response with the item's grader [EVL-03, EVL-04].
func (r *Runner) grade(ctx context.Context, s *gateway.Snapshot, it *eval.Item, resp *api.ChatResponse) float64 {
	a := eval.Answer{}
	if len(resp.Choices) > 0 {
		m := resp.Choices[0].Message
		a.Text = m.Content.PlainText()
		for _, tc := range m.ToolCalls {
			a.ToolNames = append(a.ToolNames, tc.Function.Name)
			a.ToolArgs = append(a.ToolArgs, tc.Function.Arguments)
		}
	}
	if score, ok := eval.GradeSimple(it.Grader, it.Reference, it.Schema, a); ok {
		return score
	}
	cfg := s.Config.Evals
	switch it.Grader {
	case "embedding":
		if cfg.Embedder == "" {
			return 0
		}
		v, err := r.Engine.EmbedForEval(ctx, cfg.Embedder, []string{a.Text, it.Reference})
		if err != nil || len(v) != 2 {
			return 0
		}
		return math.Max(0, cache.Cosine(v[0], v[1]))
	case "llm_judge":
		if cfg.Judge == "" {
			return 0
		}
		score, err := r.Engine.JudgeForEval(ctx, cfg.Judge, it.PromptText()+rubricSuffix(it), a.Text)
		if err != nil {
			return 0
		}
		return score
	case "webhook":
		return webhookGrade(ctx, cfg, it, a)
	}
	return 0
}

func rubricSuffix(it *eval.Item) string {
	if it.Rubric == "" && it.Reference == "" {
		return ""
	}
	return "\n\nRubric: " + it.Rubric + "\nReference answer: " + it.Reference
}

// webhookGrade posts {item, answer} to the configured grader and expects {"score": 0..1}.
func webhookGrade(ctx context.Context, cfg config.Evals, it *eval.Item, a eval.Answer) float64 {
	if cfg.GraderWebhook == "" {
		return 0
	}
	b, _ := json.Marshal(map[string]any{"item": it, "answer": a.Text, "tool_calls": a.ToolNames})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, cfg.GraderWebhook, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: netguard.Transport(cfg.WebhookAllow)}).Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var out struct {
		Score float64 `json:"score"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return 0
	}
	return math.Min(1, math.Max(0, out.Score))
}

// NewRunID returns a fresh run ID.
func NewRunID() string { return "run_" + strings.TrimPrefix(gateway.NewRequestID(), "req_")[:16] }
