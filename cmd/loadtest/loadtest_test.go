package main

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/testenv"
	"github.com/alicebob/miniredis/v2"
)

// The load harness against real gateway and simulator processes (separate processes, so
// each has its own file-descriptor limit; 5,000 streams need ~20,000 sockets in all).
// The numbers are a lower bound on this machine: gateway, simulator and load generator
// share its CPUs, while NFR-03 is stated for a dedicated 4 vCPU instance.
// Covers: NFR-03, NFR-09
func TestNFR03_ThroughputAndStreamsAgainstRealProcesses(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("load test needs an uninstrumented build (and the race detector caps goroutines at 8128)")
	}
	testenv.ExclusiveCPU(t)
	bin := t.TempDir()
	build := exec.Command("go", "build", "-o", bin+"/", "../gateway", "../simprovider")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	mr := miniredis.RunT(t) // shared counters and cache, as in a multi-instance deployment
	simA, simB := freeAddr(t), freeAddr(t)
	start(t, bin+"/simprovider", nil, "-instance", "sima="+simA, "-instance", "simb="+simB)
	env := []string{"SIMA_URL=http://" + simA + "/v1", "SIMB_URL=http://" + simB + "/v1", "REDIS_URL=redis://" + mr.Addr(),
		"DEMO_KEY_A=gw-load-a_aaaaaaaaaaaaaaaa", "DEMO_KEY_B=gw-load-b_bbbbbbbbbbbbbbbb", "DEMO_ADMIN_KEY=gw-load-admin"}
	var gws []string
	for i := 0; i < 2; i++ {
		addr := freeAddr(t)
		start(t, bin+"/gateway", append(env, "GATEWAY_LISTEN="+addr), "-config", "../../deploy/demo/gateway.yaml")
		gws = append(gws, "http://"+addr)
	}
	for _, u := range append([]string{"http://" + simA + "/v1/models"}, gws[0]+"/readyz", gws[1]+"/readyz") {
		waitUp(t, u)
	}
	key := "gw-load-a_aaaaaaaaaaaaaaaa"
	base := options{Key: key, Model: "auto", Prompt: "Extract the order number from: order #%d shipped to Pune", Duration: 6 * time.Second}

	one := base
	one.URLs, one.Concurrency = gws[:1], 128
	s1 := run(one)
	t.Logf("1 instance: %.0f req/s, p95 %.1f ms, %d errors", s1.Throughput, s1.LatencyMs["p95"], s1.Errors)
	if s1.Errors > 0 || s1.Throughput < 1000 {
		t.Errorf("NFR-03: %.0f req/s with %d errors (want ≥ 1,000 and none)", s1.Throughput, s1.Errors)
	}

	// Horizontal scaling: two instances behind client-side round robin share Redis state.
	// Both must serve without errors; linear speed-up can't show on shared CPUs, so it is
	// measured by scripts/load.sh (INSTANCES=N) on separate hosts.
	two := one
	two.URLs = gws
	s2 := run(two)
	t.Logf("2 instances (same CPUs): %.0f req/s, %d errors", s2.Throughput, s2.Errors)
	if s2.Errors > 0 {
		t.Errorf("NFR-09: %d errors across two instances", s2.Errors)
	}

	// 5,000 concurrent streams; slow token rate so they overlap.
	setFaults(t, "http://"+simA, `{"model":"","faults":{"tokens_per_sec":20}}`)
	setFaults(t, "http://"+simB, `{"model":"","faults":{"tokens_per_sec":20}}`)
	st := base
	st.URLs, st.Streams, st.Duration = gws[:1], 5000, 10*time.Second
	ss := run(st)
	t.Logf("streams: %d ok, %d errors, peak open %d, TTFT p50 %.0f ms p95 %.0f ms",
		ss.OK, ss.Errors, ss.PeakStreams, ss.TTFTMs["p50"], ss.TTFTMs["p95"])
	if ss.Errors > 0 || ss.PeakStreams < 4500 {
		t.Errorf("NFR-03: %d stream errors, peak %d open (want 0 and ~5,000)", ss.Errors, ss.PeakStreams)
	}
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func start(t *testing.T, bin string, env []string, args ...string) {
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	logf, _ := os.Create(filepath.Join(t.TempDir(), filepath.Base(bin)+".log"))
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		logf.Close()
	})
}

func waitUp(t *testing.T, url string) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := http.Get(url); err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s not up", url)
}

func setFaults(t *testing.T, base, body string) {
	r, err := http.Post(base+"/_sim/faults", "application/json", strings.NewReader(body))
	if err != nil || r.StatusCode != 204 {
		t.Fatalf("set faults: %v %v", err, r)
	}
	r.Body.Close()
}
