// Command loadtest drives a gateway at a target rate or concurrency and reports
// throughput, latency percentiles and gateway-added overhead [NFR-01..NFR-04, NFR-09].
//
//	loadtest -url http://localhost:8080 -key $KEY -duration 30s -concurrency 256
//	loadtest -url ... -streams 5000 -duration 60s       # concurrent streaming sessions
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type sample struct {
	ok       bool
	latency  time.Duration
	ttft     time.Duration
	overhead float64 // ms, from the gateway's own accounting when available
}

// options are the load parameters (flags in main, set directly by tests).
type options struct {
	URLs        []string
	Key, Model  string
	Concurrency int
	Streams     int
	Duration    time.Duration
	RPS         float64
	Cache       bool
	Prompt      string
}

// summary is the result of one run.
type summary struct {
	Mode       string             `json:"mode"`
	Workers    int                `json:"workers"`
	DurationS  float64            `json:"duration_s"`
	Requests   int                `json:"requests"`
	OK         int                `json:"ok"`
	Errors     int                `json:"errors"`
	Throughput float64            `json:"throughput_rps"`
	LatencyMs  map[string]float64 `json:"latency_ms"`
	TTFTMs     map[string]float64 `json:"ttft_ms,omitempty"`
	// PeakStreams is the most streams open at once (first byte seen, not yet finished).
	PeakStreams int `json:"peak_open_streams,omitempty"`
}

func main() {
	url := flag.String("url", "http://localhost:8080", "gateway base URL (comma-separated for several instances)")
	key := flag.String("key", os.Getenv("GATEWAY_KEY"), "virtual API key")
	model := flag.String("model", "auto", "model")
	conc := flag.Int("concurrency", 64, "concurrent non-streaming workers")
	streams := flag.Int("streams", 0, "concurrent streaming sessions (instead of non-streaming load)")
	dur := flag.Duration("duration", 30*time.Second, "test duration")
	rps := flag.Float64("rps", 0, "target requests/s (0 = as fast as concurrency allows)")
	cache := flag.Bool("cache", false, "allow cache hits (default sends X-Gw-Cache: bypass)")
	prompt := flag.String("prompt", "Extract the order number from: order #%d shipped to Pune", "prompt (%d = request number)")
	out := flag.String("json", "", "write the summary as JSON to this file")
	flag.Parse()

	sum := run(options{URLs: strings.Split(*url, ","), Key: *key, Model: *model, Concurrency: *conc, Streams: *streams,
		Duration: *dur, RPS: *rps, Cache: *cache, Prompt: *prompt})
	b, _ := json.MarshalIndent(sum, "", "  ")
	fmt.Println(string(b))
	if *out != "" {
		_ = os.WriteFile(*out, b, 0o644)
	}
	if sum.Errors > 0 {
		os.Exit(1)
	}
}

func run(o options) summary {
	urls := o.URLs
	tr := &http.Transport{MaxIdleConns: 100000, MaxIdleConnsPerHost: 100000, MaxConnsPerHost: 0, IdleConnTimeout: 90 * time.Second,
		DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), o.Duration)
	defer cancel()
	var open, peak atomic.Int64

	var mu sync.Mutex
	var samples []sample
	var seq atomic.Int64
	record := func(s sample) { mu.Lock(); samples = append(samples, s); mu.Unlock() }
	var tick <-chan time.Time
	if o.RPS > 0 {
		t := time.NewTicker(time.Duration(float64(time.Second) / o.RPS))
		defer t.Stop()
		tick = t.C
	}

	one := func(stream bool) {
		n := seq.Add(1)
		body, _ := json.Marshal(map[string]any{"model": o.Model, "stream": stream,
			"messages": []map[string]string{{"role": "user", "content": strings.ReplaceAll(o.Prompt, "%d", fmt.Sprint(n))}}})
		req, _ := http.NewRequestWithContext(context.Background(), "POST", urls[int(n)%len(urls)]+"/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+o.Key)
		if !o.Cache {
			req.Header.Set("X-Gw-Cache", "bypass")
		}
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			record(sample{latency: time.Since(start)})
			return
		}
		s := sample{ok: resp.StatusCode == 200}
		if stream {
			rd := bufio.NewReader(resp.Body)
			for {
				line, err := rd.ReadString('\n')
				if s.ttft == 0 && strings.HasPrefix(line, "data: {") {
					s.ttft = time.Since(start)
					if n := open.Add(1); n > peak.Load() {
						peak.Store(n) // approximate under contention; good enough for a gauge
					}
				}
				if err != nil {
					break
				}
			}
			if s.ttft > 0 {
				open.Add(-1)
			}
		} else {
			io.Copy(io.Discard, resp.Body)
		}
		resp.Body.Close()
		s.latency = time.Since(start)
		record(s)
	}

	var wg sync.WaitGroup
	workers := o.Concurrency
	stream := false
	if o.Streams > 0 {
		workers, stream = o.Streams, true
	}
	started := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if tick != nil {
					select {
					case <-tick:
					case <-ctx.Done():
						return
					}
				}
				one(stream)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(started)

	var lat, ttft []float64
	okN := 0
	for _, s := range samples {
		if s.ok {
			okN++
			lat = append(lat, float64(s.latency.Microseconds())/1000)
			if s.ttft > 0 {
				ttft = append(ttft, float64(s.ttft.Microseconds())/1000)
			}
		}
	}
	pct := func(xs []float64, p float64) float64 {
		if len(xs) == 0 {
			return 0
		}
		sort.Float64s(xs)
		return xs[min(len(xs)-1, int(p*float64(len(xs))))]
	}
	sum := summary{Mode: map[bool]string{true: "streaming", false: "non-streaming"}[stream], Workers: workers,
		DurationS: elapsed.Seconds(), Requests: len(samples), OK: okN, Errors: len(samples) - okN,
		Throughput: float64(okN) / elapsed.Seconds(),
		LatencyMs:  map[string]float64{"p50": pct(lat, .5), "p95": pct(lat, .95), "p99": pct(lat, .99)}}
	if stream {
		sum.TTFTMs = map[string]float64{"p50": pct(ttft, .5), "p95": pct(ttft, .95), "p99": pct(ttft, .99)}
		sum.PeakStreams = int(peak.Load())
	}
	return sum
}
