package server_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/SureshKhemka/llm-gateway/internal/config"
)

const repo = "../.."

func readDoc(t *testing.T, path string) string {
	b, err := os.ReadFile(filepath.Join(repo, path))
	if err != nil {
		t.Fatalf("missing %s: %v", path, err)
	}
	return string(b)
}

var metricRef = regexp.MustCompile(`\b(gw_[a-z_]+?)(_bucket|_sum|_count)?\b`)

// The documentation set exists and stays in step with the code: guides cover their
// subjects, the API reference lists every route, every shipped alert has a runbook,
// alerts and dashboards only use metrics the gateway exports, the LM Studio overlay
// validates with the demo config, and relative links resolve.
// Covers: NFR-23, OBS-07, ENV-02
func TestNFR23_DocumentationIsCompleteAndConsistent(t *testing.T) {
	required := map[string][]string{
		"docs/OPERATOR_GUIDE.md": {"## 1. Install and run", "## 2. Configuration", "## 3. Observability", "## 5. Backup and restore", "deploy/demo/up.sh", "helm install"},
		"docs/API.md":            {"## Inference endpoints", "### Errors", "## Admin API", "budget_exceeded` | 402"},
		"docs/POLICY_GUIDE.md":   {"## 1. Which policy applies", "### Objectives", "### Difficulty", "### Cascade", "### Budget degradation", "## 3. Lifecycle"},
		"docs/EVAL_GUIDE.md":     {"## 1. Suites", "### Graders", "## 3. Gating", "## 4. Tuning"},
		"docs/LMSTUDIO.md":       {"Auto-Evict", "Preload", "Max Concurrent Predictions", "context length"},
		"docs/SECURITY.md":       {"ASVS", "STRIDE", "Remaining gaps"},
	}
	for path, wants := range required {
		body := readDoc(t, path)
		for _, w := range wants {
			if !strings.Contains(body, w) {
				t.Errorf("%s: missing %q", path, w)
			}
		}
	}

	// API reference: every admin route and every public route.
	apiDoc := readDoc(t, "docs/API.md")
	env := m5Env(t)
	for _, rt := range env.Admin.Routes() {
		if !strings.Contains(apiDoc, "`"+rt.Method+"` | `"+rt.Path+"`") {
			t.Errorf("docs/API.md: admin route %s %s not documented", rt.Method, rt.Path)
		}
	}
	for _, p := range []string{"/v1/chat/completions", "/v1/messages", "/v1/messages/count_tokens", "/v1/embeddings", "/v1/models",
		"/v1/route/explain", "/v1/route/decisions/{id}", "/v1/feedback", "/livez", "/readyz", "/metrics"} {
		if !strings.Contains(apiDoc, "`"+p+"`") {
			t.Errorf("docs/API.md: %s not documented", p)
		}
	}

	// Alerts ↔ runbooks.
	var rules struct {
		Groups []struct {
			Rules []struct {
				Alert       string            `yaml:"alert"`
				Expr        string            `yaml:"expr"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal([]byte(readDoc(t, "deploy/observability/alerts.yml")), &rules); err != nil {
		t.Fatal(err)
	}
	alerts := map[string]bool{}
	var exprs []string
	for _, g := range rules.Groups {
		for _, r := range g.Rules {
			alerts[r.Alert] = true
			exprs = append(exprs, r.Expr)
			if r.Annotations["runbook_url"] != "docs/runbooks/"+r.Alert+".md" || r.Annotations["summary"] == "" {
				t.Errorf("alert %s: runbook_url %q, summary %q", r.Alert, r.Annotations["runbook_url"], r.Annotations["summary"])
				continue
			}
			body := readDoc(t, r.Annotations["runbook_url"])
			for _, h := range []string{"## Meaning", "## Impact", "## Diagnosis", "## Mitigation", "## Escalation"} {
				if !strings.Contains(body, h) {
					t.Errorf("%s: missing %s", r.Annotations["runbook_url"], h)
				}
			}
		}
	}
	if len(alerts) < 8 {
		t.Errorf("only %d alerts", len(alerts))
	}
	files, _ := filepath.Glob(filepath.Join(repo, "docs/runbooks/*.md"))
	for _, f := range files {
		if n := strings.TrimSuffix(filepath.Base(f), ".md"); n != "README" && !alerts[n] {
			t.Errorf("runbook %s has no alert", n)
		}
	}

	// Dashboards and alerts use only exported metrics.
	var dash struct {
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal([]byte(readDoc(t, "deploy/observability/grafana/dashboards/llm-gateway.json")), &dash); err != nil {
		t.Fatal(err)
	}
	for _, p := range dash.Panels {
		for _, tg := range p.Targets {
			exprs = append(exprs, tg.Expr)
		}
	}
	// Exported = scraped after a request, plus vectors and gauges that only appear once
	// they have a sample (declared by name in the metrics code).
	env.Chat(key1, "auto", "hello metrics", nil)
	exported := scrape(t, env.HTTP.URL)
	for _, f := range []string{"internal/observe/metrics.go", "internal/gateway/metrics.go"} {
		for _, m := range regexp.MustCompile(`Name: *"(gw_[a-z_]+)"`).FindAllStringSubmatch(readDoc(t, f), -1) {
			exported += "\n# TYPE " + m[1] + " declared"
		}
	}
	for _, e := range exprs {
		for _, m := range metricRef.FindAllStringSubmatch(e, -1) {
			name := m[1] + m[2]
			if !strings.Contains(exported, "# TYPE "+m[1]+" ") {
				t.Errorf("%q uses %s, which the gateway does not export", e, name)
			}
		}
	}
	if len(dash.Panels) < 10 {
		t.Errorf("dashboard has %d panels", len(dash.Panels))
	}

	// The LM Studio overlay validates on top of the demo configuration.
	dir := t.TempDir()
	for _, f := range []string{"deploy/demo/gateway.yaml", "deploy/demo/overlays/lmstudio.yaml"} {
		os.WriteFile(filepath.Join(dir, filepath.Base(f)), []byte(readDoc(t, f)), 0o644)
	}
	for k, v := range map[string]string{"DEMO_KEY_A": "gw-docs-a_aaaaaaaaaaaaaaaa", "DEMO_KEY_B": "gw-docs-b_bbbbbbbbbbbbbbbb", "DEMO_ADMIN_KEY": "gw-docs-admin"} {
		t.Setenv(k, v)
	}
	if _, err := config.Load(dir); err != nil {
		t.Errorf("demo + LM Studio overlay: %v", err)
	}

	// Relative links in docs resolve.
	link := regexp.MustCompile(`\]\(([^)#\s]+)(#[^)]*)?\)`)
	mds, _ := filepath.Glob(filepath.Join(repo, "docs", "*.md"))
	more, _ := filepath.Glob(filepath.Join(repo, "docs", "runbooks", "*.md"))
	for _, md := range append(mds, more...) {
		b, _ := os.ReadFile(md)
		for _, m := range link.FindAllStringSubmatch(string(b), -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(md), target)); err != nil {
				t.Errorf("%s: broken link %s", md, target)
			}
		}
	}
}
