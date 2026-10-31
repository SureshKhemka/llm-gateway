package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/sim"
)

const appCfg = `
server:
  reload_every: 300ms
providers:
  - id: sima
    base_url: %SIM%
deployments:
  - id: sima/qwen-4b
    provider: sima
    model: qwen-4b
    tier: 1
    prices: [{effective_from: 2026-01-01T00:00:00Z, input: 1.0, output: 1.0}]
%EXTRA%
tenants:
  - id: acme
    projects: [{id: web, keys: [{id: k1, secret: env:APP_KEY}]}]
admins:
  - {id: org, role: org_admin, secret: env:APP_ADMIN}
`

const newPrice = `    prices:
      - {effective_from: 2026-01-01T00:00:00Z, input: 1.0, output: 1.0}
      - {effective_from: 2026-02-01T00:00:00Z, input: 50.0, output: 50.0}`

func writeCfg(t *testing.T, path, simURL, extra string, priced bool) {
	body := strings.ReplaceAll(appCfg, "%SIM%", simURL)
	body = strings.ReplaceAll(body, "%EXTRA%", extra)
	if priced {
		body = strings.Replace(body, "    prices: [{effective_from: 2026-01-01T00:00:00Z, input: 1.0, output: 1.0}]", newPrice, 1)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil { // atomic, like a ConfigMap update
		t.Fatal(err)
	}
}

func do(t *testing.T, method, url, key, body string) (*http.Response, string) {
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// Scenario 14: a price card change and a new deployment are applied from the file
// while a stream is running; both take effect within 10 s and the stream is not dropped.
// Covers: CFG-01, CFG-02, CFG-03, PRV-07, BUD-06, CFG-05
func TestScenario14_HotReload(t *testing.T) {
	in := sim.New("sima", sim.Model{ID: "qwen-4b", Tier: 1}, sim.Model{ID: "qwen-9b", Tier: 2})
	simSrv := httptest.NewServer(in.Handler())
	defer simSrv.Close()
	t.Setenv("APP_KEY", "gw-app-k1_aaaaaaaaaaaaaaaaaaaa")
	t.Setenv("APP_ADMIN", "gw-app-admin_bbbbbbbbbbbbbbbb")
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	writeCfg(t, path, simSrv.URL+"/v1", "", false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := New(ctx, Options{ConfigPath: path, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	a.RunBackground(ctx)
	gw := httptest.NewServer(a.Server.Handler())
	defer gw.Close()
	key, adm := "gw-app-k1_aaaaaaaaaaaaaaaaaaaa", "gw-app-admin_bbbbbbbbbbbbbbbb"
	chat := func(model, extra string) (*http.Response, string) {
		return do(t, "POST", gw.URL+"/v1/chat/completions", key, `{"model":"`+model+`","messages":[{"role":"user","content":"price check please"}]`+extra+`}`)
	}
	costPerToken := func(resp *http.Response) float64 {
		c, _ := strconv.ParseFloat(resp.Header.Get("X-Gw-Cost-Usd"), 64)
		in, _ := strconv.Atoi(resp.Header.Get("X-Gw-Input-Tokens"))
		out, _ := strconv.Atoi(resp.Header.Get("X-Gw-Output-Tokens"))
		return c / float64(in+out) * 1e6
	}
	resp, _ := chat("sima/qwen-4b", "")
	if p := costPerToken(resp); p < 0.99 || p > 1.01 {
		t.Fatalf("initial price %v", p)
	}
	if r, _ := chat("sima/qwen-9b", ""); r.StatusCode != 404 {
		t.Fatalf("qwen-9b exists before the change: %d", r.StatusCode)
	}

	// A long stream in flight across the reload.
	in.SetFaults("qwen-4b", sim.Faults{TokensPerSec: 8})
	streamDone := make(chan string, 1)
	go func() {
		_, body := do(t, "POST", gw.URL+"/v1/chat/completions", key,
			`{"model":"sima/qwen-4b","stream":true,"messages":[{"role":"user","content":"`+strings.Repeat("streaming ", 20)+`"}]}`)
		streamDone <- body
	}()
	time.Sleep(300 * time.Millisecond)

	// Preview, then apply by editing the file.
	extra := `  - id: sima/qwen-9b
    provider: sima
    model: qwen-9b
    tier: 2
    prices: [{effective_from: 2026-01-01T00:00:00Z, input: 2.0, output: 2.0}]`
	writeCfg(t, path, simSrv.URL+"/v1", extra, true)
	changed := time.Now()
	in.ClearFaults()
	var applied time.Duration
	for time.Since(changed) < 10*time.Second {
		r, _ := chat("sima/qwen-9b", "")
		r2, _ := chat("sima/qwen-4b", "")
		if r.StatusCode == 200 && costPerToken(r2) > 49 {
			applied = time.Since(changed)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if applied == 0 {
		t.Fatal("changes not applied within 10 s")
	}
	t.Logf("price change and new deployment applied %v after the file changed", applied.Round(time.Millisecond))
	select {
	case body := <-streamDone:
		if !strings.Contains(body, "[DONE]") || strings.Contains(body, `"error"`) {
			t.Fatalf("in-flight stream dropped: %s", body[max(0, len(body)-300):])
		}
	case <-time.After(15 * time.Second):
		t.Fatal("stream never finished")
	}

	// An invalid edit is rejected with file/path errors and the running state is kept.
	if err := os.WriteFile(path, []byte("providers:\n  - id: sima\n    type: bogus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, body := do(t, "POST", gw.URL+"/admin/v1/config/diff", adm, "")
	if r.StatusCode != 422 || !strings.Contains(body, "providers[0].type") {
		t.Fatalf("diff of an invalid file: %d %s", r.StatusCode, body)
	}
	time.Sleep(700 * time.Millisecond)
	if r, _ := chat("sima/qwen-9b", ""); r.StatusCode != 200 {
		t.Fatalf("invalid file disturbed the running config: %d", r.StatusCode)
	}
	// Diff shows a pending valid change without applying it.
	writeCfg(t, path, simSrv.URL+"/v1", strings.Replace(extra, "tier: 2", "tier: 3", 1), true)
	a.mu.Lock()
	for f := range a.fileStat {
		a.fileStat[f] = time.Now().Add(time.Hour) // keep the poller from applying it during the check
	}
	a.mu.Unlock()
	r, body = do(t, "POST", gw.URL+"/admin/v1/config/diff", adm, "")
	var diff struct {
		Changes map[string]any `json:"changes"`
	}
	_ = json.Unmarshal([]byte(body), &diff)
	if r.StatusCode != 200 || diff.Changes["deployments.sima/qwen-9b.tier"] == nil {
		t.Fatalf("diff: %d %s", r.StatusCode, body)
	}
}
