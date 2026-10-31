package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/catalog"
	"github.com/SureshKhemka/llm-gateway/internal/cost"
	"github.com/SureshKhemka/llm-gateway/internal/observe"
	"github.com/SureshKhemka/llm-gateway/internal/provider"
	"github.com/SureshKhemka/llm-gateway/internal/resilience"
)

// attemptFunc runs one attempt on d.
type attemptFunc func(ctx context.Context, d *catalog.Deployment) error

// held is a deployment slot an attempt holds until the response is complete.
type held struct {
	d       *catalog.Deployment
	release func()
	start   time.Time
}

// execute walks the plan's attempt list: it skips deployments whose breaker is open or
// whose concurrency slots stay full past the queue wait, falls back on retriable errors,
// cycles back with jittered exponential backoff (honouring Retry-After) when every
// candidate failed, and stops at the attempt limit or total deadline [RES-02, RES-03,
// RES-05, RES-07]. Every candidate comes from the already-filtered set, so fallback
// never relaxes a hard constraint [RES-08].
func (e *Engine) execute(ctx context.Context, s *Snapshot, rc *RequestContext, p *Plan, meta *Meta, run attemptFunc) (*held, error) {
	// Budget: every attempt must be covered by a worst-case reservation; a pricier
	// fallback grows it or is skipped [BUD-03].
	pre := func(d *catalog.Deployment) *api.Error {
		if rc.reservation == nil {
			return nil
		}
		return rc.reservation.Grow(ctx, e.worstCase(s, rc, p, d))
	}
	var skipErr error
	rcfg := s.Config.Resilience
	maxAttempts := rcfg.MaxAttempts
	if p.Policy != nil && p.Policy.MaxAttempts > 0 {
		maxAttempts = p.Policy.MaxAttempts
	}
	deadline := rc.Start.Add(rcfg.TotalDeadline.D())
	list := p.Attempts
	var lastErr error
	var lastRetryAfter time.Duration
	attempts, idx, cycles := 0, 0, 0
	lastDep := ""
	skipped := map[string]bool{}
	for attempts < maxAttempts {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if idx >= len(list) {
			// Every candidate was tried or skipped. Cycle back only after a retriable
			// failure; a list of skips alone means nothing can serve the request now.
			if lastErr == nil || cycles >= maxAttempts {
				break
			}
			idx, cycles = 0, cycles+1
		}
		d := list[idx]
		idx++
		if berr := pre(d); berr != nil {
			if !skipped[d.ID+"/budget"] {
				meta.Attempts = append(meta.Attempts, observe.Attempt{Deployment: d.ID, Provider: d.Provider, Kind: "skip", Status: "skipped", Error: berr.Code})
				skipped[d.ID+"/budget"] = true
			}
			skipErr = berr
			continue
		}
		if !e.Health.Acquire(d.ID) {
			if !skipped[d.ID+"/circuit"] {
				meta.Attempts = append(meta.Attempts, observe.Attempt{Deployment: d.ID, Provider: d.Provider, Kind: "skip", Status: "skipped", Error: "circuit_open"})
				skipped[d.ID+"/circuit"] = true
			}
			continue
		}
		release, ok := e.Limiter.Acquire(ctx, d.ID, d.Concurrency, d.QueueWait)
		if !ok {
			e.Health.Release(d.ID)
			meta.Attempts = append(meta.Attempts, observe.Attempt{Deployment: d.ID, Provider: d.Provider, Kind: "skip", Status: "skipped", Error: "concurrency_full"})
			if lastErr == nil {
				lastErr = api.NewError(api.CodeOverloaded, "all eligible deployments are at their concurrency limit")
			}
			continue
		}
		kind := "primary"
		if attempts > 0 {
			kind = "fallback"
			if d.ID == lastDep || cycles > 0 {
				kind = "retry"
				wait := resilience.Backoff(attempts-1, rcfg.BackoffBase.D(), rcfg.BackoffMax.D(), lastRetryAfter)
				if time.Now().Add(wait).After(deadline) {
					release()
					e.Health.Release(d.ID)
					break
				}
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					release()
					e.Health.Release(d.ID)
					return nil, ctx.Err()
				}
			}
		}
		attempts++
		lastDep = d.ID
		start := e.Now()
		err := run(ctx, d)
		if err == nil {
			meta.Attempts = append(meta.Attempts, observe.Attempt{Deployment: d.ID, Provider: d.Provider, Kind: kind, Status: "ok"})
			meta.FallbackAttempts = attempts - 1
			return &held{d: d, release: release, start: start}, nil
		}
		release()
		ue := provider.AsUpstream(err)
		lat := msSince(e.Now, start)
		if ue.Kind == provider.KindCanceled {
			e.Health.Release(d.ID)
		} else {
			e.Health.Record(d.ID, !ue.CountsAgainstHealth(), lat, string(ue.Kind)+": "+ue.Message)
		}
		meta.Attempts = append(meta.Attempts, observe.Attempt{Deployment: d.ID, Provider: d.Provider, Kind: kind, Status: "error", Error: string(ue.Kind), LatencyMs: lat})
		lastErr, lastRetryAfter = err, ue.RetryAfter
		if !ue.Retriable() {
			return nil, err
		}
		if time.Now().After(deadline) {
			break
		}
	}
	meta.FallbackAttempts = max(0, attempts-1)
	if lastErr == nil && skipErr != nil {
		return nil, skipErr
	}
	if lastErr == nil {
		return nil, api.NewError(api.CodeUpstreamUnavailable, "every eligible deployment is unavailable (circuit open)")
	}
	return nil, lastErr
}

// worstCase is the reservation for serving the request on d: estimated input (inflated
// by the safety factor) at the input price plus the output cap at the output price.
func (e *Engine) worstCase(s *Snapshot, rc *RequestContext, p *Plan, d *catalog.Deployment) float64 {
	in := int(float64(p.PromptTokens) * e.Gov.InputSafety)
	out := min(p.MaxOutput, d.MaxOutputTokens)
	if out <= 0 {
		out = d.MaxOutputTokens
	}
	return cost.WorstCase(d, rc.Start, in, out)
}

// Pins remembers which deployment served each session [RTE-11]. Entries expire after TTL.
type Pins struct {
	mu  sync.Mutex
	m   map[string]pin
	TTL time.Duration
}

type pin struct {
	dep string
	at  time.Time
}

func NewPins(ttl time.Duration) *Pins { return &Pins{m: map[string]pin{}, TTL: ttl} }

func (p *Pins) key(rc *RequestContext) string {
	return rc.Principal.TenantID() + "\x00" + rc.Hints.Session
}

func (p *Pins) Get(rc *RequestContext) string {
	if rc.Hints.Session == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.m[p.key(rc)]
	if !ok || time.Since(v.at) > p.TTL {
		return ""
	}
	return v.dep
}

func (p *Pins) Set(rc *RequestContext, dep string) {
	if rc.Hints.Session == "" || dep == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[p.key(rc)] = pin{dep: dep, at: time.Now()}
	if len(p.m) > 100000 {
		for k, v := range p.m {
			if time.Since(v.at) > p.TTL {
				delete(p.m, k)
			}
		}
	}
}
