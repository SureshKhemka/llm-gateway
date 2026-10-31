package resilience

import (
	"context"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// Covers: RES-01, RES-02
func TestRES02_BreakerOpensOnErrorRateAndRecoversHalfOpen(t *testing.T) {
	c := &clock{t: time.Unix(1e9, 0)}
	tr := NewTracker(BreakerConfig{Window: time.Minute, MinRequests: 4, ErrorRate: 0.5, CoolDown: 10 * time.Second})
	tr.Now = c.now
	var transitions []string
	var mu sync.Mutex
	tr.OnChange = func(_ string, from, to BreakerState, _ string) {
		mu.Lock()
		transitions = append(transitions, string(from)+"→"+string(to))
		mu.Unlock()
	}
	for i := 0; i < 3; i++ {
		tr.Record("d", true, 100, "")
	}
	if h, ok := tr.Available("d"); !ok || h != Healthy {
		t.Fatalf("expected healthy, got %s %v", h, ok)
	}
	tr.Record("d", false, 0, "boom")
	if st := tr.Status("d"); st.Health != Degraded || st.Breaker != Closed {
		t.Fatalf("25%% errors should be degraded: %+v", st)
	}
	tr.Record("d", false, 0, "boom")
	tr.Record("d", false, 0, "boom")
	if _, ok := tr.Available("d"); ok {
		t.Fatal("breaker should be open at 50% errors")
	}
	if tr.Acquire("d") {
		t.Fatal("open breaker admitted a request before cool-down")
	}
	c.add(11 * time.Second)
	if _, ok := tr.Available("d"); !ok {
		t.Fatal("after cool-down the breaker should admit a probe")
	}
	if !tr.Acquire("d") || tr.Acquire("d") {
		t.Fatal("half-open must admit exactly one probe")
	}
	tr.Record("d", false, 0, "still down")
	if tr.Status("d").Breaker != Open {
		t.Fatal("failed probe should re-open")
	}
	c.add(11 * time.Second)
	if !tr.Acquire("d") {
		t.Fatal("second probe refused")
	}
	tr.Record("d", true, 100, "")
	if st := tr.Status("d"); st.Breaker != Closed || st.Health != Healthy {
		t.Fatalf("successful probe should close: %+v", st)
	}
	time.Sleep(10 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(transitions) != 5 {
		t.Fatalf("transitions %v", transitions)
	}
}

// Covers: RES-02
func TestRES02_BreakerOpensOnLatency(t *testing.T) {
	tr := NewTracker(BreakerConfig{MinRequests: 3, LatencyP95Ms: 1000})
	for i := 0; i < 3; i++ {
		tr.Record("slow", true, 2500, "")
	}
	if st := tr.Status("slow"); st.Breaker != Open {
		t.Fatalf("p95 2500ms > 1000ms should open: %+v", st)
	}
}

// Covers: RES-01
func TestRES01_ProbeMarksUnloadedModelColdAndUnhealthy(t *testing.T) {
	tr := NewTracker(BreakerConfig{MinRequests: 3})
	tr.ProbeResult("d", true, false, "")
	if st := tr.Status("d"); st.Health != Unhealthy || !st.Cold {
		t.Fatalf("unlisted model: %+v", st)
	}
	tr.ProbeResult("d", true, true, "")
	if st := tr.Status("d"); st.Cold {
		t.Fatalf("listed again: %+v", st)
	}
}

// Covers: RES-05
func TestRES05_BackoffJitterAndRetryAfter(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		d := Backoff(2, 100*time.Millisecond, 2*time.Second, 0)
		if d < 200*time.Millisecond || d > 400*time.Millisecond {
			t.Fatalf("attempt 2 backoff %v outside [200ms,400ms]", d)
		}
		seen[d] = true
	}
	if len(seen) < 10 {
		t.Fatal("backoff has no jitter")
	}
	if d := Backoff(10, 100*time.Millisecond, time.Second, 0); d > time.Second {
		t.Fatalf("backoff above max: %v", d)
	}
	if d := Backoff(0, 100*time.Millisecond, time.Second, 3*time.Second); d != 3*time.Second {
		t.Fatalf("Retry-After not honoured: %v", d)
	}
}

// Covers: RES-07
func TestRES07_LimiterBoundedQueueWait(t *testing.T) {
	l := NewLimiter()
	r1, ok := l.Acquire(context.Background(), "d", 1, 0)
	if !ok || l.InFlight("d") != 1 {
		t.Fatal("first acquire")
	}
	start := time.Now()
	if _, ok := l.Acquire(context.Background(), "d", 1, 50*time.Millisecond); ok {
		t.Fatal("second acquire should time out")
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatal("did not wait for the queue timeout")
	}
	go func() { time.Sleep(20 * time.Millisecond); r1() }()
	r2, ok := l.Acquire(context.Background(), "d", 1, time.Second)
	if !ok {
		t.Fatal("queued acquire should succeed after release")
	}
	r2()
	r2() // double release is harmless
	if l.InFlight("d") != 0 {
		t.Fatalf("in flight %d", l.InFlight("d"))
	}
}
