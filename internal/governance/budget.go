package governance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/cost"
)

// Grant is a temporary budget increase [BUD-08].
type Grant struct {
	ID        string    `json:"id"`
	BudgetID  string    `json:"budget_id"`
	ExtraUSD  float64   `json:"extra_usd"`
	ExpiresAt time.Time `json:"expires_at"`
	Actor     string    `json:"actor"`
	Reason    string    `json:"reason"`
}

// Governor enforces budgets and rate limits through shared Counters.
type Governor struct {
	C        Counters
	Now      func() time.Time
	FailOpen bool // let requests through when counters are unavailable (default: fail closed) [NFR-08]
	// InputSafety inflates estimated prompt tokens in reservations, covering tokenizer
	// differences so a settled cost never exceeds its reservation [BUD-03].
	InputSafety float64
	Alerts      *Alerter
	Log         *slog.Logger

	mu      sync.RWMutex
	revoked map[string]time.Time
}

func NewGovernor(c Counters, log *slog.Logger) *Governor {
	if log == nil {
		log = slog.Default()
	}
	g := &Governor{C: c, Now: time.Now, InputSafety: 1.3, Log: log, revoked: map[string]time.Time{}}
	g.Alerts = NewAlerter(c, log)
	return g
}

// BudgetRef is one budget that applies to a request, resolved for the current period.
type BudgetRef struct {
	Budget      *config.Budget
	Key         string
	LimitNano   int64
	PeriodStart time.Time
	PeriodEnd   time.Time
	EndUser     string
}

// PeriodBounds returns the budget period containing now, in the budget's time zone
// [BUD-01].
func PeriodBounds(b *config.Budget, now time.Time) (time.Time, time.Time) {
	loc, err := time.LoadLocation(b.Timezone)
	if err != nil {
		loc = time.UTC
	}
	t := now.In(loc)
	switch b.Period {
	case "daily":
		s := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
		return s, s.AddDate(0, 0, 1)
	case "weekly":
		wd := (int(t.Weekday()) + 6) % 7 // Monday = 0
		s := time.Date(t.Year(), t.Month(), t.Day()-wd, 0, 0, 0, 0, loc)
		return s, s.AddDate(0, 0, 7)
	case "custom":
		d := b.Custom.D()
		anchor := b.Anchor
		if anchor.IsZero() {
			anchor = time.Unix(0, 0)
		}
		n := now.Sub(anchor) / d
		if now.Before(anchor) {
			n--
		}
		s := anchor.Add(n * d)
		return s, s.Add(d)
	default: // monthly
		s := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
		return s, s.AddDate(0, 1, 0)
	}
}

// Scope identifies who a request is attributed to.
type Scope struct {
	Tenant, Project, Key, EndUser string
	Eval, Shadow                  bool
}

// Applicable returns the budgets that apply to a request in scope sc.
func (g *Governor) Applicable(budgets []config.Budget, grants []Grant, sc Scope) []BudgetRef {
	now := g.Now()
	var out []BudgetRef
	for i := range budgets {
		b := &budgets[i]
		var match bool
		endUser := ""
		switch b.Scope {
		case "tenant":
			match = b.ScopeID == sc.Tenant && !sc.Shadow
		case "project":
			match = b.ScopeID == sc.Project && !sc.Shadow
		case "key":
			match = b.ScopeID == sc.Key && !sc.Shadow
		case "end_user":
			// scope_id names the tenant; every end user of it gets their own counter.
			match = b.ScopeID == sc.Tenant && sc.EndUser != "" && !sc.Shadow
			endUser = sc.EndUser
		case "eval":
			match = sc.Eval && (b.ScopeID == "" || b.ScopeID == sc.Tenant)
		case "shadow":
			match = sc.Shadow && (b.ScopeID == "" || b.ScopeID == sc.Tenant)
		}
		if !match {
			continue
		}
		start, end := PeriodBounds(b, now)
		limit := b.HardUSD
		for _, gr := range grants {
			if gr.BudgetID == b.ID && now.Before(gr.ExpiresAt) {
				limit += gr.ExtraUSD
			}
		}
		key := fmt.Sprintf("b:%s:%d", b.ID, start.Unix())
		if endUser != "" {
			key += ":" + endUser
		}
		out = append(out, BudgetRef{Budget: b, Key: key, LimitNano: cost.Nano(limit), PeriodStart: start, PeriodEnd: end, EndUser: endUser})
	}
	return out
}

func keysOf(refs []BudgetRef) ([]string, []int64) {
	keys, limits := make([]string, len(refs)), make([]int64, len(refs))
	for i, r := range refs {
		keys[i], limits[i] = r.Key, r.LimitNano
	}
	return keys, limits
}

func ttlOf(refs []BudgetRef, now time.Time) time.Duration {
	var ttl time.Duration
	for _, r := range refs {
		if d := r.PeriodEnd.Sub(now) + 7*24*time.Hour; d > ttl {
			ttl = d
		}
	}
	return ttl
}

// Reservation holds reserved budget for one in-flight request.
type Reservation struct {
	g        *Governor
	refs     []BudgetRef
	amount   int64
	mu       sync.Mutex
	settled  bool
	disabled bool // counters unavailable and fail-open
}

// Reserve reserves amountUSD against every applicable budget, or fails with
// budget_exceeded naming the exhausted budget [BUD-03].
func (g *Governor) Reserve(ctx context.Context, refs []BudgetRef, amountUSD float64) (*Reservation, *api.Error) {
	res := &Reservation{g: g, refs: refs}
	if len(refs) == 0 {
		return res, nil
	}
	if err := res.grow(ctx, cost.Nano(amountUSD)); err != nil {
		return nil, err
	}
	return res, nil
}

func (r *Reservation) grow(ctx context.Context, target int64) *api.Error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.disabled || target <= r.amount {
		return nil
	}
	keys, limits := keysOf(r.refs)
	delta := target - r.amount
	idx, err := r.g.C.Reserve(ctx, keys, limits, delta, ttlOf(r.refs, r.g.Now()))
	if err != nil {
		if r.g.FailOpen {
			r.g.Log.Warn("budget counters unavailable; failing open", "err", err)
			r.disabled = true
			return nil
		}
		return api.NewError(api.CodeBudgetUnavailable, "budget service unavailable")
	}
	if idx >= 0 {
		b := r.refs[idx].Budget
		r.g.Alerts.Exceeded(ctx, r.refs[idx], r.g.Now())
		e := api.NewError(api.CodeBudgetExceeded, "%s budget %q (%s, $%.2f) is exhausted for this period", b.Scope, b.ID, b.Period, cost.FromNano(r.refs[idx].LimitNano))
		e.Headers = map[string]string{"X-Gw-Budget-Id": b.ID}
		return e
	}
	r.amount = target
	return nil
}

// Grow raises the reservation to amountUSD (a fallback to a pricier deployment).
func (r *Reservation) Grow(ctx context.Context, amountUSD float64) *api.Error {
	if r == nil || len(r.refs) == 0 {
		return nil
	}
	return r.grow(ctx, cost.Nano(amountUSD))
}

// Settle replaces the reservation with the actual cost, then fires threshold alerts
// [BUD-02, BUD-07]. It is idempotent.
func (r *Reservation) Settle(ctx context.Context, actualUSD float64) {
	if r == nil || len(r.refs) == 0 {
		return
	}
	r.mu.Lock()
	if r.settled || r.disabled {
		r.mu.Unlock()
		return
	}
	r.settled = true
	amount := r.amount
	r.mu.Unlock()
	keys, _ := keysOf(r.refs)
	actual := cost.Nano(actualUSD)
	if err := r.g.C.Settle(ctx, keys, amount, actual, ttlOf(r.refs, r.g.Now())); err != nil {
		r.g.Log.Error("budget settle failed", "err", err)
		return
	}
	r.g.Alerts.Check(ctx, r.g, r.refs)
}

// Remaining returns the smallest remaining amount across refs, in USD.
func (g *Governor) Remaining(ctx context.Context, refs []BudgetRef) *float64 {
	if len(refs) == 0 {
		return nil
	}
	keys, limits := keysOf(refs)
	spent, res, err := g.C.Usage(ctx, keys)
	if err != nil {
		return nil
	}
	best := math.Inf(1)
	for i := range refs {
		rem := cost.FromNano(limits[i] - spent[i] - res[i])
		best = math.Min(best, math.Max(0, rem))
	}
	return &best
}

// Utilisation returns the highest utilisation percentage (spent+reserved over limit)
// across refs; routing uses it to degrade to cheaper tiers [RTE-08].
func (g *Governor) Utilisation(ctx context.Context, refs []BudgetRef) float64 {
	if len(refs) == 0 {
		return 0
	}
	keys, limits := keysOf(refs)
	spent, res, err := g.C.Usage(ctx, keys)
	if err != nil {
		return 0
	}
	var u float64
	for i := range refs {
		if limits[i] > 0 {
			u = math.Max(u, 100*float64(spent[i]+res[i])/float64(limits[i]))
		}
	}
	return u
}

// BudgetStatus is a budget's state in the current period.
type BudgetStatus struct {
	Budget      config.Budget `json:"budget"`
	EndUser     string        `json:"end_user,omitempty"`
	PeriodStart time.Time     `json:"period_start"`
	PeriodEnd   time.Time     `json:"period_end"`
	LimitUSD    float64       `json:"limit_usd"`
	SpentUSD    float64       `json:"spent_usd"`
	ReservedUSD float64       `json:"reserved_usd"`
	Utilisation float64       `json:"utilisation_pct"`
}

func (g *Governor) Status(ctx context.Context, refs []BudgetRef) ([]BudgetStatus, error) {
	keys, _ := keysOf(refs)
	spent, res, err := g.C.Usage(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := make([]BudgetStatus, len(refs))
	for i, r := range refs {
		out[i] = BudgetStatus{Budget: *r.Budget, EndUser: r.EndUser, PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd,
			LimitUSD: cost.FromNano(r.LimitNano), SpentUSD: cost.FromNano(spent[i]), ReservedUSD: cost.FromNano(res[i])}
		if r.LimitNano > 0 {
			out[i].Utilisation = 100 * float64(spent[i]+res[i]) / float64(r.LimitNano)
		}
	}
	return out, nil
}

// ---------- revocation [TEN-02] ----------

const RevokeChannel = "gw:revoke"

// WatchRevocations subscribes to cross-instance revocation broadcasts.
func (g *Governor) WatchRevocations(ctx context.Context) {
	g.C.Subscribe(ctx, RevokeChannel, func(id string) { g.MarkRevoked(id) })
}

func (g *Governor) MarkRevoked(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.revoked[id] = g.Now()
}

// Revoke marks a key revoked here and on every other instance.
func (g *Governor) Revoke(ctx context.Context, id string) error {
	g.MarkRevoked(id)
	return g.C.Publish(ctx, RevokeChannel, id)
}

func (g *Governor) IsRevoked(id string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	_, ok := g.revoked[id]
	return ok
}

// IsUnavailable reports whether err came from the counters backend.
func IsUnavailable(err error) bool { return errors.Is(err, ErrUnavailable) }
