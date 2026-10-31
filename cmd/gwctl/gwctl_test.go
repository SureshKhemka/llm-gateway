package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SureshKhemka/llm-gateway/internal/testenv"
)

const cfg = `
providers:
  - {id: sima, type: openai, base_url: "${SIM_SIMA_URL}"}
` + testenv.StandardDeployments + `
policies:
  - {name: default, objective: min_cost}
tenants:
  - id: acme
    projects: [{id: web, keys: [{id: k1, secret: env:CLI_KEY}]}]
admins:
  - {id: org, role: org_admin, secret: env:CLI_ADMIN}
  - {id: fo, role: finops_viewer, secret: env:CLI_FINOPS}
`

// The CLI drives keys, budgets, policies, config and raw calls through the admin API
// with the caller's role.
// Covers: ADM-04, ADM-05
func TestADM04_CLI(t *testing.T) {
	env := testenv.New(t, testenv.Options{YAML: cfg, Env: map[string]string{
		"CLI_KEY": "gw-cli-k1_aaaaaaaaaaaaaaaaaaaaaaaa", "CLI_ADMIN": "gw-cli-admin_bbbbbbbbbbbbbbbbbbbb", "CLI_FINOPS": "gw-cli-fo_cccccccccccccccccccc"}})
	t.Setenv("GWCTL_URL", env.HTTP.URL)
	t.Setenv("GWCTL_KEY", "gw-cli-admin_bbbbbbbbbbbbbbbbbbbb")
	gw := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := run(args, &out)
		return out.String(), err
	}
	out, err := gw("keys", "create", "--tenant", "acme", "--project", "web", "--name", "ci")
	var created struct {
		Key    string `json:"key"`
		Record struct {
			ID string `json:"id"`
		} `json:"record"`
	}
	if err != nil || json.Unmarshal([]byte(out), &created) != nil || created.Key == "" {
		t.Fatalf("keys create: %v %s", err, out)
	}
	if r := env.Chat(created.Key, "auto", "hello", nil); r.Status != 200 {
		t.Fatalf("CLI-created key rejected: %d", r.Status)
	}
	if out, err = gw("keys", "revoke", created.Record.ID, "--reason", "test"); err != nil {
		t.Fatalf("revoke: %v %s", err, out)
	}
	if r := env.Chat(created.Key, "auto", "hello", nil); r.Status != 401 {
		t.Fatalf("revoked key still works: %d", r.Status)
	}

	dir := t.TempDir()
	budget := filepath.Join(dir, "budget.yaml")
	os.WriteFile(budget, []byte("id: acme-monthly\nscope: tenant\nscope_id: acme\nperiod: monthly\nhard_usd: 10\n"), 0o644)
	if out, err = gw("budgets", "apply", "-f", budget); err != nil {
		t.Fatalf("budgets apply: %v %s", err, out)
	}
	if out, _ = gw("budgets", "list"); !strings.Contains(out, "acme-monthly") {
		t.Fatalf("budget not listed: %s", out)
	}
	pol := filepath.Join(dir, "policy.yaml")
	os.WriteFile(pol, []byte("name: default\nobjective: max_quality\n"), 0o644)
	if out, err = gw("policies", "apply", "-f", pol); err != nil {
		t.Fatalf("policies apply: %v %s", err, out)
	}
	// Activation without an eval run is refused (EVL-08 gate) unless overridden.
	if _, err = gw("policies", "activate", "default", "--version", "2"); err == nil {
		t.Fatal("activation without an eval passed")
	}
	if out, err = gw("policies", "activate", "default", "--version", "2", "--override-reason", "cli test"); err != nil {
		t.Fatalf("activate with override: %v %s", err, out)
	}
	if out, err = gw("config", "show"); err != nil || !strings.Contains(out, "env:CLI_ADMIN") || strings.Contains(out, "gw-cli-admin_") {
		t.Fatalf("config show: %v %s", err, out)
	}
	if out, err = gw("api", "GET", "/admin/v1/health"); err != nil || !strings.Contains(out, "sima/") {
		t.Fatalf("raw api: %v %s", err, out)
	}
	// The same permission table as API and console: a FinOps viewer can't write.
	t.Setenv("GWCTL_KEY", "gw-cli-fo_cccccccccccccccccccc")
	if _, err = gw("budgets", "apply", "-f", budget); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("finops viewer wrote a budget: %v", err)
	}
	if _, err = gw("costs", "--group-by", "tenant"); err != nil {
		t.Fatalf("finops viewer can't read costs: %v", err)
	}
	if _, err = gw("nonsense"); err == nil {
		t.Fatal("unknown command accepted")
	}
}
