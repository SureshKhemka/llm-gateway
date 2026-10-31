// Command chaos runs the chaos suite against real processes [NFR-21, RES-01..RES-10,
// NFR-05, NFR-06]: two simulated providers (separate processes), two gateway instances
// sharing Redis behind a round-robin load balancer, and steady mixed load. Each phase
// injects one fault and checks what clients see.
//
//	scripts/chaos.sh                  # builds the binaries and runs every phase
//	chaos -bin ./bin -phase 20s -json out.json
//
// Phases: provider kill (and recovery), provider slowdown, 429 storm, mid-stream drops,
// gateway instance kill.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type sample struct {
	start, end time.Time
	ok         bool
	stream     bool
	model      string // served deployment
	asked      string // requested model
	status     int
	err        string
}

type proc struct {
	name string
	cmd  *exec.Cmd
	args []string
	env  []string
	log  string
}

func (p *proc) start() error {
	p.cmd = exec.Command(p.args[0], p.args[1:]...)
	p.cmd.Env = append(os.Environ(), p.env...)
	f, err := os.OpenFile(p.log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	p.cmd.Stdout, p.cmd.Stderr = f, f
	return p.cmd.Start()
}

func (p *proc) kill() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGKILL)
		_, _ = p.cmd.Process.Wait()
	}
}

type phaseResult struct {
	Name     string         `json:"name"`
	Pass     bool           `json:"pass"`
	Checks   []string       `json:"checks"`
	Requests int            `json:"requests"`
	Errors   int            `json:"errors"`
	P95Ms    float64        `json:"p95_ms"`
	Served   map[string]int `json:"served"`
	ErrorsBy map[string]int `json:"errors_by,omitempty"`
}

func main() {
	bin := flag.String("bin", "bin", "directory with gateway and simprovider binaries")
	cfg := flag.String("config", "deploy/demo/gateway.yaml", "gateway config")
	phaseDur := flag.Duration("phase", 15*time.Second, "duration of each phase")
	workers := flag.Int("workers", 24, "concurrent clients")
	out := flag.String("json", "", "write results as JSON")
	only := flag.String("only", "", "comma-separated phases to run (default all)")
	redisURL := flag.String("redis", os.Getenv("REDIS_URL"), "Redis shared by the instances (empty: start redis-server if installed)")
	flag.Parse()
	res, err := run(*bin, *cfg, *phaseDur, *workers, *redisURL, *only)
	if err != nil {
		fmt.Fprintln(os.Stderr, "chaos:", err)
		os.Exit(2)
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	if *out != "" {
		_ = os.WriteFile(*out, b, 0o644)
	}
	for _, r := range res {
		if !r.Pass {
			os.Exit(1)
		}
	}
}

func freeAddr() string {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	return l.Addr().String()
}

func waitUp(url string) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := http.Get(url); err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s not up", url)
}

func run(bin, cfgPath string, phaseDur time.Duration, workers int, redisURL, only string) ([]phaseResult, error) {
	logDir, _ := os.MkdirTemp("", "gw-chaos-")
	var procs []*proc
	defer func() {
		for _, p := range procs {
			p.kill()
		}
	}()
	if redisURL == "" {
		if rs, err := exec.LookPath("redis-server"); err == nil {
			addr := freeAddr()
			_, port, _ := net.SplitHostPort(addr)
			p := &proc{name: "redis", args: []string{rs, "--port", port, "--bind", "127.0.0.1", "--save", "", "--appendonly", "no"}, log: filepath.Join(logDir, "redis.log")}
			if err := p.start(); err != nil {
				return nil, err
			}
			procs = append(procs, p)
			redisURL = "redis://" + addr + "/0"
			time.Sleep(300 * time.Millisecond)
		}
	}
	simA, simB := freeAddr(), freeAddr()
	sa := &proc{name: "sima", args: []string{filepath.Join(bin, "simprovider"), "-instance", "sima=" + simA}, log: filepath.Join(logDir, "sima.log")}
	sb := &proc{name: "simb", args: []string{filepath.Join(bin, "simprovider"), "-instance", "simb=" + simB}, log: filepath.Join(logDir, "simb.log")}
	for _, p := range []*proc{sa, sb} {
		if err := p.start(); err != nil {
			return nil, err
		}
		procs = append(procs, p)
	}
	env := []string{"SIMA_URL=http://" + simA + "/v1", "SIMB_URL=http://" + simB + "/v1",
		"DEMO_KEY_A=gw-chaos-a_aaaaaaaaaaaaaaaa", "DEMO_KEY_B=gw-chaos-b_bbbbbbbbbbbbbbbb", "DEMO_ADMIN_KEY=gw-chaos-admin"}
	if redisURL != "" {
		env = append(env, "REDIS_URL="+redisURL)
	}
	var gws []*proc
	var backends []string
	for i := 0; i < 2; i++ {
		addr := freeAddr()
		g := &proc{name: fmt.Sprintf("gateway-%d", i+1), args: []string{filepath.Join(bin, "gateway"), "-config", cfgPath},
			env: append(append([]string{}, env...), "GATEWAY_LISTEN="+addr, "WAL_DIR="+filepath.Join(logDir, fmt.Sprintf("wal-%d", i))),
			log: filepath.Join(logDir, fmt.Sprintf("gateway-%d.log", i+1))}
		if err := g.start(); err != nil {
			return nil, err
		}
		procs = append(procs, g)
		gws = append(gws, g)
		backends = append(backends, "http://"+addr)
	}
	for _, u := range []string{"http://" + simA + "/v1/models", "http://" + simB + "/v1/models", backends[0] + "/readyz", backends[1] + "/readyz"} {
		if err := waitUp(u); err != nil {
			return nil, fmt.Errorf("%w (logs in %s)", err, logDir)
		}
	}
	fmt.Fprintln(os.Stderr, "chaos: logs in", logDir)

	lb := newLB(backends)
	lbSrv := &http.Server{Handler: lb}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go lbSrv.Serve(ln)
	defer lbSrv.Close()
	base := "http://" + ln.Addr().String()

	d := &driver{base: base, key: "gw-chaos-a_aaaaaaaaaaaaaaaa", client: &http.Client{Timeout: 60 * time.Second,
		Transport: &http.Transport{MaxIdleConnsPerHost: 1000}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for ctx.Err() == nil {
				d.one(w%3 == 0) // a third of the clients stream
			}
		}(w)
	}
	faults := func(addr, body string) {
		r, err := http.Post("http://"+addr+"/_sim/faults", "application/json", strings.NewReader(body))
		if err == nil {
			r.Body.Close()
		}
	}
	clear := func(addr string) {
		req, _ := http.NewRequest("DELETE", "http://"+addr+"/_sim/faults", nil)
		if r, err := http.DefaultClient.Do(req); err == nil {
			r.Body.Close()
		}
	}
	var results []phaseResult
	phase := func(name string, setup func(), checks func(all, late []sample) []string, teardown func()) {
		if only != "" && !strings.Contains(","+only+",", ","+name+",") {
			return
		}
		fmt.Fprintf(os.Stderr, "chaos: phase %s\n", name)
		t0 := time.Now()
		setup()
		time.Sleep(phaseDur)
		t1 := time.Now()
		teardown()
		time.Sleep(3 * time.Second) // let in-flight requests started in the phase finish
		all := d.window(t0, t1)
		late := d.window(t0.Add(phaseDur/2), t1)
		var failed []string
		for _, c := range checks(all, late) {
			if c != "" {
				failed = append(failed, c)
			}
		}
		pr := summarize(name, all)
		pr.Pass, pr.Checks = len(failed) == 0, failed
		if pr.Pass {
			pr.Checks = []string{"ok"}
		}
		results = append(results, pr)
		time.Sleep(4 * time.Second) // recovery between phases (breaker cool-down)
	}
	errRate := func(s []sample) float64 {
		if len(s) == 0 {
			return 1
		}
		n := 0
		for _, x := range s {
			if !x.ok {
				n++
			}
		}
		return float64(n) / float64(len(s))
	}
	share := func(s []sample, provider string) float64 {
		if len(s) == 0 {
			return 0
		}
		n := 0
		for _, x := range s {
			if x.ok && strings.HasPrefix(x.model, provider+"/") {
				n++
			}
		}
		return float64(n) / float64(len(s))
	}
	need := func(cond bool, format string, args ...any) string {
		if cond {
			return ""
		}
		return fmt.Sprintf(format, args...)
	}
	time.Sleep(3 * time.Second) // warm-up

	phase("baseline", func() {}, func(all, late []sample) []string {
		return []string{need(errRate(all) == 0, "baseline error rate %.3f", errRate(all)), need(len(all) > 50, "only %d requests", len(all))}
	}, func() {})

	phase("provider_kill", func() { sa.kill() }, func(all, late []sample) []string {
		return []string{
			need(errRate(all) <= 0.02, "error rate %.3f with sima killed (want ≤ 2%%: only streams cut mid-flight)", errRate(all)),
			need(errRate(late) == 0, "errors continue after failover: %.3f", errRate(late)),
			need(share(late, "sima") == 0, "traffic still served by the dead provider"),
		}
	}, func() { _ = sa.start(); _ = waitUp("http://" + simA + "/v1/models") })

	phase("provider_recovery", func() {}, func(all, late []sample) []string {
		return []string{
			need(errRate(all) == 0, "errors during recovery: %.3f", errRate(all)),
			need(share(late, "sima") > 0.2, "traffic did not return to the cheaper provider (sima share %.2f)", share(late, "sima")),
		}
	}, func() {})

	// min_cost traffic tolerates a slow but working cheap provider by design; traffic
	// under the latency-sensitive policy must move to the fast one.
	phase("provider_slowdown", func() { faults(simA, `{"model":"","faults":{"latency_ms":2500}}`) }, func(all, late []sample) []string {
		lat := filter(late, func(s sample) bool { return s.asked == "latency" })
		return []string{
			need(errRate(all) <= 0.01, "error rate %.3f during slowdown", errRate(all)),
			need(len(lat) > 0 && share(lat, "sima") < 0.1, "latency-sensitive traffic did not move off the slow provider (sima share %.2f of %d)", share(lat, "sima"), len(lat)),
		}
	}, func() { clear(simA) })

	phase("rate_limit_storm", func() { faults(simA, `{"model":"","faults":{"rate_limit":true,"retry_after_sec":5}}`) }, func(all, late []sample) []string {
		return []string{need(errRate(all) <= 0.01, "client errors %.3f during the 429 storm (fallback should absorb it)", errRate(all))}
	}, func() { clear(simA) })

	phase("mid_stream_drop", func() { faults(simA, `{"model":"","faults":{"drop_after_tokens":2}}`) }, func(all, late []sample) []string {
		nonStream := filter(all, func(s sample) bool { return !s.stream })
		hung := filter(all, func(s sample) bool { return s.end.Sub(s.start) > 30*time.Second })
		return []string{
			need(len(hung) == 0, "%d requests hung on dropped streams", len(hung)),
			need(errRate(nonStream) <= 0.01, "non-streaming errors %.3f (should fall back)", errRate(nonStream)),
			need(errRate(late) <= 0.05, "stream errors continue after the breaker should have tripped: %.3f", errRate(late)),
		}
	}, func() { clear(simA) })

	var killedAt time.Time
	phase("instance_kill", func() { killedAt = time.Now(); time.Sleep(2 * time.Second); gws[1].kill(); lb.markDead(1) }, func(all, late []sample) []string {
		after := filter(all, func(s sample) bool { return s.start.After(killedAt.Add(2500 * time.Millisecond)) })
		inflight := filter(all, func(s sample) bool { return !s.ok })
		return []string{
			need(len(inflight) <= workers, "%d failures: more than the requests in flight at the kill", len(inflight)),
			need(errRate(after) == 0, "requests started after the kill failed: %.3f", errRate(after)),
		}
	}, func() {})

	cancel()
	wg.Wait()
	return results, nil
}

func filter(s []sample, f func(sample) bool) []sample {
	var out []sample
	for _, x := range s {
		if f(x) {
			out = append(out, x)
		}
	}
	return out
}

func summarize(name string, s []sample) phaseResult {
	pr := phaseResult{Name: name, Requests: len(s), Served: map[string]int{}, ErrorsBy: map[string]int{}}
	var lat []float64
	for _, x := range s {
		if !x.ok {
			pr.Errors++
			e := x.err
			if e == "" {
				e = fmt.Sprintf("HTTP %d", x.status)
			}
			if len(e) > 80 {
				e = e[:80]
			}
			pr.ErrorsBy[e]++
			continue
		}
		pr.Served[x.model]++
		lat = append(lat, float64(x.end.Sub(x.start).Milliseconds()))
	}
	if len(lat) > 0 {
		sort.Float64s(lat)
		pr.P95Ms = lat[min(len(lat)-1, len(lat)*95/100)]
	}
	return pr
}

type driver struct {
	base, key string
	client    *http.Client
	mu        sync.Mutex
	samples   []sample
	n         atomic.Int64
}

func (d *driver) window(from, to time.Time) []sample {
	d.mu.Lock()
	defer d.mu.Unlock()
	return filter(d.samples, func(s sample) bool { return !s.start.Before(from) && s.start.Before(to) })
}

var prompts = []string{
	"Extract the order number from: order #%d shipped to Pune",
	"Classify the sentiment of: the delivery %d was late again",
	"Summarise in one line: ticket %d reports a login loop after password reset",
	"Explain step by step how to compute compound interest for %d years",
}

func (d *driver) one(stream bool) {
	n := d.n.Add(1)
	model := "auto"
	if n%4 == 0 {
		model = "latency" // latency-sensitive policy (latency_target_ms)
	}
	body, _ := json.Marshal(map[string]any{"model": model, "stream": stream, "temperature": 0.9,
		"messages": []map[string]string{{"role": "user", "content": fmt.Sprintf(prompts[n%int64(len(prompts))], n)}}})
	req, _ := http.NewRequest("POST", d.base+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+d.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gw-Cache", "bypass")
	s := sample{start: time.Now(), stream: stream, asked: model}
	resp, err := d.client.Do(req)
	if err != nil {
		s.err = err.Error()
	} else {
		s.status, s.model = resp.StatusCode, resp.Header.Get("X-Gw-Served-Model")
		s.ok = resp.StatusCode == 200
		if stream && s.ok {
			done := false
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			for sc.Scan() {
				line := sc.Text()
				if strings.Contains(line, `"error"`) {
					s.ok, s.err = false, "stream error event"
				}
				if line == "data: [DONE]" {
					done = true
				}
			}
			if !done {
				s.ok = false
				if s.err == "" {
					s.err = "stream ended without [DONE]"
				}
			}
		} else if !s.ok {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
			s.err = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		} else {
			io.Copy(io.Discard, resp.Body)
		}
		resp.Body.Close()
	}
	s.end = time.Now()
	d.mu.Lock()
	d.samples = append(d.samples, s)
	d.mu.Unlock()
}

// lb is a round-robin load balancer that retries the next backend when a connection is
// refused (standard LB behaviour) and skips backends marked dead (health checks).
type lb struct {
	backends []string
	dead     []atomic.Bool
	next     atomic.Int64
	client   *http.Client
}

func newLB(b []string) *lb {
	return &lb{backends: b, dead: make([]atomic.Bool, len(b)), client: &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1000}}}
}

func (l *lb) markDead(i int) { l.dead[i].Store(true) }

func (l *lb) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	start := int(l.next.Add(1))
	for try := 0; try < len(l.backends); try++ {
		i := (start + try) % len(l.backends)
		if l.dead[i].Load() {
			continue
		}
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, l.backends[i]+r.URL.RequestURI(), bytes.NewReader(body))
		req.Header = r.Header.Clone()
		resp, err := l.client.Do(req)
		if err != nil {
			var op *net.OpError
			if errors.As(err, &op) && op.Op == "dial" {
				continue // nothing was sent: safe to retry elsewhere
			}
			http.Error(w, err.Error(), 502)
			return
		}
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		fl, _ := w.(http.Flusher)
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				if fl != nil {
					fl.Flush()
				}
			}
			if err != nil {
				break
			}
		}
		resp.Body.Close()
		return
	}
	http.Error(w, "no backend", 502)
}
