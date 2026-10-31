package sim

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

const chatBody = `{"model":"m1","messages":[{"role":"user","content":"tell me about rivers and lakes"}]}`

func newSim(t *testing.T) (*Instance, *httptest.Server) {
	in := New("a", Model{ID: "m1", Tier: 1}, Model{ID: "emb", Embeddings: true})
	srv := httptest.NewServer(in.Handler())
	t.Cleanup(srv.Close)
	return in, srv
}

// Covers: ENV-04
func TestENV04_NamedInstancesAndFaultsOnCommand(t *testing.T) {
	in, srv := newSim(t)
	b := New("b", Model{ID: "m1", Tier: 1})
	srvB := httptest.NewServer(b.Handler())
	defer srvB.Close()
	if r := post(t, srvB.URL+"/v1/chat/completions", chatBody); r.StatusCode != 200 {
		t.Fatalf("second instance: %d", r.StatusCode)
	}

	set := func(f string) {
		r := post(t, srv.URL+"/_sim/faults", `{"faults":`+f+`}`)
		if r.StatusCode != 204 {
			t.Fatalf("set faults %d", r.StatusCode)
		}
	}
	// 429 with Retry-After
	set(`{"rate_limit":true,"retry_after_sec":7}`)
	r := post(t, srv.URL+"/v1/chat/completions", chatBody)
	if r.StatusCode != 429 || r.Header.Get("Retry-After") != "7" {
		t.Fatalf("rate limit fault: %d %q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	// error rate 1.0 with status
	set(`{"error_rate":1,"error_status":503}`)
	if r := post(t, srv.URL+"/v1/chat/completions", chatBody); r.StatusCode != 503 {
		t.Fatalf("error fault: %d", r.StatusCode)
	}
	// added latency
	set(`{"latency_ms":200}`)
	start := time.Now()
	post(t, srv.URL+"/v1/chat/completions", chatBody)
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("latency fault not applied")
	}
	// malformed JSON
	set(`{"malformed_json":true}`)
	r = post(t, srv.URL+"/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_object"}}`)
	var resp struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
		Usage *json.RawMessage `json:"usage"`
	}
	_ = json.NewDecoder(r.Body).Decode(&resp)
	var v any
	if json.Unmarshal([]byte(resp.Choices[0].Message.Content), &v) == nil {
		t.Fatal("malformed_json produced valid JSON")
	}
	// missing usage
	set(`{"omit_usage":true}`)
	resp.Usage = nil
	r = post(t, srv.URL+"/v1/chat/completions", chatBody)
	_ = json.NewDecoder(r.Body).Decode(&resp)
	if resp.Usage != nil {
		t.Fatal("omit_usage still returned usage")
	}
	// timeout: request hangs until the client gives up
	set(`{"timeout":true}`)
	c := &http.Client{Timeout: 150 * time.Millisecond}
	if _, err := c.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatBody)); err == nil {
		t.Fatal("timeout fault answered")
	}
	// per-model fault leaves other models alone
	in.ClearFaults()
	in.SetFaults("m1", Faults{Down: true})
	if r := post(t, srv.URL+"/v1/embeddings", `{"model":"emb","input":"x"}`); r.StatusCode != 200 {
		t.Fatalf("per-model fault leaked: %d", r.StatusCode)
	}
}

// Covers: ENV-04
func TestENV04_SlowTokensAndMidStreamDisconnect(t *testing.T) {
	in, srv := newSim(t)
	in.SetFaults("m1", Faults{TokensPerSec: 50, DropAfterTokens: 3})
	body := `{"model":"m1","stream":true,"messages":[{"role":"user","content":"one two three four five six seven"}]}`
	r := post(t, srv.URL+"/v1/chat/completions", body)
	defer r.Body.Close()
	sc := bufio.NewScanner(r.Body)
	var chunks int
	sawDone := false
	start := time.Now()
	for sc.Scan() {
		line := sc.Text()
		if line == "data: [DONE]" {
			sawDone = true
		}
		if strings.HasPrefix(line, "data: {") {
			chunks++
		}
	}
	if sawDone {
		t.Fatal("dropped stream should not end with [DONE]")
	}
	if chunks != 4 { // role chunk + 3 tokens
		t.Fatalf("expected 4 chunks before drop, got %d", chunks)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatal("slow token rate not applied")
	}
}

// Covers: ENV-04
func TestENV04_OracleTierSkill(t *testing.T) {
	in := New("o", Model{ID: "small", Tier: 0}, Model{ID: "big", Tier: 3})
	in.LoadOracle(map[string]OracleItem{"What is the capital of France?": {Reference: "Paris", Difficulty: 3}})
	srv := httptest.NewServer(in.Handler())
	defer srv.Close()
	ask := func(model string) string {
		r := post(t, srv.URL+"/v1/chat/completions", `{"model":"`+model+`","messages":[{"role":"user","content":"what is the capital of   France?"}]}`)
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}
	if !strings.Contains(ask("big"), `"Paris"`) {
		t.Fatal("tier-3 model should answer a difficulty-3 item")
	}
	if strings.Contains(ask("small"), `"content":"Paris"`) {
		t.Fatal("tier-0 model should not reliably answer a difficulty-3 item")
	}
}

// Covers: ENV-04
func TestENV04_EmbeddingsSimilarity(t *testing.T) {
	cos := func(a, b []float64) float64 {
		var s float64
		for i := range a {
			s += a[i] * b[i]
		}
		return s
	}
	a := Embed("What are your store opening hours?", 256)
	b := Embed("Could you tell me the opening hours of your store?", 256)
	c := Embed("Write a haiku about the ocean", 256)
	if cos(a, b) < 0.9 || cos(a, c) > 0.3 {
		t.Fatalf("paraphrase %.2f unrelated %.2f", cos(a, b), cos(a, c))
	}
}
