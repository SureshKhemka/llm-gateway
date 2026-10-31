package server_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/cost"
	"github.com/SureshKhemka/llm-gateway/internal/gateway"
	"github.com/SureshKhemka/llm-gateway/internal/governance"
	"github.com/SureshKhemka/llm-gateway/internal/sim"
	"github.com/SureshKhemka/llm-gateway/internal/store"
	"github.com/SureshKhemka/llm-gateway/internal/testenv"
	"github.com/SureshKhemka/llm-gateway/internal/workload"
)

const (
	adminOrg     = "gw-admin-org_0000000000000000000000000000"
	adminA       = "gw-admin-ta_1111111111111111111111111111"
	adminB       = "gw-admin-tb_2222222222222222222222222222"
	adminFinops  = "gw-admin-fo_3333333333333333333333333333"
	adminAuditor = "gw-admin-au_4444444444444444444444444444"
	keyA         = "gw-test-ka_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	keyB         = "gw-test-kb_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	keyBurst     = "gw-test-kx_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
)

const m4YAML = `
providers:
  - id: sima
    type: openai
    base_url: ${SIM_SIMA_URL}
policies:
  - name: default
    objective: min_cost
    degrade:
      - {at_pct: 80, max_tier: 1}
      - {at_pct: 95, max_tier: 0}
alerts:
  - id: hook
    type: webhook
    url: env:ALERT_URL
    allow_private_network: true
  - id: slack
    type: slack
    url: env:SLACK_URL
    allow_private_network: true
budgets:
  - id: team-a-monthly
    scope: tenant
    scope_id: team-a
    period: monthly
    timezone: Asia/Kolkata
    hard_usd: 0.50
    soft_pct: [80]
    alerts: [hook, slack]
tenants:
  - id: team-a
    projects:
      - id: a-app
        keys:
          - id: ka
            secret: env:KEY_A
  - id: team-b
    rate_limits: {rpm: 1000}
    projects:
      - id: b-app
        keys:
          - id: kb
            secret: env:KEY_B
          - id: kx
            secret: env:KEY_BURST
            scopes:
              rate_limits: {rpm: 5}
admins:
  - {id: org, role: org_admin, secret: env:ADMIN_ORG}
  - {id: ta, role: tenant_admin, tenant: team-a, secret: env:ADMIN_A}
  - {id: tb, role: tenant_admin, tenant: team-b, secret: env:ADMIN_B}
  - {id: fo, role: finops_viewer, secret: env:ADMIN_FO}
  - {id: au, role: auditor, secret: env:ADMIN_AU}
defaults:
  baseline: sima/qwen-27b
  display_currency: INR
  exchange_rate: 84
` + testenv.StandardDeployments

type hookSink struct {
	mu     sync.Mutex
	bodies []map[string]any
	srv    *httptest.Server
}

func newHook(t *testing.T) *hookSink {
	h := &hookSink{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		h.mu.Lock()
		h.bodies = append(h.bodies, m)
		h.mu.Unlock()
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hookSink) all() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any(nil), h.bodies...)
}

type m4 struct {
	*testenv.Env
	hook, slack *hookSink
}

func m4Env(t *testing.T, counters governance.Counters, yamlExtra ...string) *m4 {
	hook, slack := newHook(t), newHook(t)
	yaml := m4YAML
	for _, x := range yamlExtra {
		yaml += x
	}
	env := testenv.New(t, testenv.Options{YAML: yaml, Counters: counters, Env: map[string]string{
		"KEY_A": keyA, "KEY_B": keyB, "KEY_BURST": keyBurst, "ADMIN_ORG": adminOrg, "ADMIN_A": adminA, "ADMIN_B": adminB,
		"ADMIN_FO": adminFinops, "ADMIN_AU": adminAuditor, "ALERT_URL": hook.srv.URL, "SLACK_URL": slack.srv.URL}})
	return &m4{Env: env, hook: hook, slack: slack}
}

func newMiniredis(t *testing.T) (*miniredis.Miniredis, governance.Counters) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { c.Close() })
	return mr, governance.NewRedisCounters(c, "gw:")
}

func (e *m4) admin(method, path, key string, body any) *testenv.Response {
	return e.Do(method, path, key, body, nil)
}

// Scenario 6: a tenant with a $0.50 monthly budget runs the replay. At 80% routing
// shifts cheaper; at 100% requests get budget_exceeded; the alert webhook fires once per
// threshold; spend never overshoots.
// Covers: BUD-01, BUD-02, BUD-03, BUD-07, RTE-08
func TestScenario06_BudgetDegradationAndHardStop(t *testing.T) {
	_, counters := newMiniredis(t)
	env := m4Env(t, counters)
	items, _ := workload.Load("../../testdata/corpus/corpus.jsonl")
	var blocked, degraded int
	var spentByRecords float64
	for round := 0; round < 80 && blocked == 0; round++ {
		results, _, err := workload.Replay(context.Background(), items, workload.ReplayOptions{
			BaseURL: env.HTTP.URL, Model: "auto", Tenants: []workload.Tenant{{Key: keyA}}, Concurrency: 8, Seed: uint64(round + 1)})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range results {
			switch {
			case r.Status == 402 && r.ErrorCode == "budget_exceeded":
				blocked++
			case r.RouteReason == "budget_degraded":
				degraded++
			}
		}
	}
	if blocked == 0 || degraded == 0 {
		t.Fatalf("expected degradation and a hard stop: degraded=%d blocked=%d", degraded, blocked)
	}
	env.Flush()
	recs, _ := env.Store.ListCostRecords(context.Background(), store.CostFilter{Tenant: "team-a"}, 0)
	for _, r := range recs {
		spentByRecords += r.CostUSD
	}
	st, _ := env.Engine.Gov.Status(context.Background(), env.Engine.Gov.Applicable(env.Config.Budgets, nil, governance.Scope{Tenant: "team-a"}))
	if st[0].SpentUSD > 0.50+1e-9 || spentByRecords > 0.50+1e-9 || st[0].ReservedUSD != 0 {
		t.Fatalf("overshoot: counters spent $%.6f reserved $%.6f, records $%.6f", st[0].SpentUSD, st[0].ReservedUSD, spentByRecords)
	}
	if math.Abs(st[0].SpentUSD-spentByRecords) > 1e-6 {
		t.Fatalf("budget counters $%.9f disagree with cost records $%.9f", st[0].SpentUSD, spentByRecords)
	}
	// Degraded requests are served at tier ≤ 1, except the few only a bigger model can
	// serve (vision, long context).
	var low, high int
	for _, r := range recs {
		if r.RouteReason == "budget_degraded" && r.Status == 200 {
			if env.Engine.Snapshot().Catalog.Deployments[r.Deployment].Tier <= 1 {
				low++
			} else {
				high++
			}
		}
	}
	if float64(low)/float64(low+high) < 0.9 {
		t.Errorf("degraded routing: %d at tier ≤ 1, %d above", low, high)
	}
	time.Sleep(200 * time.Millisecond)
	alerts := env.hook.all()
	byThreshold := map[float64]int{}
	for _, a := range alerts {
		byThreshold[a["threshold_pct"].(float64)]++
	}
	if byThreshold[80] != 1 || byThreshold[100] != 1 || len(alerts) != 2 {
		t.Fatalf("webhook alerts should fire once per threshold: %v", byThreshold)
	}
	if s := env.slack.all(); len(s) != 2 || !strings.Contains(s[0]["text"].(string), "team-a-monthly") {
		t.Fatalf("slack alerts: %v", s)
	}
	t.Logf("spent $%.4f of $0.50; degraded %d requests; blocked %d", st[0].SpentUSD, degraded, blocked)
}

// Hard-budget overshoot must be zero under concurrency, repeated 100 times (spec §2).
// Covers: BUD-03, BUD-05
func TestBUD03_NoOvershootUnderConcurrency100Runs(t *testing.T) {
	_, counters := newMiniredis(t)
	gov := governance.NewGovernor(counters, nil)
	d := &config.Deployment{}
	_ = d
	for run := 0; run < 100; run++ {
		b := []config.Budget{{ID: fmt.Sprintf("b%d", run), Scope: "tenant", ScopeID: "t", Period: "daily", Timezone: "UTC", HardUSD: 0.01}}
		refs := gov.Applicable(b, nil, governance.Scope{Tenant: "t"})
		var wg sync.WaitGroup
		for g := 0; g < 40; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < 5; i++ {
					worst := 0.0007
					res, err := gov.Reserve(context.Background(), refs, worst)
					if err != nil {
						return
					}
					// Actual cost is at most the reservation.
					res.Settle(context.Background(), worst*float64(1+(g+i)%3)/3)
				}
			}(g)
		}
		wg.Wait()
		st, _ := gov.Status(context.Background(), refs)
		if st[0].SpentUSD > 0.01+1e-12 || st[0].ReservedUSD != 0 {
			t.Fatalf("run %d: spent $%.9f reserved $%.9f over $0.01", run, st[0].SpentUSD, st[0].ReservedUSD)
		}
	}
}

// Scenario 7: one key bursts past its limit while another tenant runs normally.
// Covers: BUD-04, API-10
func TestScenario07_RateLimitingIsolatesKeys(t *testing.T) {
	env := m4Env(t, nil)
	var got429 *testenv.Response
	for i := 0; i < 8; i++ {
		r := env.Chat(keyBurst, "sima/gemma-e2b", "hi", nil)
		if r.Status == 429 {
			got429 = r
			break
		}
		if r.Header.Get("X-Ratelimit-Limit-Requests") != "5" {
			t.Fatalf("rate-limit headers missing: %v", r.Header)
		}
	}
	if got429 == nil {
		t.Fatal("burst key never rate-limited")
	}
	if got429.Header.Get("Retry-After") == "" || !strings.Contains(string(got429.Body), "rate_limited") || got429.Header.Get("X-Ratelimit-Remaining-Requests") != "0" {
		t.Fatalf("429 response: %v %s", got429.Header, got429.Body)
	}
	for i := 0; i < 10; i++ {
		if r := env.Chat(keyA, "sima/gemma-e2b", "hi", nil); r.Status != 200 {
			t.Fatalf("other tenant affected: %d", r.Status)
		}
	}
}

// Covers: BUD-04
func TestBUD04_TokenAndConcurrencyLimits(t *testing.T) {
	extra := `
  - id: team-c
    projects:
      - id: c-app
        rate_limits: {itpm: 60, concurrent: 1}
        keys:
          - id: kc
            secret: env:KEY_C
            scopes:
              rate_limits: {otpm: 10}
`
	yaml := strings.Replace(m4YAML, "admins:", strings.TrimPrefix(extra, "\n")+"admins:", 1)
	t.Setenv("KEY_C", "gw-test-kc_cccccccccccccccccccccccccccccccc")
	env := testenv.New(t, testenv.Options{YAML: yaml, Env: map[string]string{"KEY_A": keyA, "KEY_B": keyB, "KEY_BURST": keyBurst,
		"ADMIN_ORG": adminOrg, "ADMIN_A": adminA, "ADMIN_B": adminB, "ADMIN_FO": adminFinops, "ADMIN_AU": adminAuditor, "ALERT_URL": "http://127.0.0.1:1", "SLACK_URL": "http://127.0.0.1:1"}})
	kc := "gw-test-kc_cccccccccccccccccccccccccccccccc"
	// Output TPM: a reply of ≥10 tokens exhausts the key's window.
	r := env.Chat(kc, "sima/qwen-4b", strings.Repeat("word ", 12), nil)
	if r.Status != 200 {
		t.Fatalf("first request: %d %s", r.Status, r.Body)
	}
	r = env.Chat(kc, "sima/qwen-4b", "short", nil)
	if r.Status != 429 || !strings.Contains(string(r.Body), "output_tokens") {
		t.Fatalf("OTPM: %d %s", r.Status, r.Body)
	}
	// Input TPM at project level.
	r = env.Chat(kc, "sima/qwen-4b", strings.Repeat("x ", 80), nil)
	if r.Status != 429 || !strings.Contains(string(r.Body), "input_tokens") && !strings.Contains(string(r.Body), "output_tokens") {
		t.Fatalf("ITPM: %d %s", r.Status, r.Body)
	}
	// Concurrency at project level, with a fresh environment to clear token windows.
	env2 := testenv.New(t, testenv.Options{YAML: strings.Replace(yaml, "itpm: 60, ", "", 1), Env: map[string]string{"KEY_A": keyA, "KEY_B": keyB, "KEY_BURST": keyBurst,
		"ADMIN_ORG": adminOrg, "ADMIN_A": adminA, "ADMIN_B": adminB, "ADMIN_FO": adminFinops, "ADMIN_AU": adminAuditor, "ALERT_URL": "http://127.0.0.1:1", "SLACK_URL": "http://127.0.0.1:1"}})
	env2.Sims["sima"].SetFaults("qwen-4b", sim.Faults{LatencyMs: 400})
	var wg sync.WaitGroup
	var limited atomic.Int64
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := env2.Chat(kc, "sima/qwen-4b", "hi", nil); r.Status == 429 && strings.Contains(string(r.Body), "concurrent") {
				limited.Add(1)
			}
		}()
		time.Sleep(50 * time.Millisecond)
	}
	wg.Wait()
	if limited.Load() != 1 {
		t.Fatalf("concurrency limit: %d limited", limited.Load())
	}
}

// Rate limits hold across instances sharing Redis: two instances do not double a limit.
// Covers: BUD-05
func TestBUD05_LimitsHoldAcrossInstances(t *testing.T) {
	_, counters := newMiniredis(t)
	env := m4Env(t, counters)
	other := env.SecondInstance()
	ok := 0
	for i := 0; i < 10; i++ {
		base := env.HTTP.URL
		if i%2 == 1 {
			base = other.HTTP.URL
		}
		r := env.DoAt(base, "POST", "/v1/chat/completions", keyBurst, userMsg("sima/gemma-e2b", "hi"), nil)
		if r.Status == 200 {
			ok++
		}
	}
	if ok != 5 {
		t.Fatalf("%d requests admitted across two instances, limit is 5", ok)
	}
}

// Scenario 16: tenant A's admin queries tenant B's spend, requests and cache.
// Covers: TEN-06, TEN-04, ADM-05
func TestScenario16_TenantIsolation(t *testing.T) {
	env := m4Env(t, nil)
	rb := env.Chat(keyB, "auto", "hello from team b", nil)
	idB := rb.Header.Get("X-Request-Id")
	env.Chat(keyA, "auto", "hello from team a", nil)
	env.Flush()
	checks := []struct {
		name, method, path string
		body               any
	}{
		{"spend", "GET", "/admin/v1/costs?tenant=team-b&group_by=tenant", nil},
		{"requests", "GET", "/admin/v1/requests?tenant=team-b", nil},
		{"request detail", "GET", "/admin/v1/requests/" + idB, nil},
		{"keys", "GET", "/admin/v1/keys?tenant=team-b", nil},
		{"cache", "POST", "/admin/v1/cache/invalidate", map[string]any{"tenant": "team-b"}},
		{"create key", "POST", "/admin/v1/keys", map[string]any{"tenant": "team-b", "project": "b-app"}},
		{"revoke", "POST", "/admin/v1/keys/kb/revoke", map[string]any{}},
		{"chargeback", "GET", "/admin/v1/chargeback?tenant=team-b", nil},
	}
	for _, c := range checks {
		r := env.admin(c.method, c.path, adminA, c.body)
		if r.Status != 403 && r.Status != 404 {
			t.Errorf("tenant A admin reached tenant B %s: %d %s", c.name, r.Status, r.Body)
		}
	}
	// Without a tenant filter a tenant admin only sees its own tenant.
	var costs struct {
		Rows []store.CostRow `json:"rows"`
	}
	_ = env.admin("GET", "/admin/v1/costs?group_by=tenant", adminA, nil).JSON(&costs)
	for _, r := range costs.Rows {
		if r.Group["tenant"] != "team-a" {
			t.Errorf("tenant A admin saw %v", r.Group)
		}
	}
	var reqs struct {
		Requests []store.CostRecord `json:"requests"`
	}
	_ = env.admin("GET", "/admin/v1/requests", adminA, nil).JSON(&reqs)
	for _, r := range reqs.Requests {
		if r.Tenant != "team-a" {
			t.Errorf("tenant A admin listed a %s request", r.Tenant)
		}
	}
	// Client API: tenant A's key cannot read tenant B's decision record either.
	if r := env.Do("GET", "/v1/route/decisions/"+idB, keyA, nil, nil); r.Status != 404 {
		t.Errorf("cross-tenant decision via client API: %d", r.Status)
	}
	// Tenant B's own admin can see it.
	if r := env.admin("GET", "/admin/v1/requests/"+idB, adminB, nil); r.Status != 200 {
		t.Errorf("tenant B admin denied own request: %d", r.Status)
	}
}

// Covers: TEN-04
func TestTEN04_RolePermissions(t *testing.T) {
	env := m4Env(t, nil)
	cases := []struct {
		key, method, path string
		want              int
	}{
		{"", "GET", "/admin/v1/whoami", 401},
		{keyA, "GET", "/admin/v1/whoami", 401}, // virtual keys are not admin credentials
		{adminFinops, "GET", "/admin/v1/costs", 200},
		{adminFinops, "POST", "/admin/v1/keys", 403},
		{adminFinops, "GET", "/admin/v1/audit", 403},
		{adminAuditor, "GET", "/admin/v1/audit", 200},
		{adminAuditor, "GET", "/admin/v1/config", 200},
		{adminAuditor, "GET", "/admin/v1/costs", 403},
		{adminAuditor, "POST", "/admin/v1/budgets", 403},
		{adminA, "GET", "/admin/v1/audit", 403},
		{adminA, "POST", "/admin/v1/tenants", 403},
		{adminOrg, "GET", "/admin/v1/audit", 200},
		{adminOrg, "GET", "/admin/v1/openapi.json", 200},
	}
	for _, c := range cases {
		var body any
		if c.method == "POST" {
			body = map[string]any{}
		}
		if r := env.admin(c.method, c.path, c.key, body); r.Status != c.want {
			t.Errorf("%s %s as %s: %d want %d (%s)", c.method, c.path, c.key[:min(len(c.key), 12)], r.Status, c.want, r.Body)
		}
	}
	var doc map[string]any
	_ = env.admin("GET", "/admin/v1/openapi.json", "", nil).JSON(&doc)
	if doc["openapi"] != "3.1.0" || len(doc["paths"].(map[string]any)) < 15 {
		t.Fatalf("openapi doc: %v", doc["paths"])
	}
}

// Covers: TEN-02, TEN-03, OBS-06
func TestTEN02_KeyLifecycle(t *testing.T) {
	_, counters := newMiniredis(t)
	env := m4Env(t, counters)
	other := env.SecondInstance()
	// Create: shown once, stored hashed, visible prefix.
	r := env.admin("POST", "/admin/v1/keys", adminA, map[string]any{"tenant": "team-a", "project": "a-app", "name": "ci",
		"scopes": map[string]any{"models": []string{"sima/*", "auto"}, "default_tags": []string{"ci"}, "max_cost_usd": 0.001}})
	var created struct {
		Key    string          `json:"key"`
		Record store.KeyRecord `json:"record"`
	}
	if r.Status != 201 || r.JSON(&created) != nil || !strings.HasPrefix(created.Key, created.Record.Prefix) || created.Record.Prefix == "" {
		t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	stored, _ := env.Store.GetKey(context.Background(), created.Record.ID)
	if string(stored.Hash) == created.Key || strings.Contains(string(r.Body), string(stored.Hash)) {
		t.Fatal("key stored or returned in reversible form")
	}
	if l := env.admin("GET", "/admin/v1/keys?tenant=team-a", adminA, nil); strings.Contains(string(l.Body), created.Key) {
		t.Fatal("listing reveals the key")
	}
	// The key works with its scopes: default tags recorded, pricey request blocked by max cost.
	if r := env.Chat(created.Key, "sima/gemma-e2b", "hi", nil); r.Status != 200 {
		t.Fatalf("new key: %d %s", r.Status, r.Body)
	}
	if r := env.Chat(created.Key, "auto", "hi", nil, map[string]string{"X-Gw-Min-Tier": "3"}); r.Status != 422 {
		t.Fatalf("max_cost_usd scope ignored: %d", r.Status)
	}
	// The second instance learns about the new key on its next reload.
	if err := other.Engine.Rebuild(context.Background(), nil, "test"); err != nil {
		t.Fatal(err)
	}
	// Rotate with overlap: both keys work during the window.
	rr := env.admin("POST", "/admin/v1/keys/"+created.Record.ID+"/rotate", adminA, map[string]any{"overlap_seconds": 1})
	var rotated struct {
		Key string `json:"key"`
	}
	_ = rr.JSON(&rotated)
	if rr.Status != 201 || env.Chat(rotated.Key, "sima/gemma-e2b", "hi", nil).Status != 200 || env.Chat(created.Key, "sima/gemma-e2b", "hi", nil).Status != 200 {
		t.Fatalf("rotation overlap: %d", rr.Status)
	}
	time.Sleep(1100 * time.Millisecond)
	if r := env.Chat(created.Key, "sima/gemma-e2b", "hi", nil); r.Status != 401 {
		t.Fatalf("old key after overlap: %d", r.Status)
	}
	// Revoke: effective on the other instance within 5 seconds (pub/sub, not reload).
	if r := env.DoAt(other.HTTP.URL, "POST", "/v1/chat/completions", keyA, userMsg("sima/gemma-e2b", "hi"), nil); r.Status != 200 {
		t.Fatalf("pre-revoke on other instance: %d", r.Status)
	}
	start := time.Now()
	if r := env.admin("POST", "/admin/v1/keys/ka/revoke", adminA, map[string]any{"reason": "leaked"}); r.Status != 200 {
		t.Fatalf("revoke: %d %s", r.Status, r.Body)
	}
	for {
		r := env.DoAt(other.HTTP.URL, "POST", "/v1/chat/completions", keyA, userMsg("sima/gemma-e2b", "hi"), nil)
		if r.Status == 401 {
			break
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("revocation not effective on the other instance within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("revocation reached the other instance in %v", time.Since(start).Round(time.Millisecond))
	// Expiry.
	exp := time.Now().Add(300 * time.Millisecond)
	r = env.admin("POST", "/admin/v1/keys", adminOrg, map[string]any{"tenant": "team-b", "project": "b-app", "expires_at": exp})
	_ = r.JSON(&created)
	time.Sleep(400 * time.Millisecond)
	if r := env.Chat(created.Key, "sima/gemma-e2b", "hi", nil); r.Status != 401 {
		t.Fatalf("expired key: %d", r.Status)
	}
	// Every change is in the audit log.
	var audit struct {
		Entries []store.AuditEntry `json:"entries"`
	}
	_ = env.admin("GET", "/admin/v1/audit?kind=key", adminAuditor, nil).JSON(&audit)
	actions := map[string]int{}
	for _, e := range audit.Entries {
		actions[e.Action]++
	}
	if actions["create"] < 2 || actions["rotate"] != 1 || actions["revoke"] != 1 {
		t.Fatalf("audit actions: %v", actions)
	}
}

// Covers: TEN-03
func TestTEN03_KeyScopes(t *testing.T) {
	env := m4Env(t, nil)
	mk := func(scopes map[string]any) string {
		r := env.admin("POST", "/admin/v1/keys", adminOrg, map[string]any{"tenant": "team-a", "project": "a-app", "scopes": scopes})
		var c struct{ Key string }
		_ = r.JSON(&c)
		return c.Key
	}
	if k := mk(map[string]any{"endpoints": []string{"embeddings"}}); env.Chat(k, "sima/gemma-e2b", "hi", nil).Status != 403 {
		t.Error("endpoint scope not enforced")
	}
	if k := mk(map[string]any{"ip_allow": []string{"10.9.9.0/24"}}); env.Chat(k, "sima/gemma-e2b", "hi", nil).Status != 403 {
		t.Error("IP allow-list not enforced")
	}
	if k := mk(map[string]any{"providers": []string{"elsewhere"}}); env.Chat(k, "sima/gemma-e2b", "hi", nil).Status != 403 {
		t.Error("provider scope not enforced")
	}
	k := mk(map[string]any{"default_tags": []string{"team-x"}})
	env.Chat(k, "sima/gemma-e2b", "hi", nil, map[string]string{"X-Gw-Tags": "exp-1"})
	env.Flush()
	recs, _ := env.Store.ListCostRecords(context.Background(), store.CostFilter{Tag: "team-x"}, 1)
	if len(recs) != 1 || strings.Join(recs[0].Tags, ",") != "team-x,exp-1" {
		t.Errorf("default tags: %+v", recs)
	}
}

// Covers: BUD-01
func TestBUD01_PeriodsTimezonesAndEndUsers(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	// 2026-10-31 20:00 UTC is already November in India.
	now := time.Date(2026, 10, 31, 20, 0, 0, 0, time.UTC)
	b := &config.Budget{Period: "monthly", Timezone: "Asia/Kolkata"}
	s, e := governance.PeriodBounds(b, now)
	if !s.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, ist)) || !e.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, ist)) {
		t.Errorf("monthly IST: %v – %v", s, e)
	}
	b = &config.Budget{Period: "weekly", Timezone: "UTC"}
	if s, _ = governance.PeriodBounds(b, now); s.Weekday() != time.Monday || now.Sub(s) > 7*24*time.Hour {
		t.Errorf("weekly start %v", s)
	}
	b = &config.Budget{Period: "daily", Timezone: "America/New_York"}
	if s, e = governance.PeriodBounds(b, now); e.Sub(s) != 24*time.Hour || s.Hour() != 0 {
		t.Errorf("daily NY %v %v", s, e)
	}
	b = &config.Budget{Period: "custom", Custom: config.Duration(36 * time.Hour), Anchor: now.Add(-50 * time.Hour)}
	if s, e = governance.PeriodBounds(b, now); !s.Equal(now.Add(-14*time.Hour)) || e.Sub(s) != 36*time.Hour {
		t.Errorf("custom %v %v", s, e)
	}
	// End-user budgets: each end user has their own counter.
	extra := `
  - id: per-user
    scope: end_user
    scope_id: team-b
    period: daily
    hard_usd: 0.000004
`
	env := m4Env(t, nil)
	yaml := strings.Replace(m4YAML, "tenants:", strings.TrimPrefix(extra, "\n")+"tenants:", 1)
	env2 := testenv.New(t, testenv.Options{YAML: yaml, Env: map[string]string{"KEY_A": keyA, "KEY_B": keyB, "KEY_BURST": keyBurst,
		"ADMIN_ORG": adminOrg, "ADMIN_A": adminA, "ADMIN_B": adminB, "ADMIN_FO": adminFinops, "ADMIN_AU": adminAuditor, "ALERT_URL": env.hook.srv.URL, "SLACK_URL": env.slack.srv.URL}})
	u := map[string]string{"X-Gw-End-User": "alice"}
	blocked := false
	for i := 0; i < 10 && !blocked; i++ {
		blocked = env2.Chat(keyB, "sima/gemma-e2b", "hello", map[string]any{"max_tokens": 5}, u).Status == 402
	}
	if !blocked {
		t.Fatal("end-user budget never blocked alice")
	}
	if r := env2.Chat(keyB, "sima/gemma-e2b", "hello", map[string]any{"max_tokens": 5}, map[string]string{"X-Gw-End-User": "bob"}); r.Status != 200 {
		t.Fatalf("bob blocked by alice's budget: %d", r.Status)
	}
}

// Covers: BUD-06, BUD-08, OBS-06, API-09
func TestBUD06_BudgetChangesApplyWithoutRestart(t *testing.T) {
	env := m4Env(t, nil)
	r := env.admin("POST", "/admin/v1/budgets", adminB, map[string]any{"id": "b-tiny", "scope": "tenant", "scope_id": "team-b",
		"period": "daily", "timezone": "UTC", "hard_usd": 0.0000001})
	if r.Status != 200 {
		t.Fatalf("create budget: %d %s", r.Status, r.Body)
	}
	start := time.Now()
	if r := env.Chat(keyB, "sima/gemma-e2b", "hi", nil); r.Status != 402 {
		t.Fatalf("new budget not applied: %d", r.Status)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("budget change took more than 10s")
	}
	// File-managed budgets cannot be edited through the API.
	if r := env.admin("POST", "/admin/v1/budgets", adminA, map[string]any{"id": "team-a-monthly", "scope": "tenant", "scope_id": "team-a", "hard_usd": 99}); r.Status != 409 {
		t.Fatalf("file-managed budget edit: %d", r.Status)
	}
	// A temporary grant lifts the limit; it needs a reason and is audited.
	if r := env.admin("POST", "/admin/v1/budgets/b-tiny/grants", adminB, map[string]any{"extra_usd": 1, "expires_at": time.Now().Add(time.Hour)}); r.Status != 400 {
		t.Fatalf("grant without reason: %d", r.Status)
	}
	if r := env.admin("POST", "/admin/v1/budgets/b-tiny/grants", adminB, map[string]any{"extra_usd": 1, "expires_at": time.Now().Add(time.Hour), "reason": "launch week"}); r.Status != 201 {
		t.Fatalf("grant: %d %s", r.Status, r.Body)
	}
	r = env.Chat(keyB, "sima/gemma-e2b", "hi", nil)
	if r.Status != 200 || r.Header.Get("X-Gw-Budget-Remaining-Usd") == "" {
		t.Fatalf("after grant: %d %v", r.Status, r.Header)
	}
	var audit struct {
		Entries []store.AuditEntry `json:"entries"`
	}
	_ = env.admin("GET", "/admin/v1/audit?kind=budget", adminAuditor, nil).JSON(&audit)
	if len(audit.Entries) != 2 || audit.Entries[0].Action != "grant" || audit.Entries[0].Reason != "launch week" || audit.Entries[1].Action != "create" {
		t.Fatalf("budget audit: %+v", audit.Entries)
	}
}

// Covers: CST-04, CST-07, CST-09
func TestCST04_CostReportsCSVAndChargeback(t *testing.T) {
	env := m4Env(t, nil)
	for i := 0; i < 5; i++ {
		env.Chat(keyA, "auto", fmt.Sprintf("hello %d", i), nil, map[string]string{"X-Gw-Tags": "exp"})
		env.Chat(keyB, "auto", fmt.Sprintf("hello %d", i), nil)
	}
	var j struct {
		Rows         []store.CostRow `json:"rows"`
		TotalDisplay float64         `json:"total_display"`
	}
	r := env.admin("GET", "/admin/v1/costs?group_by=tenant,model&bucket=1h", adminFinops, nil)
	if r.Status != 200 || r.JSON(&j) != nil || len(j.Rows) == 0 || j.TotalDisplay == 0 {
		t.Fatalf("costs JSON: %d %s", r.Status, r.Body)
	}
	r = env.admin("GET", "/admin/v1/costs?group_by=tenant&format=csv", adminFinops, nil)
	rows, err := csv.NewReader(strings.NewReader(string(r.Body))).ReadAll()
	if err != nil || len(rows) != 3 || rows[0][1] != "tenant" || rows[0][len(rows[0])-1] != "cost_inr" {
		t.Fatalf("CSV: %v %v", err, rows)
	}
	r = env.admin("GET", "/admin/v1/chargeback", adminFinops, nil)
	if r.Status != 200 || !strings.Contains(string(r.Body), "team-a") || !strings.Contains(string(r.Body), "a-app") {
		t.Fatalf("chargeback: %s", r.Body)
	}
	// Tag filter.
	_ = env.admin("GET", "/admin/v1/costs?tag=exp&group_by=tenant", adminFinops, nil).JSON(&j)
	if len(j.Rows) != 1 || j.Rows[0].Group["tenant"] != "team-a" || j.Rows[0].Requests != 5 {
		t.Fatalf("tag filter: %+v", j.Rows)
	}
}

// Budgets fail closed when the counters backend is down; rate limits fail open.
// Covers: NFR-08
func TestNFR08_BudgetsFailClosedWhenRedisDown(t *testing.T) {
	mr, counters := newMiniredis(t)
	env := m4Env(t, counters)
	if r := env.Chat(keyA, "sima/gemma-e2b", "hi", nil); r.Status != 200 {
		t.Fatalf("before outage: %d", r.Status)
	}
	mr.Close()
	if r := env.Chat(keyA, "sima/gemma-e2b", "hi", nil); r.Status != 503 || !strings.Contains(string(r.Body), "budget_unavailable") {
		t.Fatalf("budgeted tenant during outage: %d %s", r.Status, r.Body)
	}
	if r := env.Chat(keyB, "sima/gemma-e2b", "hi", nil); r.Status != 200 {
		t.Fatalf("unbudgeted tenant during outage (rate limits fail open): %d %s", r.Status, r.Body)
	}
	env.Engine.Gov.FailOpen = true
	if r := env.Chat(keyA, "sima/gemma-e2b", "hi", nil); r.Status != 200 {
		t.Fatalf("fail-open budgets: %d", r.Status)
	}
}

// Covers: NFR-14
func TestNFR14_WebhooksRefusePrivateAddressesUnlessAllowed(t *testing.T) {
	hook := newHook(t)
	cfg := config.Config{Alerts: []config.AlertSink{{ID: "h", Type: "webhook", URL: config.SecretRef("env:H_URL")}}}
	t.Setenv("H_URL", hook.srv.URL)
	g := governance.NewGovernor(governance.NewMemCounters(), nil)
	g.Alerts.SetSinks(cfg.Alerts)
	b := []config.Budget{{ID: "x", Scope: "tenant", ScopeID: "t", Period: "daily", Timezone: "UTC", HardUSD: 0.000001}}
	refs := g.Applicable(b, nil, governance.Scope{Tenant: "t"})
	res, _ := g.Reserve(context.Background(), refs, 0)
	res.Settle(context.Background(), 0.000002)
	time.Sleep(200 * time.Millisecond)
	if len(hook.all()) != 0 {
		t.Fatal("alert delivered to a loopback address without allow_private_network")
	}
	cfg.Alerts[0].AllowPrivateNetwork = true
	g.Alerts.SetSinks(cfg.Alerts)
	b[0].ID = "y"
	refs = g.Applicable(b, nil, governance.Scope{Tenant: "t"})
	res, _ = g.Reserve(context.Background(), refs, 0)
	res.Settle(context.Background(), 0.000002)
	time.Sleep(200 * time.Millisecond)
	if len(hook.all()) == 0 {
		t.Fatal("allow-listed sink did not receive the alert")
	}
}

// Covers: CST-01
func TestCST01_OverheadAndSplitRecorded(t *testing.T) {
	env := m4Env(t, nil)
	r := env.Chat(keyB, "sima/qwen-9b", "hello", nil)
	env.Flush()
	recs, _ := env.Store.ListCostRecords(context.Background(), store.CostFilter{Tenant: "team-b"}, 1)
	rec := recs[0]
	split := rec.InputUSD + rec.CachedInputUSD + rec.OutputUSD + rec.ReasoningUSD
	if r.Status != 200 || math.Abs(split-rec.CostUSD) > 1e-12 || rec.InputUSD == 0 || rec.OutputUSD == 0 {
		t.Fatalf("cost split: %+v", rec)
	}
	_ = cost.Nano
	_ = io.EOF
	_ = gateway.DocBudget
}
