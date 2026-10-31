// Package gateway is the request engine shared by every client format: it resolves the
// caller, plans the route, executes attempts against providers and records cost.
package gateway

import (
	"fmt"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/catalog"
	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/governance"
	"github.com/SureshKhemka/llm-gateway/internal/provider"
	"github.com/SureshKhemka/llm-gateway/internal/store"
)

// Snapshot is an immutable view of configuration and derived state. Reloads build a new
// snapshot and swap it atomically; in-flight requests keep theirs [PRV-07, CFG-03].
type Snapshot struct {
	Version   int64
	Loaded    time.Time
	Config    *config.Config
	Catalog   *catalog.Catalog
	Policies  map[string]*config.Policy // active version per policy name
	AllPolicy map[string][]config.Policy
	Keys      *governance.KeyIndex
	Providers *provider.Registry
	Tenants   map[string]*config.Tenant
	Projects  map[string]*config.Project
}

// ProviderFactory builds adapters; tests swap it to point at in-process simulators.
type ProviderFactory func(p *catalog.Provider) (provider.Provider, error)

// DefaultProviderFactory builds real HTTP adapters.
func DefaultProviderFactory(p *catalog.Provider) (provider.Provider, error) {
	key, err := p.APIKey.Resolve()
	if err != nil {
		return nil, fmt.Errorf("provider %s api_key: %w", p.ID, err)
	}
	switch p.Type {
	case "openai":
		return provider.NewOpenAI(p.ID, p.BaseURL, key, p.Headers, p.Timeouts.Connect), nil
	case "anthropic":
		return provider.NewAnthropic(p.ID, p.BaseURL, key, p.Headers, p.Timeouts.Connect), nil
	}
	return nil, fmt.Errorf("provider %s: unknown type %q", p.ID, p.Type)
}

// BuildInput is everything a snapshot is derived from.
type BuildInput struct {
	Config      *config.Config
	APIKeys     []store.KeyRecord
	PolicyVers  []store.PolicyVersion
	Discovered  []*catalog.Deployment
	Factory     ProviderFactory
	PrevVersion int64
	// Reuse lets a reload keep existing provider adapters (and their connection pools).
	Reuse *provider.Registry
}

func BuildSnapshot(in BuildInput) (*Snapshot, error) {
	cfg := in.Config
	cat := catalog.Build(cfg)
	for _, d := range in.Discovered {
		if _, exists := cat.Deployments[d.ID]; !exists {
			cat.Add(d)
		}
	}
	factory := in.Factory
	if factory == nil {
		factory = DefaultProviderFactory
	}
	reg := provider.NewRegistry()
	for id, p := range cat.Providers {
		if in.Reuse != nil {
			if existing := in.Reuse.Get(id); existing != nil && in.Reuse.SameSpec(id, p) {
				reg.SetWithSpec(existing, p)
				continue
			}
		}
		adapter, err := factory(p)
		if err != nil {
			return nil, err
		}
		reg.SetWithSpec(adapter, p)
	}
	keys, err := governance.BuildKeyIndex(cfg, in.APIKeys)
	if err != nil {
		return nil, err
	}
	s := &Snapshot{
		Version: in.PrevVersion + 1, Loaded: time.Now(), Config: cfg, Catalog: cat,
		Keys: keys, Providers: reg, Tenants: map[string]*config.Tenant{}, Projects: map[string]*config.Project{},
	}
	for i := range cfg.Tenants {
		t := &cfg.Tenants[i]
		s.Tenants[t.ID] = t
		for j := range t.Projects {
			s.Projects[t.Projects[j].ID] = &t.Projects[j]
		}
	}
	s.Policies, s.AllPolicy = resolvePolicies(cfg, in.PolicyVers)
	return s, nil
}

// resolvePolicies picks the active version of each policy. Versions come from config
// files and from the policy store (admin API / promotions); the store's Active flag wins,
// otherwise the highest version is active.
func resolvePolicies(cfg *config.Config, stored []store.PolicyVersion) (map[string]*config.Policy, map[string][]config.Policy) {
	all := map[string][]config.Policy{}
	for _, p := range config.BuiltinPolicies() {
		all[p.Name] = append(all[p.Name], p)
	}
	fileNames := map[string]bool{}
	for _, p := range cfg.Policies {
		if !fileNames[p.Name] {
			// A file definition replaces the built-in of the same name.
			all[p.Name] = nil
			fileNames[p.Name] = true
		}
		all[p.Name] = append(all[p.Name], p)
	}
	activeStored := map[string]int{}
	for _, sv := range stored {
		p, err := DecodePolicy(sv.Body)
		if err != nil {
			continue
		}
		p.Name, p.Version = sv.Name, sv.Version
		replaced := false
		for i := range all[sv.Name] {
			if all[sv.Name][i].Version == sv.Version {
				all[sv.Name][i] = *p
				replaced = true
			}
		}
		if !replaced {
			all[sv.Name] = append(all[sv.Name], *p)
		}
		if sv.Active {
			activeStored[sv.Name] = sv.Version
		}
	}
	active := map[string]*config.Policy{}
	for name, vs := range all {
		want, pinned := activeStored[name]
		var pick *config.Policy
		for i := range vs {
			v := &vs[i]
			if pinned {
				if v.Version == want {
					pick = v
				}
			} else if pick == nil || v.Version > pick.Version {
				pick = v
			}
		}
		if pick != nil {
			active[name] = pick
		}
	}
	return active, all
}

// Policy returns the active policy by name, falling back to the default policy.
func (s *Snapshot) Policy(name string) *config.Policy {
	if p, ok := s.Policies[name]; ok {
		return p
	}
	return s.Policies[s.Config.Defaults.Policy]
}
