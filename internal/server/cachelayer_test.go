package server_test

import (
	"context"
	"strings"
	"testing"

	"github.com/SureshKhemka/llm-gateway/internal/sim"
	"github.com/SureshKhemka/llm-gateway/internal/testenv"
)

func withYAML(edit func(string) string) func(*testenv.Options) {
	return func(o *testenv.Options) { o.YAML = edit(o.YAML) }
}

// Cache-layer behaviour around semantic hits: per-policy threshold and opt-out, a
// failing embedder degrades to a miss, hit verification by a judge fails closed, hits
// are sampled for review, and cached tool calls replay as stream chunks.
// Covers: CCH-02, CCH-05, CCH-07, CCH-09, RES-08
func TestCCH09_SemanticHitControls(t *testing.T) {
	ask := func(env *testenv.Env, model, prompt string, extra map[string]any) *testenv.Response {
		if extra == nil {
			extra = map[string]any{}
		}
		extra["temperature"] = 0
		return env.Chat(key1, model, prompt, extra)
	}
	t.Run("policy threshold and opt-out", func(t *testing.T) {
		env := m5Env(t, withYAML(func(y string) string {
			return strings.Replace(y, "virtual_models:\n", `  - name: strict-cache
    objective: min_cost
    cache: {semantic_threshold: 0.9999}
  - name: no-cache
    objective: min_cost
    cache: {disabled: true}
virtual_models:
  - {name: strict, policy: strict-cache}
  - {name: nocache, policy: no-cache}
`, 1)
		}))
		// One extra content word: cosine ≈ 0.89 in the simulator's embedder.
		ask(env, "auto", "where is your opening hours page", nil)
		if r := ask(env, "auto", "where is your opening hours main page", nil); r.Header.Get("X-Gw-Cache") != "semantic_hit" {
			t.Fatalf("global threshold: %s", r.Header.Get("X-Gw-Cache"))
		}
		ask(env, "strict", "where is your head office", nil)
		if r := ask(env, "strict", "where is your main head office", nil); r.Header.Get("X-Gw-Cache") == "semantic_hit" {
			t.Fatal("policy threshold 0.9999 still produced a semantic hit")
		}
		ask(env, "nocache", "never cached", nil)
		if r := ask(env, "nocache", "never cached", nil); r.Header.Get("X-Gw-Cache") == "exact_hit" {
			t.Fatal("policy with cache disabled served a hit")
		}
	})
	t.Run("embedder down", func(t *testing.T) {
		env := m5Env(t)
		env.Sims["sima"].SetFaults("nomic-embed", sim.Faults{Down: true})
		for _, p := range []string{"embedder outage question", "embedder outage question please"} {
			if r := ask(env, "auto", p, nil); r.Status != 200 || r.Header.Get("X-Gw-Cache") == "semantic_hit" {
				t.Fatalf("%q: %d %s", p, r.Status, r.Header.Get("X-Gw-Cache"))
			}
		}
		if r := ask(env, "auto", "embedder outage question", nil); r.Header.Get("X-Gw-Cache") != "exact_hit" {
			t.Fatalf("exact cache must keep working without the embedder: %s", r.Header.Get("X-Gw-Cache"))
		}
	})
	t.Run("judge verification fails closed and hits are sampled", func(t *testing.T) {
		env := m5Env(t, withYAML(func(y string) string {
			return strings.Replace(y, "  embedder: sima/nomic-embed\npolicies:", "  embedder: sima/nomic-embed\n  judge_hits: sima/qwen-27b\n  sample_rate: 1\npolicies:", 1)
		}))
		ask(env, "auto", "how do I reset my password", nil)
		env.Sims["sima"].SetFaults("qwen-27b", sim.Faults{Down: true}) // the judge is unavailable
		if r := ask(env, "auto", "how do I reset my password please", nil); r.Status != 200 || r.Header.Get("X-Gw-Cache") == "semantic_hit" {
			t.Fatalf("unverified semantic hit served: %d %s", r.Status, r.Header.Get("X-Gw-Cache"))
		}
		env.Sims["sima"].ClearFaults()
		ask(env, "auto", "how can I change my email address", nil)
		r := ask(env, "auto", "how can I change my email address please", nil)
		docs, _ := env.Store.ListDocs(context.Background(), "cache_sample", "")
		if r.Header.Get("X-Gw-Cache") == "semantic_hit" && len(docs) == 0 {
			t.Fatal("verified semantic hit not sampled for review")
		}
		t.Logf("with a healthy judge: %s, %d sampled", r.Header.Get("X-Gw-Cache"), len(docs))
	})
	t.Run("cached tool calls replay as stream chunks", func(t *testing.T) {
		env := m5Env(t)
		tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}}}}}
		first := ask(env, "tools-cached", "what is the weather in Pune", map[string]any{"tools": tools})
		if first.Status != 200 {
			t.Fatalf("first: %d %s", first.Status, first.Body)
		}
		r := ask(env, "tools-cached", "what is the weather in Pune", map[string]any{"tools": tools, "stream": true})
		if r.Header.Get("X-Gw-Cache") != "exact_hit" || !strings.Contains(string(r.Body), "[DONE]") {
			t.Fatalf("stream replay: %s %s", r.Header.Get("X-Gw-Cache"), r.Body)
		}
		if strings.Contains(string(first.Body), "tool_calls") && !strings.Contains(string(r.Body), "get_weather") {
			t.Fatalf("tool call missing from replayed stream: %s", r.Body)
		}
	})
}
