package governance

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/config"
)

// RateScope is one level (key, project, tenant) whose limits apply to a request.
type RateScope struct {
	Kind   string
	ID     string
	Limits config.RateLimits
}

// Admission is an admitted request's hold on rate-limit state.
type Admission struct {
	g        *Governor
	scopes   []RateScope
	estInput int64
	id       string
	conc     []string
	Headers  map[string]string
}

// RateScopes lists the applicable rate-limit scopes for a principal.
func RateScopes(p *Principal) []RateScope {
	var out []RateScope
	if p.KeyID != "" {
		out = append(out, RateScope{"key", p.KeyID, p.Scopes.RateLimits})
	}
	if p.Project != nil {
		out = append(out, RateScope{"project", p.Project.ID, p.Project.Limits})
	}
	if p.Tenant != nil {
		out = append(out, RateScope{"tenant", p.Tenant.ID, p.Tenant.Limits})
	}
	return out
}

func rlKey(s RateScope, metric string) string { return "rl:" + s.Kind + ":" + s.ID + ":" + metric }

// Admit checks RPM, input TPM, output TPM and concurrency for every scope atomically
// and records the request [BUD-04]. Rate-limit backends fail open: an outage must not
// take the gateway down (budgets, not rate limits, protect money) [NFR-08].
func (g *Governor) Admit(ctx context.Context, requestID string, scopes []RateScope, estInput int) (*Admission, *api.Error) {
	ad := &Admission{g: g, scopes: scopes, estInput: int64(estInput), id: requestID, Headers: map[string]string{}}
	now := g.Now()
	var keys []string
	var limits, incr []int64
	type slot struct {
		scope  RateScope
		metric string
	}
	var slots []slot
	for _, s := range scopes {
		if s.Limits.RPM > 0 {
			keys, limits, incr = append(keys, rlKey(s, "rpm")), append(limits, int64(s.Limits.RPM)), append(incr, 1)
			slots = append(slots, slot{s, "requests"})
		}
		if s.Limits.ITPM > 0 {
			keys, limits, incr = append(keys, rlKey(s, "itpm")), append(limits, int64(s.Limits.ITPM)), append(incr, int64(estInput))
			slots = append(slots, slot{s, "input_tokens"})
		}
		if s.Limits.OTPM > 0 {
			// Output tokens are unknown up front: admit while the window is below the limit.
			keys, limits, incr = append(keys, rlKey(s, "otpm")), append(limits, int64(s.Limits.OTPM)-1), append(incr, 0)
			slots = append(slots, slot{s, "output_tokens"})
		}
	}
	if len(keys) > 0 {
		idx, counts, err := g.C.Window(ctx, keys, limits, incr, now)
		if err != nil {
			g.Log.Warn("rate-limit counters unavailable; admitting", "err", err)
			return ad, nil
		}
		reset := 60 - now.Unix()%60
		for i, sl := range slots {
			lim := limits[i]
			if sl.metric == "output_tokens" {
				lim++
			}
			if sl.scope.Kind == "key" || ad.Headers["X-Ratelimit-Limit-"+hdr(sl.metric)] == "" {
				ad.Headers["X-Ratelimit-Limit-"+hdr(sl.metric)] = strconv.FormatInt(lim, 10)
				ad.Headers["X-Ratelimit-Remaining-"+hdr(sl.metric)] = strconv.FormatInt(max(0, lim-counts[i]), 10)
				ad.Headers["X-Ratelimit-Reset-"+hdr(sl.metric)] = fmt.Sprintf("%ds", reset)
			}
		}
		if idx >= 0 {
			sl := slots[idx]
			e := api.NewError(api.CodeRateLimited, "%s rate limit exceeded for %s %q (%d %s per minute)", sl.metric, sl.scope.Kind, sl.scope.ID, limits[idx]+boolToInt(sl.metric == "output_tokens"), sl.metric)
			e.RetryAfter = time.Duration(reset) * time.Second
			e.Headers = ad.Headers
			return nil, e
		}
	}
	for _, s := range scopes {
		if s.Limits.Concurrent <= 0 {
			continue
		}
		key := "cc:" + s.Kind + ":" + s.ID
		ok, err := g.C.Acquire(ctx, key, requestID, int64(s.Limits.Concurrent), 15*time.Minute)
		if err != nil {
			continue
		}
		if !ok {
			ad.Release(ctx, 0, 0)
			e := api.NewError(api.CodeRateLimited, "concurrent request limit (%d) reached for %s %q", s.Limits.Concurrent, s.Kind, s.ID)
			e.RetryAfter = time.Second
			e.Headers = ad.Headers
			return nil, e
		}
		ad.conc = append(ad.conc, key)
	}
	return ad, nil
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func hdr(metric string) string {
	switch metric {
	case "requests":
		return "Requests"
	case "input_tokens":
		return "Input-Tokens"
	}
	return "Output-Tokens"
}

// Release ends the admission: frees concurrency slots, debits output tokens and
// corrects the input-token estimate with the actual count.
func (a *Admission) Release(ctx context.Context, actualInput, actualOutput int) {
	if a == nil {
		return
	}
	for _, k := range a.conc {
		_ = a.g.C.Release(ctx, k, a.id)
	}
	a.conc = nil
	var keys []string
	var incr []int64
	for _, s := range a.scopes {
		if s.Limits.ITPM > 0 && int64(actualInput) != a.estInput && actualInput > 0 {
			keys, incr = append(keys, rlKey(s, "itpm")), append(incr, int64(actualInput)-a.estInput)
		}
		if s.Limits.OTPM > 0 && actualOutput > 0 {
			keys, incr = append(keys, rlKey(s, "otpm")), append(incr, int64(actualOutput))
		}
	}
	if len(keys) > 0 {
		_ = a.g.C.Add(ctx, keys, incr, a.g.Now())
	}
	a.estInput = int64(actualInput)
}
