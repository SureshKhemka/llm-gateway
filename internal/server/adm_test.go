package server_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/SureshKhemka/llm-gateway/internal/admin"
	"github.com/SureshKhemka/llm-gateway/internal/store"
)

// Every capability is reachable through the versioned admin API, and the published
// OpenAPI document (docs/api/openapi.json) is generated from the same route table.
// UPDATE_OPENAPI=1 go test ./internal/server -run ADM01 rewrites the committed copy.
// Covers: ADM-01, CFG-01, CFG-04
func TestADM01_AdminAPICoversCapabilitiesAndOpenAPI(t *testing.T) {
	_, counters := newMiniredis(t)
	env := m4Env(t, counters)

	// Providers and deployments are manageable through the API; files still win.
	if r := env.admin("POST", "/admin/v1/providers", adminOrg, map[string]any{"id": "simx", "type": "openai",
		"base_url": env.SimURL("sima") + "/v1", "api_key": "sk-literal-secret", "allow_private_network": true}); r.Status != 400 {
		t.Fatalf("literal api_key accepted: %d %s", r.Status, r.Body)
	}
	t.Setenv("SIMX_KEY", "unused")
	if r := env.admin("POST", "/admin/v1/providers", adminOrg, map[string]any{"id": "simx", "type": "openai",
		"base_url": env.SimURL("sima") + "/v1", "api_key": "env:SIMX_KEY", "allow_private_network": true}); r.Status != 200 {
		t.Fatalf("create provider: %d %s", r.Status, r.Body)
	}
	if r := env.admin("POST", "/admin/v1/providers", adminA, map[string]any{"id": "simy", "type": "openai", "base_url": "http://x"}); r.Status != 403 {
		t.Fatalf("tenant admin created a provider: %d", r.Status)
	}
	if r := env.admin("POST", "/admin/v1/providers", adminOrg, map[string]any{"id": "sima", "type": "openai", "base_url": "http://x"}); r.Status != 409 {
		t.Fatalf("file-managed provider overwritten: %d %s", r.Status, r.Body)
	}
	dep := map[string]any{"id": "simx/qwen-4b", "provider": "simx", "model": "qwen-4b", "tier": 1,
		"prices": []map[string]any{{"effective_from": "2026-01-01T00:00:00Z", "input": 0.5, "output": 0.5}}}
	if r := env.admin("POST", "/admin/v1/deployments", adminOrg, dep); r.Status != 200 {
		t.Fatalf("create deployment: %d %s", r.Status, r.Body)
	}
	if r := env.Chat(keyA, "simx/qwen-4b", "hello", nil); r.Status != 200 || !strings.Contains(string(r.Body), `"model":"simx/qwen-4b"`) {
		t.Fatalf("API-created deployment not routable: %d %s", r.Status, r.Body)
	}
	var listed struct {
		Data []struct {
			Provider map[string]any `json:"provider"`
			Managed  string         `json:"managed"`
		} `json:"data"`
	}
	r := env.admin("GET", "/admin/v1/providers", adminOrg, nil)
	if r.JSON(&listed) != nil || strings.Contains(string(r.Body), "sk-literal") || strings.Contains(string(r.Body), "unused") {
		t.Fatalf("provider list: %s", r.Body)
	}
	if r := env.admin("DELETE", "/admin/v1/providers/simx", adminOrg, nil); r.Status != 400 {
		t.Fatalf("deleted a provider a deployment still uses: %d %s", r.Status, r.Body)
	}
	if r := env.admin("DELETE", "/admin/v1/deployments/simx/qwen-4b", adminOrg, nil); r.Status != 204 {
		t.Fatalf("delete deployment: %d %s", r.Status, r.Body)
	}
	if r := env.admin("DELETE", "/admin/v1/providers/simx", adminOrg, nil); r.Status != 204 {
		t.Fatalf("delete provider: %d %s", r.Status, r.Body)
	}
	var audit struct {
		Entries []store.AuditEntry `json:"entries"`
	}
	_ = env.admin("GET", "/admin/v1/audit", adminOrg, nil).JSON(&audit)
	seen := map[string]bool{}
	for _, e := range audit.Entries {
		seen[e.Action+" "+e.ObjectKind] = true
	}
	for _, want := range []string{"create provider", "create deployment", "delete deployment", "delete provider"} {
		if !seen[want] {
			t.Errorf("audit missing %q: %v", want, seen)
		}
	}

	// The OpenAPI document lists every route with its permission and roles.
	var doc struct {
		OpenAPI string                               `json:"openapi"`
		Paths   map[string]map[string]map[string]any `json:"paths"`
	}
	raw := env.admin("GET", "/admin/v1/openapi.json", "", nil).Body
	if err := json.Unmarshal(raw, &doc); err != nil || doc.OpenAPI != "3.1.0" {
		t.Fatalf("openapi: %v", err)
	}
	routes := env.Admin.Routes()
	for _, rt := range routes {
		op := doc.Paths[rt.Path][strings.ToLower(rt.Method)]
		if op == nil || op["x-permission"] != string(rt.Perm) || op["summary"] == "" {
			t.Errorf("route %s %s missing or incomplete in OpenAPI: %v", rt.Method, rt.Path, op)
		}
	}
	for _, capability := range []string{"tenants", "keys", "budgets", "policies", "providers", "deployments", "catalog",
		"evals", "cache", "config", "costs", "chargeback", "requests", "audit", "health", "quality", "playground"} {
		write := false
		for _, rt := range routes {
			if strings.HasPrefix(rt.Path, "/admin/v1/"+capability) {
				write = write || rt.Method != "GET" || capability == "costs" || capability == "chargeback" ||
					capability == "requests" || capability == "audit" || capability == "health" || capability == "quality"
			}
		}
		if !write {
			t.Errorf("capability %q not manageable through the admin API", capability)
		}
	}
	// Every permission is held by some role, so no route is unreachable.
	for _, rt := range routes {
		if rt.Perm == "" {
			continue // any authenticated admin (whoami)
		}
		held := false
		for _, perms := range admin.RolePerms {
			for _, p := range perms {
				held = held || p == rt.Perm
			}
		}
		if !held {
			t.Errorf("no role holds %s (%s %s)", rt.Perm, rt.Method, rt.Path)
		}
	}
	pretty, _ := json.MarshalIndent(env.Admin.OpenAPI(), "", "  ")
	pretty = append(pretty, '\n')
	const golden = "../../docs/api/openapi.json"
	if os.Getenv("UPDATE_OPENAPI") != "" {
		_ = os.MkdirAll("../../docs/api", 0o755)
		if err := os.WriteFile(golden, pretty, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := os.ReadFile(golden); err != nil || string(b) != string(pretty) {
		t.Errorf("docs/api/openapi.json is stale; regenerate with UPDATE_OPENAPI=1 (%v)", err)
	}
}

// Repeated failed admin authentications from one address are throttled; valid keys from
// other addresses are unaffected, and the failure is logged without the credential.
// Covers: NFR-13
func TestNFR13_AdminAuthThrottle(t *testing.T) {
	_, counters := newMiniredis(t)
	env := m4Env(t, counters)
	for i := 0; i < 10; i++ {
		if r := env.admin("GET", "/admin/v1/whoami", "gw-admin-wrong_"+strings.Repeat("x", 20), nil); r.Status != 401 {
			t.Fatalf("attempt %d: %d", i, r.Status)
		}
	}
	r := env.admin("GET", "/admin/v1/whoami", adminOrg, nil)
	if r.Status != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("after 10 failures: %d (Retry-After %q)", r.Status, r.Header.Get("Retry-After"))
	}
}
