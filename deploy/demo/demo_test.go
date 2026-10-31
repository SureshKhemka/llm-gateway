// Package demo holds the test for the one-command demo (up.sh / down.sh).
package demo

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/testenv"
)

// up.sh brings up gateway, simulated providers and seeded tenants and keys with one
// command; down.sh tears everything down. The default run uses the no-Docker path;
// DEMO_TEST_DOCKER=1 runs the Docker Compose stack (Redis, Postgres, Prometheus, Grafana).
// Covers: ENV-07, NFR-17
func TestENV07_DemoUpAndDown(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and starts the demo processes")
	}
	if _, err := os.Stat(".run/mode"); err == nil {
		t.Skip("a demo is already running here (deploy/demo/down.sh first)")
	}
	testenv.ExclusiveCPU(t)
	port := freePort(t)
	env := append(os.Environ(), "GATEWAY_PORT="+port, "SIMA_PORT="+freePort(t), "SIMB_PORT="+freePort(t), "REDIS_PORT="+freePort(t))
	docker := os.Getenv("DEMO_TEST_DOCKER") == "1"
	if !docker {
		env = append(env, "DEMO_NO_DOCKER=1")
	}
	t.Cleanup(func() {
		cmd := exec.Command("./down.sh")
		cmd.Env = env
		_ = cmd.Run()
	})
	up := exec.Command("./up.sh")
	up.Env = env
	out, err := up.CombinedOutput()
	if err != nil {
		t.Fatalf("up.sh: %v\n%s", err, out)
	}
	// The seeded keys are printed for the user.
	var keyA, keyB, admin string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		for i, w := range f {
			switch {
			case strings.HasPrefix(w, "api_key="):
				keyA = strings.TrimPrefix(w, "api_key=")
			case strings.HasPrefix(w, "ANTHROPIC_API_KEY="):
				keyB = strings.TrimPrefix(w, "ANTHROPIC_API_KEY=")
			case w == "key:" && i+1 < len(f):
				admin = f[i+1]
			}
		}
	}
	if keyA == "" || keyB == "" || admin == "" {
		t.Fatalf("up.sh did not print the seeded keys:\n%s", out)
	}
	base := "http://127.0.0.1:" + port
	post := func(path, key, body string, hdr ...string) int {
		req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if s := post("/v1/chat/completions", keyA, `{"model":"auto","messages":[{"role":"user","content":"hello"}]}`); s != 200 {
		t.Fatalf("OpenAI chat with seeded key A: %d", s)
	}
	if s := post("/v1/messages", keyB, `{"model":"claude-sonnet-4","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`,
		"anthropic-version", "2023-06-01"); s != 200 {
		t.Fatalf("Anthropic messages with seeded key B: %d", s)
	}
	req, _ := http.NewRequest("GET", base+"/admin/v1/tenants", nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("admin API with seeded admin key: %v %v", err, resp)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "team-a") || !strings.Contains(string(b), "team-b") {
		t.Fatalf("seeded tenants missing: %s", b)
	}
	if r, err := http.Get(base + "/console/"); err != nil || r.StatusCode != 200 {
		t.Fatalf("console: %v %v", err, r)
	}

	down := exec.Command("./down.sh")
	down.Env = env
	if out, err := down.CombinedOutput(); err != nil {
		t.Fatalf("down.sh: %v\n%s", err, out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("gateway still listening after down.sh")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := os.Stat(".run/env"); err == nil {
		t.Error("down.sh left the generated keys behind")
	}
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p
}
