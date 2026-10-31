// Package admin is the versioned REST admin API (/admin/v1) [ADM-01]. The console and
// CLI are clients of this API, so role checks here apply to all three [ADM-05].
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/api"
	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/console"
	"github.com/SureshKhemka/llm-gateway/internal/gateway"
	"github.com/SureshKhemka/llm-gateway/internal/governance"
	"github.com/SureshKhemka/llm-gateway/internal/store"
)

// Perm is one admin permission.
type Perm string

const (
	PermTenantsRead   Perm = "tenants:read"
	PermTenantsWrite  Perm = "tenants:write"
	PermKeysRead      Perm = "keys:read"
	PermKeysWrite     Perm = "keys:write"
	PermBudgetsRead   Perm = "budgets:read"
	PermBudgetsWrite  Perm = "budgets:write"
	PermCostRead      Perm = "costs:read"
	PermRequestsRead  Perm = "requests:read"
	PermAuditRead     Perm = "audit:read"
	PermConfigRead    Perm = "config:read"
	PermConfigWrite   Perm = "config:write"
	PermPoliciesRead  Perm = "policies:read"
	PermPoliciesWrite Perm = "policies:write"
	PermCacheWrite    Perm = "cache:write"
	PermHealthRead    Perm = "health:read"
	PermEvalsRead     Perm = "evals:read"
	PermEvalsRun      Perm = "evals:run"
	PermCatalogWrite  Perm = "catalog:write"
	PermPlayground    Perm = "playground:use"
)

// RolePerms is the single permission table for API, console and CLI [TEN-04, ADM-05].
var RolePerms = map[string][]Perm{
	"org_admin": {PermTenantsRead, PermTenantsWrite, PermKeysRead, PermKeysWrite, PermBudgetsRead, PermBudgetsWrite,
		PermCostRead, PermRequestsRead, PermAuditRead, PermConfigRead, PermConfigWrite, PermPoliciesRead, PermPoliciesWrite,
		PermCacheWrite, PermHealthRead, PermEvalsRead, PermEvalsRun, PermCatalogWrite, PermPlayground},
	// Tenant admins manage their own tenant only.
	"tenant_admin": {PermTenantsRead, PermKeysRead, PermKeysWrite, PermBudgetsRead, PermBudgetsWrite, PermCostRead,
		PermRequestsRead, PermPoliciesRead, PermCacheWrite, PermHealthRead, PermEvalsRead, PermPlayground},
	"finops_viewer": {PermTenantsRead, PermBudgetsRead, PermCostRead, PermHealthRead},
	"auditor":       {PermTenantsRead, PermAuditRead, PermConfigRead, PermKeysRead, PermPoliciesRead, PermBudgetsRead, PermHealthRead},
}

func (a *Admin) Can(p Perm) bool {
	for _, x := range RolePerms[a.Role] {
		if x == p {
			return true
		}
	}
	return false
}

// Admin is an authenticated admin caller.
type Admin struct {
	ID     string `json:"id"`
	Role   string `json:"role"`
	Tenant string `json:"tenant,omitempty"` // set for tenant_admin: the only tenant it may see
}

// CanTenant reports whether the admin may see tenant t [TEN-06].
func (a *Admin) CanTenant(t string) bool { return a.Tenant == "" || a.Tenant == t }

// API serves /admin/v1.
type API struct {
	Engine *gateway.Engine
	// Reload re-reads configuration files and rebuilds the snapshot (config apply).
	Reload func(ctx context.Context, reason string) error
	// Diff previews what a reload would change [CFG-05].
	Diff     func(ctx context.Context) (map[string]any, error)
	mux      *http.ServeMux
	route    []Route
	throttle *authThrottle
}

type CacheInvalidation struct {
	Tenant    string `json:"tenant"`
	Policy    string `json:"policy"`
	Model     string `json:"model"`
	OlderThan string `json:"older_than"`
}

// Route documents one endpoint for the OpenAPI document.
type Route struct {
	Method, Path, Summary string
	Perm                  Perm
	h                     func(w http.ResponseWriter, r *http.Request, a *Admin)
}

func New(e *gateway.Engine) *API {
	x := &API{Engine: e, mux: http.NewServeMux(), throttle: newAuthThrottle()}
	x.routes()
	for _, rt := range x.route {
		rt := rt
		x.mux.HandleFunc(rt.Method+" "+rt.Path, func(w http.ResponseWriter, r *http.Request) {
			securityHeaders(w)
			addr := remoteHost(r)
			if wait, locked := x.throttle.blocked(addr); locked {
				w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
				api.WriteError(w, api.FormatOpenAI, api.NewError(api.CodeRateLimited, "too many failed admin authentications"))
				return
			}
			adm, err := x.authenticate(r)
			if err != nil {
				x.throttle.fail(addr)
				x.Engine.Log.Warn("admin authentication failed", "remote", addr, "path", r.URL.Path)
				api.WriteError(w, api.FormatOpenAI, err)
				return
			}
			x.throttle.succeed(addr)
			if rt.Perm != "" && !adm.Can(rt.Perm) {
				api.WriteError(w, api.FormatOpenAI, api.NewError(api.CodeForbidden, "role %s lacks permission %s", adm.Role, rt.Perm))
				return
			}
			rt.h(w, r, adm)
		})
	}
	x.mux.HandleFunc("GET /admin/v1/openapi.json", x.openapi)
	return x
}

func (x *API) Handler() http.Handler { return x.mux }

// securityHeaders are set on every admin response [NFR-13].
func securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
}

// Mount registers the admin API and the console on mux.
func (x *API) Mount(mux *http.ServeMux) {
	mux.Handle("/admin/", x.mux)
	console.Mount(mux)
}

func (x *API) add(method, path, summary string, perm Perm, h func(w http.ResponseWriter, r *http.Request, a *Admin)) {
	x.route = append(x.route, Route{Method: method, Path: path, Summary: summary, Perm: perm, h: h})
}

// Routes exposes the route table (OpenAPI, console navigation, CLI help).
func (x *API) Routes() []Route { return x.route }

func (x *API) authenticate(r *http.Request) (*Admin, *api.Error) {
	tok := ""
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		tok = strings.TrimSpace(h[7:])
	}
	if tok == "" {
		return nil, api.NewError(api.CodeUnauthorized, "admin credentials required")
	}
	s := x.Engine.Snapshot()
	cred := s.Admins[string(governance.HashKey(tok))]
	if cred == nil {
		return nil, api.NewError(api.CodeUnauthorized, "invalid admin credentials")
	}
	a := &Admin{ID: cred.ID, Role: cred.Role}
	if cred.Role == "tenant_admin" {
		a.Tenant = cred.Tenant
	}
	return a, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func fail(w http.ResponseWriter, code, format string, args ...any) {
	api.WriteError(w, api.FormatOpenAI, api.NewError(code, format, args...))
}

func decode(r *http.Request, v any) *api.Error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return api.NewError(api.CodeInvalidRequest, "invalid body: %v", err)
	}
	return nil
}

// audit appends an entry; before/after are marshalled with secrets already removed.
func (x *API) audit(ctx context.Context, a *Admin, tenant, action, kind, id string, before, after any, reason string) {
	enc := func(v any) json.RawMessage {
		if v == nil {
			return nil
		}
		b, _ := json.Marshal(v)
		return b
	}
	_, err := x.Engine.Store.AppendAudit(ctx, store.AuditEntry{Time: time.Now().UTC(), Actor: a.ID, Role: a.Role, Tenant: tenant,
		Action: action, ObjectKind: kind, ObjectID: id, Before: enc(before), After: enc(after), Reason: reason})
	if err != nil {
		x.Engine.Log.Error("audit append failed", "err", err, "action", action, "object", kind+"/"+id)
	}
}

func (x *API) rebuild(ctx context.Context, reason string) error {
	return x.Engine.Rebuild(ctx, nil, reason)
}

// tenantParam resolves the tenant a request is about and enforces the binding.
func (x *API) tenantParam(w http.ResponseWriter, a *Admin, t string) (string, bool) {
	if t == "" {
		t = a.Tenant
	}
	if !a.CanTenant(t) {
		fail(w, api.CodeNotFound, "tenant %q not found", t)
		return "", false
	}
	return t, true
}

func fileManaged(s *gateway.Snapshot, kind, id string) bool {
	fc := s.FileConfig
	switch kind {
	case gateway.DocTenant:
		for _, t := range fc.Tenants {
			if t.ID == id {
				return true
			}
		}
	case gateway.DocBudget:
		for _, b := range fc.Budgets {
			if b.ID == id {
				return true
			}
		}
	case gateway.DocProvider:
		for _, p := range fc.Providers {
			if p.ID == id {
				return true
			}
		}
	case gateway.DocDeployment:
		for _, d := range fc.Deployments {
			if d.ID == id {
				return true
			}
		}
	case gateway.DocProject:
		for _, t := range fc.Tenants {
			for _, p := range t.Projects {
				if p.ID == id {
					return true
				}
			}
		}
	}
	return false
}

var errManagedByFile = errors.New("managed_by_file")

func managedConflict(w http.ResponseWriter, kind, id string) {
	e := api.NewError(api.CodeConflict, "%s %q is defined in a configuration file; change it there (files win over the admin API)", kind, id)
	api.WriteError(w, api.FormatOpenAI, e)
}

// redactedConfig returns the effective config without secret references' targets.
// Secret refs are references, never values, so they are safe to show [PRV-08].
func redactedConfig(c *config.Config) *config.Config { return c }
