package admin

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/gateway"
	"github.com/SureshKhemka/llm-gateway/internal/governance"
	"github.com/SureshKhemka/llm-gateway/internal/store"
)

func (x *API) routes() {
	x.add("GET", "/admin/v1/whoami", "The calling admin's identity and permissions", "", x.whoami)

	x.add("GET", "/admin/v1/tenants", "List tenants (tenant admins see their own)", PermTenantsRead, x.listTenants)
	x.add("POST", "/admin/v1/tenants", "Create a tenant", PermTenantsWrite, x.createTenant)
	x.add("POST", "/admin/v1/tenants/{tenant}/projects", "Create a project in a tenant", PermKeysWrite, x.createProject)

	x.add("GET", "/admin/v1/keys", "List virtual keys (metadata only)", PermKeysRead, x.listKeys)
	x.add("POST", "/admin/v1/keys", "Create a virtual key; the secret is returned once", PermKeysWrite, x.createKey)
	x.add("POST", "/admin/v1/keys/{id}/rotate", "Rotate a key with an overlap window", PermKeysWrite, x.rotateKey)
	x.add("POST", "/admin/v1/keys/{id}/revoke", "Revoke a key (effective on every instance within seconds)", PermKeysWrite, x.revokeKey)

	x.add("GET", "/admin/v1/budgets", "List budgets with current-period status", PermBudgetsRead, x.listBudgets)
	x.add("POST", "/admin/v1/budgets", "Create or update an API-managed budget", PermBudgetsWrite, x.putBudget)
	x.add("POST", "/admin/v1/budgets/{id}/grants", "Grant a temporary budget increase", PermBudgetsWrite, x.grantBudget)

	x.add("GET", "/admin/v1/costs", "Aggregate cost, tokens and savings (JSON or CSV)", PermCostRead, x.costs)
	x.add("GET", "/admin/v1/chargeback", "Monthly chargeback report per tenant", PermCostRead, x.chargeback)
	x.add("GET", "/admin/v1/requests", "Recent requests (request explorer)", PermRequestsRead, x.listRequests)
	x.add("GET", "/admin/v1/requests/{id}", "One request's cost record and route decision", PermRequestsRead, x.getRequest)

	x.add("GET", "/admin/v1/audit", "Audit log", PermAuditRead, x.listAudit)
	x.add("GET", "/admin/v1/health", "Deployment health and circuit breakers", PermHealthRead, x.health)
	x.add("GET", "/admin/v1/config", "Effective configuration (secret references only)", PermConfigRead, x.getConfig)
	x.add("POST", "/admin/v1/config/reload", "Re-read configuration files", PermConfigWrite, x.reload)
	x.add("POST", "/admin/v1/cache/invalidate", "Invalidate cache entries", PermCacheWrite, x.invalidateCache)
}

func (x *API) whoami(w http.ResponseWriter, _ *http.Request, a *Admin) {
	writeJSON(w, 200, map[string]any{"admin": a, "permissions": RolePerms[a.Role]})
}

// ---------- tenants ----------

func (x *API) listTenants(w http.ResponseWriter, _ *http.Request, a *Admin) {
	s := x.Engine.Snapshot()
	var out []map[string]any
	for _, t := range s.Config.Tenants {
		if !a.CanTenant(t.ID) {
			continue
		}
		managed := "api"
		if fileManaged(s, gateway.DocTenant, t.ID) {
			managed = "file"
		}
		var projects []string
		for _, p := range t.Projects {
			projects = append(projects, p.ID)
		}
		out = append(out, map[string]any{"id": t.ID, "name": t.Name, "policy": t.Policy, "projects": projects, "managed": managed})
	}
	writeJSON(w, 200, map[string]any{"tenants": out})
}

func (x *API) createTenant(w http.ResponseWriter, r *http.Request, a *Admin) {
	var t config.Tenant
	if err := decode(r, &t); err != nil {
		api.WriteError(w, api.FormatOpenAI, err)
		return
	}
	s := x.Engine.Snapshot()
	if t.ID == "" {
		fail(w, api.CodeInvalidRequest, "id is required")
		return
	}
	if fileManaged(s, gateway.DocTenant, t.ID) {
		managedConflict(w, "tenant", t.ID)
		return
	}
	for _, p := range t.Projects {
		if len(p.Keys) > 0 {
			fail(w, api.CodeInvalidRequest, "create keys with POST /admin/v1/keys")
			return
		}
	}
	if err := x.putDoc(r.Context(), gateway.DocTenant, t.ID, t.ID, t); err != nil {
		fail(w, api.CodeInvalidRequest, "%v", err)
		return
	}
	x.audit(r.Context(), a, t.ID, "create", "tenant", t.ID, nil, t, "")
	writeJSON(w, 201, t)
}

func (x *API) createProject(w http.ResponseWriter, r *http.Request, a *Admin) {
	tenant, ok := x.tenantParam(w, a, r.PathValue("tenant"))
	if !ok {
		return
	}
	var p config.Project
	if err := decode(r, &p); err != nil {
		api.WriteError(w, api.FormatOpenAI, err)
		return
	}
	s := x.Engine.Snapshot()
	if s.Tenants[tenant] == nil {
		fail(w, api.CodeNotFound, "tenant %q not found", tenant)
		return
	}
	if p.ID == "" || s.Projects[p.ID] != nil {
		fail(w, api.CodeConflict, "project id must be set and unique")
		return
	}
	if err := x.putDoc(r.Context(), gateway.DocProject, p.ID, tenant, gateway.ProjectDoc{Tenant: tenant, Project: p}); err != nil {
		fail(w, api.CodeInvalidRequest, "%v", err)
		return
	}
	x.audit(r.Context(), a, tenant, "create", "project", p.ID, nil, p, "")
	writeJSON(w, 201, p)
}

// putDoc stores an object and rebuilds the snapshot; a rebuild failure (the object does
// not validate together with the files) rolls the store change back [CFG-02].
func (x *API) putDoc(ctx context.Context, kind, id, tenant string, v any) error {
	prev, _ := x.Engine.Store.GetDoc(ctx, kind, id)
	b, _ := json.Marshal(v)
	if err := x.Engine.Store.PutDoc(ctx, store.Doc{Kind: kind, ID: id, Tenant: tenant, Body: b}); err != nil {
		return err
	}
	if err := x.rebuild(ctx, "admin:"+kind); err != nil {
		if prev != nil {
			_ = x.Engine.Store.PutDoc(ctx, *prev)
		} else {
			_ = x.Engine.Store.DeleteDoc(ctx, kind, id)
		}
		return err
	}
	return nil
}

// ---------- keys [TEN-02, TEN-03] ----------

type keyView struct {
	store.KeyRecord
	Scopes config.Scopes `json:"scopes"`
	Valid  bool          `json:"valid"`
}

func view(e *governance.KeyEntry) keyView {
	return keyView{KeyRecord: e.Record, Scopes: e.Scopes, Valid: e.Valid(time.Now())}
}

func (x *API) listKeys(w http.ResponseWriter, r *http.Request, a *Admin) {
	tenant, ok := x.tenantParam(w, a, r.URL.Query().Get("tenant"))
	if !ok {
		return
	}
	var out []keyView
	for _, e := range x.Engine.Snapshot().Keys.All() {
		if tenant == "" || e.Record.Tenant == tenant {
			v := view(e)
			if x.Engine.Gov.IsRevoked(e.Record.ID) {
				v.Valid = false
			}
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, 200, map[string]any{"keys": out})
}

type createKeyReq struct {
	Tenant    string        `json:"tenant"`
	Project   string        `json:"project"`
	Name      string        `json:"name"`
	Scopes    config.Scopes `json:"scopes"`
	ExpiresAt *time.Time    `json:"expires_at"`
	Env       string        `json:"env"`
}

func (x *API) createKey(w http.ResponseWriter, r *http.Request, a *Admin) {
	var req createKeyReq
	if err := decode(r, &req); err != nil {
		api.WriteError(w, api.FormatOpenAI, err)
		return
	}
	tenant, ok := x.tenantParam(w, a, req.Tenant)
	if !ok {
		return
	}
	s := x.Engine.Snapshot()
	p := s.Projects[req.Project]
	if s.Tenants[tenant] == nil || p == nil || !projectIn(s.Tenants[tenant], req.Project) {
		fail(w, api.CodeInvalidRequest, "tenant/project not found")
		return
	}
	if req.Scopes.Policy != "" && s.Policies[req.Scopes.Policy] == nil {
		fail(w, api.CodeInvalidRequest, "unknown policy %q", req.Scopes.Policy)
		return
	}
	env := req.Env
	if env == "" {
		env = "live"
	}
	secret, id := governance.GenerateKey(env)
	rec := x.newKeyRecord(secret, id, tenant, req.Project, req.Name, req.Scopes, req.ExpiresAt)
	if err := x.Engine.Store.PutKey(r.Context(), rec); err != nil {
		fail(w, api.CodeInternal, "%v", err)
		return
	}
	if err := x.rebuild(r.Context(), "admin:key"); err != nil {
		fail(w, api.CodeInternal, "%v", err)
		return
	}
	x.audit(r.Context(), a, tenant, "create", "key", id, nil, rec, "")
	writeJSON(w, 201, map[string]any{"key": secret, "record": rec, "note": "store the key now: it is not retrievable later"})
}

func projectIn(t *config.Tenant, project string) bool {
	for _, p := range t.Projects {
		if p.ID == project {
			return true
		}
	}
	return false
}

func (x *API) newKeyRecord(secret, id, tenant, project, name string, sc config.Scopes, exp *time.Time) store.KeyRecord {
	scopes, _ := json.Marshal(sc)
	return store.KeyRecord{ID: id, Prefix: governance.KeyPrefix(secret), Hash: governance.HashKey(secret), Tenant: tenant,
		Project: project, Name: name, Scopes: scopes, CreatedAt: time.Now().UTC(), ExpiresAt: exp, Managed: "api"}
}

func (x *API) keyFor(w http.ResponseWriter, a *Admin, id string) *governance.KeyEntry {
	e := x.Engine.Snapshot().Keys.ByID(id)
	if e == nil || !a.CanTenant(e.Record.Tenant) {
		fail(w, api.CodeNotFound, "key %q not found", id)
		return nil
	}
	return e
}

func (x *API) rotateKey(w http.ResponseWriter, r *http.Request, a *Admin) {
	e := x.keyFor(w, a, r.PathValue("id"))
	if e == nil {
		return
	}
	var req struct {
		OverlapSeconds int `json:"overlap_seconds"`
	}
	_ = decode(r, &req)
	if req.OverlapSeconds <= 0 {
		req.OverlapSeconds = 3600
	}
	secret, id := governance.GenerateKey("live")
	rec := x.newKeyRecord(secret, id, e.Record.Tenant, e.Record.Project, e.Record.Name, e.Scopes, e.Record.ExpiresAt)
	rec.RotatedFrom = e.Record.ID
	until := time.Now().UTC().Add(time.Duration(req.OverlapSeconds) * time.Second)
	old := e.Record
	if old.Managed == "file" {
		// Bootstrap keys stay in the file; the overlap is recorded as a scheduled revocation.
		_ = x.putDoc(r.Context(), gateway.DocKeyRevocation, old.ID, old.Tenant, gateway.KeyRevocation{KeyID: old.ID, At: until})
	} else {
		old.OverlapUntil = &until
		if err := x.Engine.Store.PutKey(r.Context(), old); err != nil {
			fail(w, api.CodeInternal, "%v", err)
			return
		}
	}
	if err := x.Engine.Store.PutKey(r.Context(), rec); err != nil {
		fail(w, api.CodeInternal, "%v", err)
		return
	}
	if err := x.rebuild(r.Context(), "admin:key_rotate"); err != nil {
		fail(w, api.CodeInternal, "%v", err)
		return
	}
	x.audit(r.Context(), a, rec.Tenant, "rotate", "key", old.ID, map[string]any{"id": old.ID}, map[string]any{"new_id": id, "old_valid_until": until}, "")
	writeJSON(w, 201, map[string]any{"key": secret, "record": rec, "old_key_valid_until": until})
}

func (x *API) revokeKey(w http.ResponseWriter, r *http.Request, a *Admin) {
	e := x.keyFor(w, a, r.PathValue("id"))
	if e == nil {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	_ = decode(r, &req)
	now := time.Now().UTC()
	// Fast path first: every instance stops accepting the key within milliseconds.
	if err := x.Engine.Gov.Revoke(r.Context(), e.Record.ID); err != nil {
		x.Engine.Log.Warn("revocation broadcast failed; instances pick it up on the next reload", "err", err)
	}
	rec := e.Record
	if rec.Managed == "file" {
		_ = x.putDoc(r.Context(), gateway.DocKeyRevocation, rec.ID, rec.Tenant, gateway.KeyRevocation{KeyID: rec.ID, At: now})
	} else {
		rec.RevokedAt = &now
		if err := x.Engine.Store.PutKey(r.Context(), rec); err != nil {
			fail(w, api.CodeInternal, "%v", err)
			return
		}
		_ = x.rebuild(r.Context(), "admin:key_revoke")
	}
	x.audit(r.Context(), a, rec.Tenant, "revoke", "key", rec.ID, nil, map[string]any{"revoked_at": now}, req.Reason)
	writeJSON(w, 200, map[string]any{"id": rec.ID, "revoked_at": now})
}

// ---------- budgets [BUD-01, BUD-06, BUD-08] ----------

func (x *API) budgetTenant(s *gateway.Snapshot, b *config.Budget) string {
	switch b.Scope {
	case "tenant", "end_user", "eval", "shadow":
		return b.ScopeID
	case "project":
		for _, t := range s.Config.Tenants {
			if projectIn(&t, b.ScopeID) {
				return t.ID
			}
		}
	case "key":
		if k := s.Keys.ByID(b.ScopeID); k != nil {
			return k.Record.Tenant
		}
	}
	return ""
}

func (x *API) listBudgets(w http.ResponseWriter, r *http.Request, a *Admin) {
	s := x.Engine.Snapshot()
	var out []any
	for i := range s.Config.Budgets {
		b := &s.Config.Budgets[i]
		if !a.CanTenant(x.budgetTenant(s, b)) {
			continue
		}
		refs := x.Engine.Gov.Applicable([]config.Budget{*b}, s.Grants, scopeFor(b))
		st, err := x.Engine.Gov.Status(r.Context(), refs)
		entry := map[string]any{"budget": b, "managed": map[bool]string{true: "file", false: "api"}[fileManaged(s, gateway.DocBudget, b.ID)]}
		if err == nil && len(st) > 0 {
			entry["status"] = st[0]
		}
		var grants []governance.Grant
		for _, g := range s.Grants {
			if g.BudgetID == b.ID {
				grants = append(grants, g)
			}
		}
		entry["grants"] = grants
		out = append(out, entry)
	}
	writeJSON(w, 200, map[string]any{"budgets": out})
}

// scopeFor builds a Scope that matches budget b (for status queries).
func scopeFor(b *config.Budget) governance.Scope {
	switch b.Scope {
	case "tenant":
		return governance.Scope{Tenant: b.ScopeID}
	case "project":
		return governance.Scope{Project: b.ScopeID}
	case "key":
		return governance.Scope{Key: b.ScopeID}
	case "eval":
		return governance.Scope{Tenant: b.ScopeID, Eval: true}
	case "shadow":
		return governance.Scope{Tenant: b.ScopeID, Shadow: true}
	}
	return governance.Scope{}
}

func (x *API) putBudget(w http.ResponseWriter, r *http.Request, a *Admin) {
	var b config.Budget
	if err := decode(r, &b); err != nil {
		api.WriteError(w, api.FormatOpenAI, err)
		return
	}
	s := x.Engine.Snapshot()
	if !a.CanTenant(x.budgetTenant(s, &b)) || b.Scope == "" {
		fail(w, api.CodeForbidden, "budget scope outside your tenant")
		return
	}
	if fileManaged(s, gateway.DocBudget, b.ID) {
		managedConflict(w, "budget", b.ID)
		return
	}
	var before any
	for _, ob := range s.Config.Budgets {
		if ob.ID == b.ID {
			before = ob
		}
	}
	if err := x.putDoc(r.Context(), gateway.DocBudget, b.ID, x.budgetTenant(s, &b), b); err != nil {
		fail(w, api.CodeInvalidRequest, "%v", err)
		return
	}
	x.audit(r.Context(), a, x.budgetTenant(s, &b), map[bool]string{true: "update", false: "create"}[before != nil], "budget", b.ID, before, b, "")
	writeJSON(w, 200, b)
}

func (x *API) grantBudget(w http.ResponseWriter, r *http.Request, a *Admin) {
	s := x.Engine.Snapshot()
	id := r.PathValue("id")
	var b *config.Budget
	for i := range s.Config.Budgets {
		if s.Config.Budgets[i].ID == id {
			b = &s.Config.Budgets[i]
		}
	}
	if b == nil || !a.CanTenant(x.budgetTenant(s, b)) {
		fail(w, api.CodeNotFound, "budget %q not found", id)
		return
	}
	var req struct {
		ExtraUSD  float64   `json:"extra_usd"`
		ExpiresAt time.Time `json:"expires_at"`
		Reason    string    `json:"reason"`
	}
	if err := decode(r, &req); err != nil {
		api.WriteError(w, api.FormatOpenAI, err)
		return
	}
	if req.ExtraUSD <= 0 || !req.ExpiresAt.After(time.Now()) || strings.TrimSpace(req.Reason) == "" {
		fail(w, api.CodeInvalidRequest, "extra_usd > 0, a future expires_at and a reason are required")
		return
	}
	g := governance.Grant{ID: fmt.Sprintf("grant-%d", time.Now().UnixNano()), BudgetID: id, ExtraUSD: req.ExtraUSD,
		ExpiresAt: req.ExpiresAt, Actor: a.ID, Reason: req.Reason}
	if err := x.putDoc(r.Context(), gateway.DocGrant, g.ID, x.budgetTenant(s, b), g); err != nil {
		fail(w, api.CodeInternal, "%v", err)
		return
	}
	x.audit(r.Context(), a, x.budgetTenant(s, b), "grant", "budget", id, nil, g, req.Reason)
	writeJSON(w, 201, g)
}

// ---------- costs [CST-04, CST-07, CST-09] ----------

func parseFilter(r *http.Request) (store.CostFilter, error) {
	q := r.URL.Query()
	f := store.CostFilter{Tenant: q.Get("tenant"), Project: q.Get("project"), KeyID: q.Get("key"), EndUser: q.Get("end_user"),
		Deployment: q.Get("model"), Provider: q.Get("provider"), Policy: q.Get("policy"), Tag: q.Get("tag"), EvalRun: q.Get("eval_run"),
		IncludeShadow: q.Get("include_shadow") == "true"}
	for name, dst := range map[string]*time.Time{"from": &f.From, "to": &f.To} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				if d, err2 := time.Parse("2006-01-02", v); err2 == nil {
					t = d
				} else {
					return f, fmt.Errorf("%s must be RFC 3339 or YYYY-MM-DD", name)
				}
			}
			*dst = t
		}
	}
	return f, nil
}

func (x *API) costs(w http.ResponseWriter, r *http.Request, a *Admin) {
	f, err := parseFilter(r)
	if err != nil {
		fail(w, api.CodeInvalidRequest, "%v", err)
		return
	}
	tenant, ok := x.tenantParam(w, a, f.Tenant)
	if !ok {
		return
	}
	f.Tenant = tenant
	q := store.CostQuery{Filter: f}
	if g := r.URL.Query().Get("group_by"); g != "" {
		q.GroupBy = strings.Split(g, ",")
	}
	if b := r.URL.Query().Get("bucket"); b != "" {
		d, err := time.ParseDuration(b)
		if err != nil || d < time.Minute {
			fail(w, api.CodeInvalidRequest, "bucket must be a duration ≥ 1m (e.g. 1h, 24h)")
			return
		}
		q.Bucket = d
	}
	x.Engine.Writer.Flush()
	rows, err := x.Engine.Store.QueryCost(r.Context(), q)
	if err != nil {
		fail(w, api.CodeInvalidRequest, "%v", err)
		return
	}
	cfg := x.Engine.Snapshot().Config.Defaults
	if r.URL.Query().Get("format") == "csv" {
		writeCostCSV(w, q, rows, cfg)
		return
	}
	resp := map[string]any{"rows": rows, "group_by": q.GroupBy, "currency": "USD"}
	if cfg.Currency != "USD" && cfg.ExchangeRate > 0 {
		var total float64
		for _, r := range rows {
			total += r.CostUSD
		}
		resp["display_currency"] = cfg.Currency
		resp["exchange_rate"] = cfg.ExchangeRate
		resp["total_display"] = total * cfg.ExchangeRate
	}
	writeJSON(w, 200, resp)
}

func writeCostCSV(w http.ResponseWriter, q store.CostQuery, rows []store.CostRow, d config.RoutingDefaults) {
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="costs.csv"`)
	cw := csv.NewWriter(w)
	head := []string{"bucket"}
	head = append(head, q.GroupBy...)
	head = append(head, "requests", "input_tokens", "cached_input_tokens", "output_tokens", "reasoning_tokens", "cost_usd",
		"baseline_cost_usd", "routing_savings_usd", "cache_savings_usd", "cache_hits", "errors")
	display := d.Currency != "USD" && d.ExchangeRate > 0
	if display {
		head = append(head, "cost_"+strings.ToLower(d.Currency))
	}
	_ = cw.Write(head)
	for _, r := range rows {
		rec := []string{""}
		if !r.Bucket.IsZero() {
			rec[0] = r.Bucket.Format(time.RFC3339)
		}
		for _, g := range q.GroupBy {
			rec = append(rec, r.Group[g])
		}
		f := func(v float64) string { return strconv.FormatFloat(v, 'f', 10, 64) }
		i := func(v int64) string { return strconv.FormatInt(v, 10) }
		rec = append(rec, i(r.Requests), i(r.InputTokens), i(r.CachedTokens), i(r.OutputTokens), i(r.ReasoningToks), f(r.CostUSD),
			f(r.BaselineUSD), f(r.RoutingSavings), f(r.CacheSavings), i(r.CacheHits), i(r.Errors))
		if display {
			rec = append(rec, f(r.CostUSD*d.ExchangeRate))
		}
		_ = cw.Write(rec)
	}
	cw.Flush()
}

func (x *API) chargeback(w http.ResponseWriter, r *http.Request, a *Admin) {
	month := r.URL.Query().Get("month")
	start, err := time.Parse("2006-01", month)
	if err != nil {
		now := time.Now().UTC()
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	f := store.CostFilter{From: start, To: start.AddDate(0, 1, 0)}
	tenant, ok := x.tenantParam(w, a, r.URL.Query().Get("tenant"))
	if !ok {
		return
	}
	f.Tenant = tenant
	x.Engine.Writer.Flush()
	byTenant, err := x.Engine.Store.QueryCost(r.Context(), store.CostQuery{Filter: f, GroupBy: []string{"tenant", "project"}})
	if err != nil {
		fail(w, api.CodeInternal, "%v", err)
		return
	}
	type line struct {
		Project string  `json:"project"`
		CostUSD float64 `json:"cost_usd"`
		Saved   float64 `json:"savings_usd"`
		Reqs    int64   `json:"requests"`
	}
	report := map[string]map[string]any{}
	for _, row := range byTenant {
		t := row.Group["tenant"]
		if report[t] == nil {
			report[t] = map[string]any{"tenant": t, "total_usd": 0.0, "savings_usd": 0.0, "projects": []line{}}
		}
		rep := report[t]
		rep["total_usd"] = rep["total_usd"].(float64) + row.CostUSD
		rep["savings_usd"] = rep["savings_usd"].(float64) + row.RoutingSavings + row.CacheSavings
		rep["projects"] = append(rep["projects"].([]line), line{row.Group["project"], row.CostUSD, row.RoutingSavings + row.CacheSavings, row.Requests})
	}
	var out []any
	for _, v := range report {
		out = append(out, v)
	}
	writeJSON(w, 200, map[string]any{"month": start.Format("2006-01"), "tenants": out})
}

// ---------- requests ----------

func (x *API) listRequests(w http.ResponseWriter, r *http.Request, a *Admin) {
	f, err := parseFilter(r)
	if err != nil {
		fail(w, api.CodeInvalidRequest, "%v", err)
		return
	}
	tenant, ok := x.tenantParam(w, a, f.Tenant)
	if !ok {
		return
	}
	f.Tenant = tenant
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	x.Engine.Writer.Flush()
	recs, err := x.Engine.Store.ListCostRecords(r.Context(), f, limit)
	if err != nil {
		fail(w, api.CodeInternal, "%v", err)
		return
	}
	writeJSON(w, 200, map[string]any{"requests": recs})
}

func (x *API) getRequest(w http.ResponseWriter, r *http.Request, a *Admin) {
	id := r.PathValue("id")
	x.Engine.Writer.Flush()
	dec, err := x.Engine.Store.GetDecision(r.Context(), id)
	if err != nil || !a.CanTenant(dec.Tenant) {
		fail(w, api.CodeNotFound, "request %q not found", id)
		return
	}
	recs, _ := x.Engine.Store.ListCostRecords(r.Context(), store.CostFilter{Tenant: dec.Tenant, IncludeShadow: true}, 0)
	var rec *store.CostRecord
	for i := range recs {
		if recs[i].RequestID == id {
			rec = &recs[i]
		}
	}
	writeJSON(w, 200, map[string]any{"cost_record": rec, "decision": json.RawMessage(dec.Body)})
}

// ---------- audit, health, config ----------

func (x *API) listAudit(w http.ResponseWriter, r *http.Request, a *Admin) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := x.Engine.Store.ListAudit(r.Context(), store.AuditFilter{Tenant: r.URL.Query().Get("tenant"), ObjectKind: r.URL.Query().Get("kind"), Limit: limit})
	if err != nil {
		fail(w, api.CodeInternal, "%v", err)
		return
	}
	writeJSON(w, 200, map[string]any{"entries": entries})
}

func (x *API) health(w http.ResponseWriter, _ *http.Request, _ *Admin) {
	s := x.Engine.Snapshot()
	var out []any
	for _, id := range s.Catalog.Order {
		d := s.Catalog.Deployments[id]
		st := x.Engine.Health.Status(id)
		entry := map[string]any{"deployment": id, "provider": d.Provider, "enabled": d.Enabled, "approved": d.Approved,
			"tier": d.Tier, "health": st, "in_flight": x.Engine.Limiter.InFlight(id), "queued": x.Engine.Limiter.Queued(id)}
		if ttft, tps, ok := x.Engine.Latency.Observed(id); ok {
			entry["observed_ttft_ms"], entry["observed_tps"] = ttft, tps
		}
		out = append(out, entry)
	}
	writeJSON(w, 200, map[string]any{"deployments": out, "snapshot_version": s.Version, "loaded": s.Loaded})
}

func (x *API) getConfig(w http.ResponseWriter, _ *http.Request, _ *Admin) {
	writeJSON(w, 200, redactedConfig(x.Engine.Snapshot().Config))
}

func (x *API) reload(w http.ResponseWriter, r *http.Request, a *Admin) {
	if x.Reload == nil {
		fail(w, api.CodeInvalidRequest, "this gateway was not started from configuration files")
		return
	}
	before := x.Engine.Snapshot().Version
	if err := x.Reload(r.Context(), "admin:reload"); err != nil {
		writeJSON(w, 422, map[string]any{"error": map[string]any{"code": api.CodeInvalidRequest, "message": err.Error()}, "applied": false})
		return
	}
	x.audit(r.Context(), a, "", "reload", "config", "files", map[string]any{"snapshot": before}, map[string]any{"snapshot": x.Engine.Snapshot().Version}, "")
	writeJSON(w, 200, map[string]any{"applied": true, "snapshot_version": x.Engine.Snapshot().Version})
}

func (x *API) invalidateCache(w http.ResponseWriter, r *http.Request, a *Admin) {
	var req CacheInvalidation
	if err := decode(r, &req); err != nil {
		api.WriteError(w, api.FormatOpenAI, err)
		return
	}
	tenant, ok := x.tenantParam(w, a, req.Tenant)
	if !ok {
		return
	}
	req.Tenant = tenant
	if x.Hooks.InvalidateCache == nil {
		writeJSON(w, 200, map[string]any{"invalidated": 0})
		return
	}
	n, err := x.Hooks.InvalidateCache(r.Context(), req)
	if err != nil {
		fail(w, api.CodeInvalidRequest, "%v", err)
		return
	}
	x.audit(r.Context(), a, tenant, "invalidate", "cache", tenant, nil, req, "")
	writeJSON(w, 200, map[string]any{"invalidated": n})
}
