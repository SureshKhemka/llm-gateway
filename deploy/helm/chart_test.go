// Package helm holds the chart test; the chart itself is deploy/helm/llm-gateway.
package helm

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/SureshKhemka/llm-gateway/internal/config"
)

const chart = "llm-gateway"

// render runs `helm template` when helm is installed (HELM_BIN or PATH); otherwise it
// renders with text/template and the handful of Sprig functions the chart uses, so the
// default suite needs no extra tools.
func render(t *testing.T, set map[string]any) (string, string) {
	helm := os.Getenv("HELM_BIN")
	if helm == "" {
		helm, _ = exec.LookPath("helm")
	}
	if helm != "" {
		args := []string{"template", "gw", chart}
		for k, v := range set {
			args = append(args, "--set", fmt.Sprintf("%s=%v", k, v))
		}
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, out)
		}
		return string(out), "helm"
	}
	var values map[string]any
	b, _ := os.ReadFile(filepath.Join(chart, "values.yaml"))
	if err := yaml.Unmarshal(b, &values); err != nil {
		t.Fatal(err)
	}
	for k, v := range set {
		m := values
		parts := strings.Split(k, ".")
		for _, p := range parts[:len(parts)-1] {
			m = m[p].(map[string]any)
		}
		m[parts[len(parts)-1]] = v
	}
	var meta struct {
		Name       string `yaml:"name"`
		AppVersion string `yaml:"appVersion"`
	}
	b, _ = os.ReadFile(filepath.Join(chart, "Chart.yaml"))
	_ = yaml.Unmarshal(b, &meta)
	data := map[string]any{"Values": values, "Release": map[string]any{"Name": "gw"},
		"Chart": map[string]any{"Name": meta.Name, "AppVersion": meta.AppVersion},
		"Files": files{}}
	root := template.New("chart")
	funcs := template.FuncMap{
		"include": func(name string, d any) (string, error) {
			var buf bytes.Buffer
			err := root.ExecuteTemplate(&buf, name, d)
			return buf.String(), err
		},
		"nindent": func(n int, s string) string {
			pad := strings.Repeat(" ", n)
			return "\n" + pad + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n"+pad)
		},
		"toYaml": func(v any) string {
			out, _ := yaml.Marshal(v)
			return strings.TrimRight(string(out), "\n")
		},
		"quote": func(s any) string { return fmt.Sprintf("%q", fmt.Sprint(s)) },
		"default": func(d, v any) any {
			if v == nil || v == "" || v == 0 || v == false {
				return d
			}
			return v
		},
		"trunc":      func(n int, s string) string { return s[:min(n, len(s))] },
		"trimSuffix": func(suf, s string) string { return strings.TrimSuffix(s, suf) },
	}
	root.Funcs(funcs)
	paths, _ := filepath.Glob(filepath.Join(chart, "templates", "*"))
	for _, p := range paths {
		b, _ := os.ReadFile(p)
		if _, err := root.New(filepath.Base(p)).Parse(string(b)); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	var out strings.Builder
	for _, p := range paths {
		if strings.HasPrefix(filepath.Base(p), "_") {
			continue
		}
		var buf bytes.Buffer
		if err := root.ExecuteTemplate(&buf, filepath.Base(p), data); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		out.WriteString("---\n" + buf.String() + "\n")
	}
	return out.String(), "built-in renderer"
}

type files struct{}

func (files) Get(name string) string {
	b, _ := os.ReadFile(filepath.Join(chart, name))
	return string(b)
}

// The chart renders to valid manifests; the gateway runs non-root with a read-only root
// filesystem, readiness gates traffic, the drain window covers in-flight streams, the
// embedded config validates, and the shipped alert rules match deploy/observability.
// Covers: NFR-17, OBS-07
func TestNFR17_HelmChartRenders(t *testing.T) {
	out, how := render(t, map[string]any{"ingress.enabled": true, "autoscaling.enabled": true,
		"serviceMonitor.enabled": true, "prometheusRule.enabled": true, "existingSecret": "gw-secrets"})
	t.Logf("rendered with %s", how)
	kinds := map[string]map[string]any{}
	dec := yaml.NewDecoder(strings.NewReader(out))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("rendered YAML: %v\n%s", err, out)
		}
		if doc == nil {
			continue
		}
		kinds[doc["kind"].(string)] = doc
	}
	for _, k := range []string{"Deployment", "Service", "ConfigMap", "PodDisruptionBudget", "HorizontalPodAutoscaler", "Ingress", "ServiceMonitor", "PrometheusRule"} {
		if kinds[k] == nil {
			t.Errorf("missing %s (have %v)", k, keys(kinds))
		}
	}
	dep := kinds["Deployment"]
	spec := dig(dep, "spec", "template", "spec").(map[string]any)
	c := spec["containers"].([]any)[0].(map[string]any)
	if dig(c, "readinessProbe", "httpGet", "path") != "/readyz" || dig(c, "livenessProbe", "httpGet", "path") != "/livez" {
		t.Errorf("probes: %v %v", c["readinessProbe"], c["livenessProbe"])
	}
	if dig(spec, "securityContext", "runAsNonRoot") != true || dig(c, "securityContext", "readOnlyRootFilesystem") != true {
		t.Errorf("security context: %v %v", spec["securityContext"], c["securityContext"])
	}
	if g, _ := spec["terminationGracePeriodSeconds"].(int); g < 30 {
		t.Errorf("termination grace %v too short to drain streams", spec["terminationGracePeriodSeconds"])
	}
	if dig(c, "envFrom") == nil {
		t.Error("existingSecret not wired as envFrom")
	}
	if dig(dep, "spec", "replicas") != nil {
		t.Error("replicas set while the HPA owns them")
	}
	// The embedded gateway config loads and validates.
	cfgYAML := dig(kinds["ConfigMap"], "data", "gateway.yaml").(string)
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	os.WriteFile(path, []byte(cfgYAML), 0o644)
	if _, err := config.Load(path); err != nil {
		t.Errorf("chart config does not validate: %v", err)
	}
	a, _ := os.ReadFile(filepath.Join(chart, "files", "alerts.yml"))
	b, _ := os.ReadFile("../observability/alerts.yml")
	if !bytes.Equal(a, b) {
		t.Error("deploy/helm/llm-gateway/files/alerts.yml differs from deploy/observability/alerts.yml")
	}
	// The container image the chart deploys: non-root, gateway entrypoint.
	df, _ := os.ReadFile("../../Dockerfile")
	if !strings.Contains(string(df), "USER nonroot") || !strings.Contains(string(df), `ENTRYPOINT ["/usr/local/bin/gateway"]`) {
		t.Error("Dockerfile must run the gateway as non-root")
	}
}

func dig(m any, path ...string) any {
	for _, p := range path {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil
		}
		m = mm[p]
	}
	return m
}

func keys(m map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
