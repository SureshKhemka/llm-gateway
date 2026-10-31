package governance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	neturl "net/url"
	"sort"
	"sync"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/cost"
	"github.com/SureshKhemka/llm-gateway/internal/netguard"
)

// Alert is the generic JSON webhook payload [BUD-07].
type Alert struct {
	Type           string    `json:"type"` // budget_threshold | budget_exceeded
	BudgetID       string    `json:"budget_id"`
	Scope          string    `json:"scope"`
	ScopeID        string    `json:"scope_id"`
	EndUser        string    `json:"end_user,omitempty"`
	Period         string    `json:"period"`
	PeriodStart    time.Time `json:"period_start"`
	ThresholdPct   float64   `json:"threshold_pct"`
	UtilisationPct float64   `json:"utilisation_pct"`
	SpentUSD       float64   `json:"spent_usd"`
	LimitUSD       float64   `json:"limit_usd"`
	Time           time.Time `json:"ts"`
}

// Sink is a resolved alert destination.
type Sink struct {
	ID, Type, URL string
	Timeout       time.Duration
}

// Alerter sends each threshold alert once per budget period, across instances.
type Alerter struct {
	c      Counters
	log    *slog.Logger
	client *http.Client

	mu    sync.RWMutex
	sinks map[string]Sink
	// Sent records alerts for tests and the console.
	sentMu sync.Mutex
	Sent   []Alert
}

func NewAlerter(c Counters, log *slog.Logger) *Alerter {
	return &Alerter{c: c, log: log, client: &http.Client{Transport: netguard.Transport(nil), Timeout: 5 * time.Second}, sinks: map[string]Sink{}}
}

// SetSinks installs alert sinks from configuration (hot reload).
func (a *Alerter) SetSinks(cfg []config.AlertSink) {
	m := map[string]Sink{}
	var allowPrivate []string
	for _, s := range cfg {
		url, err := s.URL.Resolve()
		if err != nil {
			a.log.Warn("alert sink url unresolved", "sink", s.ID, "err", err)
			continue
		}
		to := s.Timeout.D()
		if to == 0 {
			to = 5 * time.Second
		}
		m[s.ID] = Sink{ID: s.ID, Type: s.Type, URL: url, Timeout: to}
		if s.AllowPrivateNetwork {
			if u, err := neturl.Parse(url); err == nil {
				allowPrivate = append(allowPrivate, u.Hostname())
			}
		}
	}
	a.mu.Lock()
	a.sinks = m
	a.client = &http.Client{Transport: netguard.Transport(allowPrivate), Timeout: 10 * time.Second}
	a.mu.Unlock()
}

// Check fires an alert for every soft threshold (and 100%) each ref has crossed.
func (a *Alerter) Check(ctx context.Context, g *Governor, refs []BudgetRef) {
	st, err := g.Status(ctx, refs)
	if err != nil {
		return
	}
	for i, s := range st {
		thresholds := append([]float64(nil), refs[i].Budget.SoftPct...)
		thresholds = append(thresholds, 100)
		sort.Float64s(thresholds)
		for _, th := range thresholds {
			if s.Utilisation+1e-9 >= th {
				typ := "budget_threshold"
				if th >= 100 {
					typ = "budget_exceeded"
				}
				a.fire(ctx, refs[i], s, th, typ, g.Now())
			}
		}
	}
}

// Exceeded fires the 100% alert when a request is blocked.
func (a *Alerter) Exceeded(ctx context.Context, ref BudgetRef, now time.Time) {
	s := BudgetStatus{Budget: *ref.Budget, LimitUSD: cost.FromNano(ref.LimitNano), PeriodStart: ref.PeriodStart, Utilisation: 100}
	a.fire(ctx, ref, s, 100, "budget_exceeded", now)
}

func (a *Alerter) fire(ctx context.Context, ref BudgetRef, s BudgetStatus, th float64, typ string, now time.Time) {
	latch := fmt.Sprintf("alert:%s:%d:%s:%g", ref.Budget.ID, ref.PeriodStart.Unix(), ref.EndUser, th)
	first, err := a.c.Latch(ctx, latch, ref.PeriodEnd.Sub(now)+time.Hour)
	if err != nil || !first {
		return
	}
	al := Alert{Type: typ, BudgetID: ref.Budget.ID, Scope: ref.Budget.Scope, ScopeID: ref.Budget.ScopeID, EndUser: ref.EndUser,
		Period: ref.Budget.Period, PeriodStart: ref.PeriodStart, ThresholdPct: th, UtilisationPct: s.Utilisation,
		SpentUSD: s.SpentUSD, LimitUSD: s.LimitUSD, Time: now.UTC()}
	a.sentMu.Lock()
	a.Sent = append(a.Sent, al)
	a.sentMu.Unlock()
	a.mu.RLock()
	var targets []Sink
	for _, id := range ref.Budget.AlertSink {
		if s, ok := a.sinks[id]; ok {
			targets = append(targets, s)
		}
	}
	if len(ref.Budget.AlertSink) == 0 {
		for _, s := range a.sinks {
			targets = append(targets, s)
		}
	}
	client := a.client
	a.mu.RUnlock()
	for _, s := range targets {
		go a.send(client, s, al)
	}
}

func (a *Alerter) send(client *http.Client, s Sink, al Alert) {
	var body any = al
	if s.Type == "slack" {
		body = map[string]string{"text": fmt.Sprintf(":warning: Budget *%s* (%s %s) reached %.0f%%: $%.4f of $%.2f spent this %s.",
			al.BudgetID, al.Scope, al.ScopeID, al.ThresholdPct, al.SpentUSD, al.LimitUSD, al.Period)}
	}
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), s.Timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		a.log.Warn("alert delivery failed", "sink", s.ID, "err", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		a.log.Warn("alert sink rejected", "sink", s.ID, "status", resp.StatusCode)
	}
}

// SentAlerts returns a copy of alerts fired by this instance.
func (a *Alerter) SentAlerts() []Alert {
	a.sentMu.Lock()
	defer a.sentMu.Unlock()
	return append([]Alert(nil), a.Sent...)
}
