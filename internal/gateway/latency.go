package gateway

import (
	"sync"
	"time"
)

// LatencyTracker keeps rolling (EWMA) time-to-first-token and tokens-per-second per
// deployment from live traffic [RTE-07].
type LatencyTracker struct {
	mu    sync.RWMutex
	alpha float64
	stats map[string]*latStat
	// MinSamples before observed values replace the static profile.
	MinSamples int
	// MaxAge after which observations are ignored (no recent traffic).
	MaxAge time.Duration
}

type latStat struct {
	ttft, tps float64
	n         int
	last      time.Time
}

func NewLatencyTracker(alpha float64) *LatencyTracker {
	if alpha <= 0 || alpha > 1 {
		alpha = 0.2
	}
	return &LatencyTracker{alpha: alpha, stats: map[string]*latStat{}, MinSamples: 3, MaxAge: 10 * time.Minute}
}

// Observe records one completed attempt. ttftMs is the time to first token (or full
// response for non-streaming); genMs is the time spent generating after that.
func (l *LatencyTracker) Observe(dep string, ttftMs float64, outTokens int, genMs float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.stats[dep]
	if !ok {
		s = &latStat{}
		l.stats[dep] = s
	}
	tps := 0.0
	if outTokens > 0 && genMs > 1 {
		tps = float64(outTokens) / (genMs / 1000)
	}
	if s.n == 0 {
		s.ttft = ttftMs
		s.tps = tps
	} else {
		s.ttft = l.alpha*ttftMs + (1-l.alpha)*s.ttft
		if tps > 0 {
			if s.tps == 0 {
				s.tps = tps
			} else {
				s.tps = l.alpha*tps + (1-l.alpha)*s.tps
			}
		}
	}
	s.n++
	s.last = time.Now()
}

// Observed returns rolling TTFT (ms) and TPS for a deployment if enough recent samples exist.
func (l *LatencyTracker) Observed(dep string) (ttftMs, tps float64, ok bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s, found := l.stats[dep]
	if !found || s.n < l.MinSamples || time.Since(s.last) > l.MaxAge {
		return 0, 0, false
	}
	tps = s.tps
	if tps == 0 {
		tps = 1e9 // non-streaming only: TTFT already includes generation
	}
	return s.ttft, tps, true
}

// Snapshot returns all tracked stats (for the console and metrics).
func (l *LatencyTracker) Snapshot() map[string][2]float64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := map[string][2]float64{}
	for k, s := range l.stats {
		out[k] = [2]float64{s.ttft, s.tps}
	}
	return out
}
