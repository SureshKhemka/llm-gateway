// Command workload generates the labelled corpus and replays it against a gateway
// [ENV-05].
//
//	workload gen -out testdata/corpus/corpus.jsonl
//	workload replay -url http://localhost:8080 -model auto -key $KEY -out results.jsonl
//	workload compare -a auto.json -b baseline.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/SureshKhemka/llm-gateway/internal/workload"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: workload gen|replay|compare [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "gen":
		fs := flag.NewFlagSet("gen", flag.ExitOnError)
		out := fs.String("out", "testdata/corpus/corpus.jsonl", "output JSONL")
		seed := fs.Uint64("seed", 42, "seed")
		fs.Parse(os.Args[2:])
		items := workload.Generate(*seed)
		if err := workload.Save(*out, items); err != nil {
			fail(err)
		}
		cats := map[string]int{}
		for _, it := range items {
			cats[it.Category]++
		}
		fmt.Printf("wrote %d items to %s: %v\n", len(items), *out, cats)
	case "replay":
		fs := flag.NewFlagSet("replay", flag.ExitOnError)
		url := fs.String("url", "http://localhost:8080", "gateway base URL")
		model := fs.String("model", "auto", "model or virtual model")
		keys := fs.String("key", os.Getenv("GATEWAY_KEY"), "comma-separated tenant keys (key or key:weight)")
		corpus := fs.String("corpus", "testdata/corpus/corpus.jsonl", "corpus JSONL")
		rps := fs.Float64("rps", 0, "requests per second (0 = unthrottled)")
		conc := fs.Int("concurrency", 8, "concurrent requests")
		repeat := fs.Float64("repeat", 0.2, "share of exact repeats")
		out := fs.String("out", "", "write per-request results JSONL")
		summary := fs.String("summary", "", "write summary JSON")
		limit := fs.Int("limit", 0, "replay only the first N items")
		fs.Parse(os.Args[2:])
		items, err := workload.Load(*corpus)
		if err != nil {
			fail(err)
		}
		if *limit > 0 && *limit < len(items) {
			items = items[:*limit]
		}
		var tenants []workload.Tenant
		for _, k := range strings.Split(*keys, ",") {
			if k = strings.TrimSpace(k); k == "" {
				continue
			}
			key, w := k, 1
			if i := strings.LastIndex(k, ":"); i > 0 {
				fmt.Sscan(k[i+1:], &w)
				key = k[:i]
			}
			tenants = append(tenants, workload.Tenant{Key: key, Weight: w})
		}
		results, sum, err := workload.Replay(context.Background(), items, workload.ReplayOptions{
			BaseURL: strings.TrimRight(*url, "/"), Model: *model, Tenants: tenants, RPS: *rps, Concurrency: *conc, RepeatRate: *repeat, Seed: 1})
		if err != nil {
			fail(err)
		}
		if *out != "" {
			f, _ := os.Create(*out)
			enc := json.NewEncoder(f)
			for _, r := range results {
				enc.Encode(r)
			}
			f.Close()
		}
		b, _ := json.MarshalIndent(sum, "", "  ")
		if *summary != "" {
			os.WriteFile(*summary, b, 0o644)
		}
		fmt.Println(string(b))
	case "compare":
		fs := flag.NewFlagSet("compare", flag.ExitOnError)
		a := fs.String("a", "", "candidate summary JSON")
		b := fs.String("b", "", "baseline summary JSON")
		fs.Parse(os.Args[2:])
		var sa, sb workload.Summary
		readJSON(*a, &sa)
		readJSON(*b, &sb)
		fmt.Printf("cost: %.4f vs %.4f USD (%.1f%% lower)\nquality: %.2f vs %.2f (%.2f points)\n",
			sa.CostUSD, sb.CostUSD, 100*(1-sa.CostUSD/sb.CostUSD), sa.Quality, sb.Quality, sa.Quality-sb.Quality)
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
}

func readJSON(path string, v any) {
	b, err := os.ReadFile(path)
	if err != nil {
		fail(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
