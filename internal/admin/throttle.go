package admin

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// authThrottle limits failed admin authentications per client address (ASVS V2.2.1
// anti-automation) [NFR-13]. Admin keys are random 128-bit+ tokens, so this is defence
// in depth against credential stuffing and noisy scanners, not the primary control.
type authThrottle struct {
	mu        sync.Mutex
	fails     map[string]*failWindow
	limit     int
	window    time.Duration
	lockout   time.Duration
	now       func() time.Time
	lastPrune time.Time
}

type failWindow struct {
	start, until time.Time
	n            int
}

func newAuthThrottle() *authThrottle {
	return &authThrottle{fails: map[string]*failWindow{}, limit: 10, window: time.Minute, lockout: time.Minute, now: time.Now}
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// blocked reports whether addr is locked out and for how long.
func (t *authThrottle) blocked(addr string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if f := t.fails[addr]; f != nil && t.now().Before(f.until) {
		return f.until.Sub(t.now()), true
	}
	return 0, false
}

func (t *authThrottle) fail(addr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if now.Sub(t.lastPrune) > t.window {
		for k, f := range t.fails {
			if now.Sub(f.start) > t.window && now.After(f.until) {
				delete(t.fails, k)
			}
		}
		t.lastPrune = now
	}
	f := t.fails[addr]
	if f == nil || now.Sub(f.start) > t.window {
		f = &failWindow{start: now}
		t.fails[addr] = f
	}
	f.n++
	if f.n >= t.limit {
		f.until = now.Add(t.lockout)
	}
}

func (t *authThrottle) succeed(addr string) {
	t.mu.Lock()
	delete(t.fails, addr)
	t.mu.Unlock()
}
