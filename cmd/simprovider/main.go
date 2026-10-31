// Command simprovider runs one or more named simulated provider instances [ENV-04].
//
//	simprovider -config sim.yaml
//	simprovider -instance a=:9001 -instance b=:9002   (standard model lineup on each)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/SureshKhemka/llm-gateway/internal/sim"
	"gopkg.in/yaml.v3"
)

type instanceCfg struct {
	Name   string      `yaml:"name"`
	Listen string      `yaml:"listen"`
	Models []sim.Model `yaml:"models"`
	Oracle string      `yaml:"oracle"` // JSONL corpus with prompt/reference/difficulty
}

type fileCfg struct {
	Instances []instanceCfg `yaml:"instances"`
}

var standard = []sim.Model{
	{ID: "gemma-e2b", Tier: 0, ContextWindow: 8192},
	{ID: "qwen-4b", Tier: 1, ContextWindow: 16384},
	{ID: "qwen-9b", Tier: 2, ContextWindow: 32768},
	{ID: "qwen-27b", Tier: 3, ContextWindow: 131072},
	{ID: "qwen-vl-8b", Tier: 2, ContextWindow: 32768},
	{ID: "nomic-embed", Tier: 0, Embeddings: true},
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	cfgPath := flag.String("config", "", "YAML file listing instances")
	oracle := flag.String("oracle", "", "JSONL corpus used as the answer oracle for every instance")
	var inst multiFlag
	flag.Var(&inst, "instance", "name=addr (repeatable), standard model lineup")
	flag.Parse()

	var cfg fileCfg
	if *cfgPath != "" {
		b, err := os.ReadFile(*cfgPath)
		if err != nil {
			log.Fatal(err)
		}
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			log.Fatal(err)
		}
	}
	for _, s := range inst {
		name, addr, ok := strings.Cut(s, "=")
		if !ok {
			log.Fatalf("bad -instance %q (want name=addr)", s)
		}
		cfg.Instances = append(cfg.Instances, instanceCfg{Name: name, Listen: addr})
	}
	if len(cfg.Instances) == 0 {
		cfg.Instances = []instanceCfg{{Name: "sima", Listen: ":9001"}, {Name: "simb", Listen: ":9002"}}
	}
	var wg sync.WaitGroup
	for _, ic := range cfg.Instances {
		models := ic.Models
		if len(models) == 0 {
			models = standard
		}
		in := sim.New(ic.Name, models...)
		for _, path := range []string{ic.Oracle, *oracle} {
			if path != "" {
				items, err := loadOracle(path)
				if err != nil {
					log.Fatalf("oracle %s: %v", path, err)
				}
				in.LoadOracle(items)
			}
		}
		wg.Add(1)
		go func(name, addr string, h http.Handler) {
			defer wg.Done()
			log.Printf("sim instance %s listening on %s", name, addr)
			log.Fatal(http.ListenAndServe(addr, h))
		}(ic.Name, ic.Listen, in.Handler())
	}
	wg.Wait()
}

// loadOracle reads corpus lines {"prompt"|"messages", "reference", "difficulty"}.
func loadOracle(path string) (map[string]sim.OracleItem, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]sim.OracleItem{}
	for i, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var it struct {
			Prompt     string `json:"prompt"`
			Reference  string `json:"reference"`
			Difficulty int    `json:"difficulty"`
		}
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		if it.Prompt != "" && it.Reference != "" {
			out[it.Prompt] = sim.OracleItem{Reference: it.Reference, Difficulty: it.Difficulty}
		}
	}
	return out, nil
}
