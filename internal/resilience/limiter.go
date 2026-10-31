package resilience

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Limiter enforces a concurrency limit per deployment with a bounded queue wait; when
// the wait expires the caller spills to the next candidate [RES-07].
type Limiter struct {
	mu    sync.Mutex
	slots map[string]*slot
}

type slot struct {
	ch       chan struct{} // nil = unlimited
	limit    int
	inFlight atomic.Int64
	queued   atomic.Int64
}

func NewLimiter() *Limiter { return &Limiter{slots: map[string]*slot{}} }

func (l *Limiter) get(dep string, limit int) *slot {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.slots[dep]
	if !ok || s.limit != limit {
		ns := &slot{limit: limit}
		if limit > 0 {
			ns.ch = make(chan struct{}, limit)
		}
		if ok {
			// Limit changed on reload: new requests use the new semaphore; in-flight
			// requests release into the old one.
			ns.inFlight.Store(s.inFlight.Load())
		}
		l.slots[dep] = ns
		s = ns
	}
	return s
}

// Acquire waits up to wait for a slot. The returned release must be called exactly once.
func (l *Limiter) Acquire(ctx context.Context, dep string, limit int, wait time.Duration) (release func(), ok bool) {
	s := l.get(dep, limit)
	if s.ch == nil {
		s.inFlight.Add(1)
		return func() { s.inFlight.Add(-1) }, true
	}
	select {
	case s.ch <- struct{}{}:
	default:
		if wait <= 0 {
			return nil, false
		}
		s.queued.Add(1)
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case s.ch <- struct{}{}:
			s.queued.Add(-1)
		case <-t.C:
			s.queued.Add(-1)
			return nil, false
		case <-ctx.Done():
			s.queued.Add(-1)
			return nil, false
		}
	}
	s.inFlight.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			s.inFlight.Add(-1)
			<-s.ch
		})
	}, true
}

// InFlight returns current in-flight requests for dep.
func (l *Limiter) InFlight(dep string) int {
	l.mu.Lock()
	s, ok := l.slots[dep]
	l.mu.Unlock()
	if !ok {
		return 0
	}
	return int(s.inFlight.Load())
}

// Queued returns requests waiting for a slot on dep (queue depth metric).
func (l *Limiter) Queued(dep string) int {
	l.mu.Lock()
	s, ok := l.slots[dep]
	l.mu.Unlock()
	if !ok {
		return 0
	}
	return int(s.queued.Load())
}
