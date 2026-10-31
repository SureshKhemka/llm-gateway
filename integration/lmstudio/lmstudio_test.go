//go:build lmstudio

// LM Studio integration suite. Run on the Mac with LM Studio serving:
//
//	LMSTUDIO_BASE_URL=http://localhost:1234/v1 go test -tags lmstudio ./...
//
// It discovers models via /v1/models and skips, naming the missing model role, any
// scenario whose tier or capability is not loaded.
package lmstudio

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/gateway"
	"github.com/SureshKhemka/llm-gateway/internal/server"
)

func baseURL() string {
	if u := os.Getenv("LMSTUDIO_BASE_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://localhost:1234/v1"
}

// Role classifies a loaded model by name.
type Role string

const (
	RoleTier0     Role = "tier0"
	RoleTier1     Role = "tier1"
	RoleTier2     Role = "tier2"
	RoleTier3     Role = "tier3"
	RoleCode      Role = "code"
	RoleVision    Role = "vision"
	RoleEmbedding Role = "embeddings"
)

// classify maps model IDs to roles using parameter-count hints in the name.
func classify(id string) []Role {
	l := strings.ToLower(id)
	var roles []Role
	switch {
	case strings.Contains(l, "embed"):
		return []Role{RoleEmbedding}
	case strings.Contains(l, "vl") || strings.Contains(l, "vision"):
		roles = append(roles, RoleVision)
	}
	if strings.Contains(l, "devstral") || strings.Contains(l, "coder") {
		roles = append(roles, RoleCode)
	}
	size := func(s ...string) bool {
		for _, x := range s {
			if strings.Contains(l, x) {
				return true
			}
		}
		return false
	}
	// Largest first: "gemma-4-26b-a4b" is a 26B model with 4B active parameters.
	switch {
	case size("24b", "26b", "27b", "30b", "31b", "32b", "70b", "small-2"):
		roles = append(roles, RoleTier3)
	case size("8b", "9b", "12b", "13b", "14b"):
		roles = append(roles, RoleTier2)
	case size("4b", "e4b", "3b"):
		roles = append(roles, RoleTier1)
	case size("e2b", "1b", "-2b", "0.6b", "1.7b"):
		roles = append(roles, RoleTier0)
	}
	return roles
}

type inventory map[Role][]string

func discover(t *testing.T) inventory {
	resp, err := http.Get(baseURL() + "/models")
	if err != nil {
		t.Skipf("LM Studio not reachable at %s: %v", baseURL(), err)
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct{ ID string } `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("model list: %v", err)
	}
	inv := inventory{}
	for _, m := range list.Data {
		for _, r := range classify(m.ID) {
			inv[r] = append(inv[r], m.ID)
		}
	}
	t.Logf("LM Studio inventory: %v", inv)
	return inv
}

func (inv inventory) need(t *testing.T, roles ...Role) []string {
	var out []string
	for _, r := range roles {
		ms := inv[r]
		if len(ms) == 0 {
			t.Skipf("skipping: no %s model loaded in LM Studio (load one to run this scenario)", r)
		}
		out = append(out, ms[0])
	}
	return out
}

// gatewayFor starts an in-process gateway over LM Studio with the given deployments.
func gatewayFor(t *testing.T, deployments string) *httptest.Server {
	yaml := "providers:\n  - id: lmstudio\n    base_url: " + baseURL() + "\n    timeouts: {ttft: 120s, total: 300s}\n" + deployments
	cfg, err := config.Parse("lmstudio.yaml", []byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := gateway.BuildSnapshot(gateway.BuildInput{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	eng := gateway.NewEngine(snap, gateway.EngineOptions{})
	srv := httptest.NewServer(server.New(eng, nil).Handler())
	t.Cleanup(func() { srv.Close(); eng.Close() })
	return srv
}

func deployment(id, model string, tier int, caps string) string {
	return "  - id: " + id + "\n    provider: lmstudio\n    model: " + model + "\n    tier: " + string(rune('0'+tier)) +
		"\n    capabilities: [" + caps + "]\n    context_window: 16384\n    residency: [local]\n" +
		"    prices: [{effective_from: 2026-01-01T00:00:00Z, input: 0.5, output: 2.0}]\n"
}

func post(t *testing.T, url string, body any) (int, http.Header, []byte) {
	b, _ := json.Marshal(body)
	c := &http.Client{Timeout: 5 * time.Minute}
	resp, err := c.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, out
}

// Covers: ENV-01, PRV-01, API-01
func TestLMStudio_ChatStreamThroughGateway(t *testing.T) {
	inv := discover(t)
	m := inv.need(t, RoleTier3)[0]
	srv := gatewayFor(t, "deployments:\n"+deployment("lmstudio/big", m, 3, "chat, tools, json_schema"))
	st, h, body := post(t, srv.URL+"/v1/chat/completions", map[string]any{"model": "lmstudio/big", "max_tokens": 64,
		"messages": []map[string]any{{"role": "user", "content": "Reply with the single word: pong"}}})
	if st != 200 || h.Get("X-Gw-Cost-Usd") == "" {
		t.Fatalf("chat: %d %s", st, body)
	}
	st, _, body = post(t, srv.URL+"/v1/chat/completions", map[string]any{"model": "lmstudio/big", "stream": true, "max_tokens": 32,
		"stream_options": map[string]any{"include_usage": true},
		"messages":       []map[string]any{{"role": "user", "content": "Count to five."}}})
	if st != 200 || !strings.Contains(string(body), "[DONE]") || !strings.Contains(string(body), `"usage"`) {
		t.Fatalf("stream: %d %s", st, body)
	}
}

// Covers: ENV-01, API-03
func TestLMStudio_Embeddings(t *testing.T) {
	inv := discover(t)
	m := inv.need(t, RoleEmbedding)[0]
	srv := gatewayFor(t, "deployments:\n"+strings.Replace(deployment("lmstudio/embed", m, 0, "embeddings"), "output: 2.0", "output: 0", 1))
	st, _, body := post(t, srv.URL+"/v1/embeddings", map[string]any{"model": "lmstudio/embed", "input": "hello"})
	if st != 200 || !strings.Contains(string(body), "embedding") {
		t.Fatalf("embeddings: %d %s", st, body)
	}
}

// Covers: CFG-07, PRV-06
func TestLMStudio_MinimalConfigDiscovery(t *testing.T) {
	discover(t)
	cfg, err := config.Parse("min.yaml", []byte("providers:\n  - id: lmstudio\n    base_url: "+baseURL()+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := gateway.BuildSnapshot(gateway.BuildInput{Config: cfg})
	found := gateway.NewDiscovery().Run(t.Context(), snap)
	if len(found) == 0 {
		t.Fatal("no models discovered")
	}
}
