package gateway

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"math/rand/v2"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/store"
)

// splitPolicy picks an A/B variant, sticky by end user, then session, then request
// [RTE-12].
func splitPolicy(s *Snapshot, rc *RequestContext, pol *config.Policy) (*config.Policy, string) {
	if len(pol.Split) == 0 || rc.PolicyOverride != nil {
		return pol, ""
	}
	total := 0
	for _, sp := range pol.Split {
		total += max(0, sp.Weight)
	}
	if total == 0 {
		return pol, ""
	}
	key := rc.Hints.EndUser
	if key == "" {
		key = rc.Hints.Session
	}
	if key == "" {
		key = rc.ID
	}
	h := fnv.New32a()
	h.Write([]byte(pol.Name + "|" + key))
	n := int(h.Sum32() % uint32(total))
	for _, sp := range pol.Split {
		n -= max(0, sp.Weight)
		if n < 0 {
			if sp.Policy == pol.Name {
				return pol, sp.Policy
			}
			if v, ok := s.Policies[sp.Policy]; ok {
				return v, sp.Policy
			}
			return pol, ""
		}
	}
	return pol, ""
}

// maybeShadow mirrors a sampled share of successful requests to a candidate policy or
// deployment in the background. The result is recorded (cost, latency, decision) and
// charged to the shadow budget, but never returned [RTE-13].
func (e *Engine) maybeShadow(rc *RequestContext, req *api.ChatRequest, pol *config.Policy) {
	if pol == nil || pol.Shadow == nil || rc.Shadow || rc.EvalRun != "" || rc.Explain {
		return
	}
	sh := pol.Shadow
	if sh.SampleRate <= 0 || rand.Float64() >= sh.SampleRate {
		return
	}
	s := e.Snapshot()
	shadowReq := req.Clone()
	src := &RequestContext{ID: NewRequestID(), Start: time.Now(), Endpoint: rc.Endpoint, Format: rc.Format, Principal: rc.Principal,
		Hints: rc.Hints, Shadow: true, TraceID: rc.TraceID}
	src.Hints.Cache = "bypass"
	src.Hints.Tags = append(append([]string(nil), rc.Hints.Tags...), "shadow-of:"+rc.ID)
	switch {
	case sh.Deployment != "":
		shadowReq.Model = sh.Deployment
	case sh.Policy != "":
		p, ok := s.Policies[sh.Policy]
		if !ok {
			return
		}
		shadowReq.Model = "auto"
		src.PolicyOverride = p
	default:
		return
	}
	shadowReq.Stream = false
	e.shadows.Add(1)
	go func() {
		defer e.shadows.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _, _ = e.Chat(ctx, src, shadowReq)
	}()
}

// WaitShadows blocks until background shadow requests finish (tests, shutdown).
func (e *Engine) WaitShadows() { e.shadows.Wait() }

// maybeGradeOnline asynchronously judges a sampled share of production answers and
// stores the score for quality-by-route reports [EVL-09].
func (e *Engine) maybeGradeOnline(s *Snapshot, rc *RequestContext, req *api.ChatRequest, resp *api.ChatResponse, meta *Meta) {
	cfg := s.Config.Evals
	if cfg.OnlineSampleRate <= 0 || cfg.Judge == "" || rc.Shadow || rc.EvalRun != "" || rand.Float64() >= cfg.OnlineSampleRate {
		return
	}
	question := lastUserText(req)
	answer, _ := responseText(resp)
	tenant := rc.Principal.TenantID()
	id, dep, policy := rc.ID, meta.Deployment, meta.Policy
	e.shadows.Add(1)
	go func() {
		defer e.shadows.Done()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		score, err := e.JudgeForEval(ctx, cfg.Judge, question, answer)
		if err != nil {
			return
		}
		b, _ := json.Marshal(map[string]any{"request_id": id, "deployment": dep, "policy": policy, "score": score, "source": "judge"})
		_ = e.Store.PutDoc(ctx, store.Doc{Kind: "quality_signal", ID: "judge:" + id, Tenant: tenant, Body: b})
	}()
}
