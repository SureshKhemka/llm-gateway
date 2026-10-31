package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/cache"
	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/cost"
	"github.com/SureshKhemka/llm-gateway/internal/store"
	"github.com/SureshKhemka/llm-gateway/internal/tokens"
)

// Cache statuses reported in X-Gw-Cache [API-09].
const (
	CacheExactHit    = "exact_hit"
	CacheSemanticHit = "semantic_hit"
	CacheMiss        = "miss"
	CacheBypass      = "bypass"
)

// cacheLookup is the state of one request's cache interaction.
type cacheLookup struct {
	ns, key, bucket string
	salient         string
	vec             []float32
	store           bool
	overheadUSD     float64
	policy          *config.Policy
}

// namespace scopes entries to the tenant, or to the shared namespace when the tenant
// opted in; only opted-in tenants read from it, so sharing needs both sides [CCH-03].
func namespace(rc *RequestContext) string {
	if t := rc.Principal.Tenant; t != nil && t.Cache.Shared {
		return "shared"
	}
	return "t:" + rc.Principal.TenantID()
}

// cacheable applies the default determinism rules [CCH-04].
func (e *Engine) cacheable(s *Snapshot, rc *RequestContext, req *api.ChatRequest, pol *config.Policy) (bool, string) {
	c := s.Config.Cache
	switch {
	case !c.Enabled:
		return false, "cache disabled"
	case pol != nil && pol.Cache.Disabled:
		return false, "policy disables cache"
	case rc.Hints.Cache == "bypass" || rc.Hints.Cache == "no-store":
		return false, "client bypass"
	case req.N != nil && *req.N > 1:
		return false, "n > 1"
	case len(req.Tools) > 0 && (pol == nil || !pol.Cache.AllowTools):
		return false, "tools present"
	case rc.Shadow || rc.EvalRun != "":
		return false, "eval/shadow"
	}
	temp := c.DefaultTemperature
	if req.Temperature != nil {
		temp = *req.Temperature
	}
	if temp > c.MaxTemperature {
		return false, fmt.Sprintf("temperature %.2f above %.2f", temp, c.MaxTemperature)
	}
	return true, ""
}

// outputParams is everything besides the messages that affects output.
func outputParams(req *api.ChatRequest, model string, pol *config.Policy) map[string]any {
	m := map[string]any{"model": model, "temperature": req.Temperature, "top_p": req.TopP, "max_tokens": req.MaxOutput(0),
		"stop": req.Stop, "seed": req.Seed, "tools": req.Tools, "tool_choice": req.ToolChoice, "response_format": req.ResponseFormat,
		"presence_penalty": req.PresencePenalty, "frequency_penalty": req.FrequencyPenalty, "reasoning_effort": req.ReasoningEffort,
		"extra": req.Extra}
	if pol != nil {
		m["policy"] = fmt.Sprintf("%s@%d", pol.Name, pol.Version)
	}
	return m
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:16])
}

// exactKey covers every output-affecting field [CCH-01].
func exactKey(req *api.ChatRequest, model string, pol *config.Policy) string {
	return digest(map[string]any{"p": outputParams(req, model, pol), "m": req.Messages})
}

// semanticBucket groups requests that differ only in the wording of the last user
// message: same parameters and the same earlier turns [CCH-02].
func semanticBucket(req *api.ChatRequest, model string, pol *config.Policy) (string, string) {
	last := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			last = i
			break
		}
	}
	if last < 0 {
		return "", ""
	}
	var prior []api.Message
	for i, m := range req.Messages {
		if i != last {
			prior = append(prior, m)
		}
	}
	text := req.Messages[last].Content.PlainText()
	if req.Messages[last].Content.HasImage() {
		return "", "" // images are not embedded; exact cache only
	}
	return digest(map[string]any{"p": outputParams(req, model, pol), "prior": prior}), text
}

// lookupCache returns a hit, or the lookup state to store the response under on a miss.
func (e *Engine) lookupCache(ctx context.Context, s *Snapshot, rc *RequestContext, req *api.ChatRequest, pol *config.Policy) (*cache.Entry, string, float64, *cacheLookup) {
	if ok, _ := e.cacheable(s, rc, req, pol); !ok {
		return nil, CacheBypass, 0, &cacheLookup{}
	}
	lk := &cacheLookup{ns: namespace(rc), key: exactKey(req, req.Model, pol), store: true, policy: pol}
	refresh := rc.Hints.Cache == "refresh"
	if !refresh {
		if ent, err := e.Cache.Get(ctx, lk.ns, lk.key); err == nil && ent != nil {
			return ent, CacheExactHit, 1, lk
		}
	}
	c := s.Config.Cache
	if !c.Semantic || c.Embedder == "" {
		return nil, CacheMiss, 0, lk
	}
	bucket, text := semanticBucket(req, req.Model, pol)
	if bucket == "" {
		return nil, CacheMiss, 0, lk
	}
	vecs, ecost, err := e.embed(ctx, s, rc, c.Embedder, []string{text})
	if err != nil || len(vecs) == 0 {
		return nil, CacheMiss, 0, lk
	}
	lk.bucket, lk.vec, lk.overheadUSD, lk.salient = bucket, vecs[0], ecost, cache.Salient(text)
	if refresh {
		return nil, CacheMiss, 0, lk
	}
	threshold := c.SemanticThreshold
	if pol != nil && pol.Cache.SemanticThreshold != nil {
		threshold = *pol.Cache.SemanticThreshold
	}
	cands, err := e.Cache.Candidates(ctx, lk.ns, bucket)
	if err != nil {
		return nil, CacheMiss, 0, lk
	}
	var best *cache.Candidate
	bestScore := 0.0
	for i := range cands {
		if cands[i].Salient != lk.salient {
			continue
		}
		if sc := cache.Cosine(lk.vec, cands[i].Vec); sc >= threshold && sc > bestScore {
			best, bestScore = &cands[i], sc
		}
	}
	if best == nil {
		return nil, CacheMiss, 0, lk
	}
	ent, err := e.Cache.Get(ctx, lk.ns, best.Key)
	if err != nil || ent == nil {
		return nil, CacheMiss, 0, lk
	}
	if !e.verifySemanticHit(ctx, s, rc, req, ent) {
		return nil, CacheMiss, 0, lk
	}
	e.sampleHit(ctx, s, rc, ent, text, bestScore)
	return ent, CacheSemanticHit, bestScore, lk
}

// verifySemanticHit optionally asks a cheap judge whether the cached answer fits the
// new question [CCH-09].
func (e *Engine) verifySemanticHit(ctx context.Context, s *Snapshot, rc *RequestContext, req *api.ChatRequest, ent *cache.Entry) bool {
	judge := s.Config.Cache.JudgeHits
	if judge == "" {
		return true
	}
	var resp api.ChatResponse
	if json.Unmarshal(ent.Response, &resp) != nil {
		return false
	}
	text, _ := responseText(&resp)
	score, _, err := e.judgeScore(ctx, s, rc, judge, lastUserText(req), text)
	return err == nil && score >= 0.6
}

// sampleHit records a share of semantic hits for human review [CCH-09].
func (e *Engine) sampleHit(ctx context.Context, s *Snapshot, rc *RequestContext, ent *cache.Entry, text string, score float64) {
	if rate := s.Config.Cache.SampleRate; rate > 0 && rand.Float64() < rate {
		body, _ := json.Marshal(map[string]any{"request_id": rc.ID, "entry": ent.Key, "score": score, "question_tokens": tokens.Count(text), "ts": time.Now().UTC()})
		_ = e.Store.PutDoc(ctx, store.Doc{Kind: "cache_sample", ID: rc.ID, Tenant: rc.Principal.TenantID(), Body: body})
	}
}

// storeCache saves a successful response.
func (e *Engine) storeCache(ctx context.Context, s *Snapshot, rc *RequestContext, lk *cacheLookup, resp *api.ChatResponse, meta *Meta) {
	if lk == nil || !lk.store || rc.Hints.Cache == "no-store" {
		return
	}
	b, _ := json.Marshal(resp)
	pol := lk.policy
	ent := &cache.Entry{Key: lk.key, Namespace: lk.ns, Tenant: rc.Principal.TenantID(), Model: meta.RequestedModel, Deployment: meta.Deployment,
		Provider: meta.Provider, CostUSD: meta.Cost.Total(), InputTokens: meta.Usage.TotalInput(), OutputTokens: meta.Usage.TotalOutput(),
		Response: b, Created: time.Now().UTC()}
	if pol != nil {
		ent.Policy, ent.PolicyVersion = pol.Name, pol.Version
	}
	c := s.Config.Cache
	if err := e.Cache.Put(ctx, ent, c.TTL.D(), c.MaxEntries); err != nil {
		e.Log.Warn("cache put failed", "err", err)
		return
	}
	if lk.bucket != "" && lk.vec != nil {
		_ = e.Cache.PutVector(ctx, lk.ns, lk.bucket, cache.Candidate{Key: lk.key, Vec: lk.vec, At: time.Now().Unix(), Salient: lk.salient}, c.MaxSemanticPerBucket, c.TTL.D())
	}
}

// serveHit turns a cache entry into a response billed at the cache price, recording the
// cost avoided [CCH-06].
func (e *Engine) serveHit(s *Snapshot, rc *RequestContext, ent *cache.Entry, status string, meta *Meta) (*api.ChatResponse, error) {
	var resp api.ChatResponse
	if err := json.Unmarshal(ent.Response, &resp); err != nil {
		return nil, err
	}
	resp.ID = "chatcmpl-" + strings.TrimPrefix(rc.ID, "req_")
	resp.Created = time.Now().Unix()
	price := s.Config.Cache.PriceUSD
	meta.CacheStatus = status
	meta.Deployment, meta.Provider = ent.Deployment, ent.Provider
	meta.Policy, meta.PolicyVersion = ent.Policy, ent.PolicyVersion
	meta.RouteReason = "cache_" + status
	meta.Usage = cost.Usage{Input: ent.InputTokens, Output: ent.OutputTokens}
	meta.Cost = cost.Breakdown{}
	meta.CostUSD = price
	meta.CacheSavedUSD = max(0, ent.CostUSD-price)
	return &resp, nil
}

// embed calls an embeddings deployment directly, returning vectors and their cost.
func (e *Engine) embed(ctx context.Context, s *Snapshot, rc *RequestContext, depID string, texts []string) ([][]float32, float64, error) {
	d := s.Catalog.Deployments[depID]
	if d == nil {
		return nil, 0, fmt.Errorf("embedder %q not found", depID)
	}
	raw, _ := json.Marshal(texts)
	resp, err := s.Providers.Get(d.Provider).Embeddings(ctx, d, &api.EmbeddingsRequest{Model: d.Model, Input: raw}, e.timeouts(s, rc, d))
	if err != nil {
		return nil, 0, err
	}
	out := make([][]float32, len(resp.Data))
	for i, emb := range resp.Data {
		v := make([]float32, len(emb.Embedding))
		for j, x := range emb.Embedding {
			v[j] = float32(x)
		}
		out[i] = v
	}
	in := 0
	if resp.Usage != nil {
		in = resp.Usage.PromptTokens
	} else {
		for _, t := range texts {
			in += tokens.Count(t)
		}
	}
	return out, cost.Of(d, rc.Start, cost.Usage{Input: in}).Total(), nil
}

// chatInternal calls a deployment directly for the gateway's own purposes (classifier,
// judge), returning the text, its cost and latency [RTE-16].
func (e *Engine) chatInternal(ctx context.Context, s *Snapshot, rc *RequestContext, depID, system, user string, maxTokens int) (string, float64, float64, error) {
	d := s.Catalog.Deployments[depID]
	if d == nil {
		return "", 0, 0, fmt.Errorf("deployment %q not found", depID)
	}
	req := &api.ChatRequest{Model: d.Model, MaxTokens: &maxTokens, Messages: []api.Message{
		{Role: "system", Content: api.TextContent(system)}, {Role: "user", Content: api.TextContent(user)}}}
	zero := 0.0
	req.Temperature = &zero
	start := time.Now()
	resp, err := s.Providers.Get(d.Provider).Chat(ctx, d, req, e.timeouts(s, rc, d))
	ms := float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		return "", 0, ms, err
	}
	text, calls := responseText(resp)
	u := usageFrom(req, resp.Usage, text, calls)
	return text, cost.Of(d, rc.Start, u).Total(), ms, nil
}

func lastUserText(req *api.ChatRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.Messages[i].Content.PlainText()
		}
	}
	return ""
}

// replayChunks renders a complete response as stream chunks, for cache hits and
// buffered cascades on streaming requests [CCH-05].
func replayChunks(resp *api.ChatResponse, model string) []*api.ChatChunk {
	mk := func(d api.Delta, finish *string) *api.ChatChunk {
		return &api.ChatChunk{ID: resp.ID, Object: "chat.completion.chunk", Created: resp.Created, Model: model,
			Choices: []api.ChunkChoice{{Delta: d, FinishReason: finish}}}
	}
	empty := ""
	out := []*api.ChatChunk{mk(api.Delta{Role: "assistant", Content: &empty}, nil)}
	if len(resp.Choices) == 0 {
		return out
	}
	msg := resp.Choices[0].Message
	text := msg.Content.PlainText()
	words := strings.SplitAfter(text, " ")
	const per = 8
	for i := 0; i < len(words); i += per {
		part := strings.Join(words[i:min(i+per, len(words))], "")
		out = append(out, mk(api.Delta{Content: &part}, nil))
	}
	for i, tc := range msg.ToolCalls {
		idx := i
		out = append(out, mk(api.Delta{ToolCalls: []api.ToolCall{{Index: &idx, ID: tc.ID, Type: "function", Function: tc.Function}}}, nil))
	}
	fin := resp.Choices[0].FinishReason
	if fin == "" {
		fin = "stop"
	}
	out = append(out, mk(api.Delta{}, &fin))
	return out
}

// InvalidateCache removes entries by tenant, policy, model or age [CCH-08].
func (e *Engine) InvalidateCache(ctx context.Context, tenant, policy, model string, olderThan time.Duration) (int, error) {
	ns := ""
	if tenant != "" {
		ns = "t:" + tenant
	}
	return e.Cache.Invalidate(ctx, ns, cache.Filter{Policy: policy, Model: model, OlderThan: olderThan})
}
