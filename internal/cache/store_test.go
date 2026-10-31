package cache

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func stores(t *testing.T) (map[string]Store, *miniredis.Miniredis) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { c.Close() })
	return map[string]Store{"memory": NewMemory(), "redis": NewRedis(c, "t:")}, mr
}

// The memory and Redis stores behave the same: misses, expiry, overwrite, bounded size,
// vector buckets (replace, cap), and invalidation by policy, model, age and namespace.
// Covers: CCH-01, CCH-06, CCH-08, BUD-05
func TestCCH08_StoreContract(t *testing.T) {
	ss, mr := stores(t)
	ctx := context.Background()
	for name, st := range ss {
		t.Run(name, func(t *testing.T) {
			if e, err := st.Get(ctx, "ns", "missing"); e != nil || err != nil {
				t.Fatalf("miss: %v %v", e, err)
			}
			old := time.Now().Add(-2 * time.Hour)
			put := func(ns, key, policy, model string, created time.Time, ttl time.Duration, max int) {
				t.Helper()
				if err := st.Put(ctx, &Entry{Key: key, Namespace: ns, Policy: policy, Model: model, Deployment: "dep-" + model, Created: created}, ttl, max); err != nil {
					t.Fatal(err)
				}
			}
			put("ns", "a", "p1", "m1", old, time.Hour, 0)
			put("ns", "a", "p1", "m1", old, time.Hour, 0) // overwrite keeps one entry
			put("ns", "b", "p2", "m2", time.Now(), time.Hour, 0)
			put("ns2", "c", "p1", "m1", time.Now(), time.Hour, 0)
			if e, _ := st.Get(ctx, "ns", "a"); e == nil || e.Policy != "p1" {
				t.Fatalf("get a: %+v", e)
			}
			// Expiry.
			put("ns", "short", "p", "m", time.Now(), 50*time.Millisecond, 0)
			if name == "redis" {
				mr.FastForward(time.Second)
			} else {
				time.Sleep(80 * time.Millisecond)
			}
			if e, _ := st.Get(ctx, "ns", "short"); e != nil {
				t.Error("expired entry returned")
			}
			// Vectors: same key replaces, the bucket is capped.
			for i, k := range []string{"v1", "v2", "v2", "v3"} {
				if err := st.PutVector(ctx, "ns", "b0", Candidate{Key: k, Vec: []float32{1, float32(i)}, At: int64(i)}, 2, time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			cands, err := st.Candidates(ctx, "ns", "b0")
			keys := map[string]bool{}
			for _, c := range cands {
				keys[c.Key] = true
			}
			if err != nil || len(cands) != 2 || keys["v1"] || !keys["v3"] {
				t.Errorf("bucket: %v %v", keys, err)
			}
			// Invalidation.
			if n, err := st.Invalidate(ctx, "ns", Filter{Policy: "p2"}); err != nil || n != 1 {
				t.Errorf("by policy: %d %v", n, err)
			}
			if n, _ := st.Invalidate(ctx, "ns", Filter{Model: "dep-m1", OlderThan: time.Hour}); n != 1 {
				t.Errorf("by deployment and age: %d", n)
			}
			if n, _ := st.Invalidate(ctx, "ns2", Filter{OlderThan: time.Hour}); n != 0 {
				t.Errorf("age filter removed a fresh entry: %d", n)
			}
			if n, _ := st.Invalidate(ctx, "", Filter{}); n < 1 {
				t.Errorf("all namespaces: %d", n)
			}
			if e, _ := st.Get(ctx, "ns2", "c"); e != nil {
				t.Error("entry survived invalidate-all")
			}
			// Bounded size, oldest first (memory bounds the whole process cache, Redis
			// each namespace; either way the bound holds).
			for i, k := range []string{"x1", "x2", "x3"} {
				put("bounded", k, "p", "m", time.Now().Add(time.Duration(i)*time.Second), time.Hour, 2)
			}
			if e, _ := st.Get(ctx, "bounded", "x1"); e != nil {
				t.Error("not bounded: oldest entry kept")
			}
			if e, _ := st.Get(ctx, "bounded", "x3"); e == nil {
				t.Error("newest entry evicted")
			}
		})
	}
}

// Covers: CCH-02
func TestCCH02_CosineEdgeCases(t *testing.T) {
	if Cosine(nil, nil) != 0 || Cosine([]float32{1}, []float32{1, 2}) != 0 || Cosine([]float32{0, 0}, []float32{1, 1}) != 0 {
		t.Fatal("degenerate vectors must score 0")
	}
	if c := Cosine([]float32{1, 0}, []float32{2, 0}); c < 0.999 {
		t.Fatalf("parallel vectors: %v", c)
	}
}

// The in-process store bounds semantic buckets (one per distinct conversation prefix),
// so a long-running single instance doesn't grow without limit; buckets also expire.
// Covers: CCH-08, NFR-05
func TestCCH08_MemoryBucketsAreBounded(t *testing.T) {
	m := NewMemory()
	m.MaxBuckets = 100
	ctx := context.Background()
	for i := 0; i < 1000; i++ {
		_ = m.PutVector(ctx, "ns", fmt.Sprintf("conv-%d", i), Candidate{Key: "k", Vec: []float32{1}}, 10, time.Hour)
	}
	if n := len(m.vectors); n > 100 {
		t.Fatalf("%d buckets kept, bound is 100", n)
	}
	if c, _ := m.Candidates(ctx, "ns", "conv-999"); len(c) != 1 {
		t.Fatal("most recent bucket evicted")
	}
	_ = m.PutVector(ctx, "ns", "short", Candidate{Key: "k", Vec: []float32{1}}, 10, time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if c, _ := m.Candidates(ctx, "ns", "short"); len(c) != 0 {
		t.Fatal("expired bucket returned")
	}
}
