package gateway

import (
	"context"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/catalog"
	"github.com/SureshKhemka/llm-gateway/internal/config"
)

// Discovery lists models from providers that support it [PRV-06]. Models not in the
// catalog appear as discovered-but-unregistered until approved; with auto_approve (or a
// minimal config that defines no deployments, CFG-07) they are usable immediately with
// default metadata.
type Discovery struct {
	mu       sync.Mutex
	found    map[string]*catalog.Deployment // by deployment ID
	approved map[string]bool
}

func NewDiscovery() *Discovery {
	return &Discovery{found: map[string]*catalog.Deployment{}, approved: map[string]bool{}}
}

// Run queries every discoverable provider of s and returns the discovered deployments.
func (d *Discovery) Run(ctx context.Context, s *Snapshot) []*catalog.Deployment {
	minimal := len(s.Config.Deployments) == 0
	for id, p := range s.Catalog.Providers {
		if !(p.Discover || minimal) || !p.Enabled {
			continue
		}
		ad := s.Providers.Get(id)
		if ad == nil {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		models, err := ad.ListModels(cctx)
		cancel()
		if err != nil {
			continue
		}
		for _, m := range models {
			depID := id + "/" + m
			if s.Catalog.Lookup(depID) != nil && !s.Catalog.Deployments[depID].Discovered {
				continue // already registered by config
			}
			dep := discoveredDeployment(p, m)
			d.mu.Lock()
			dep.Approved = p.AutoApprove || minimal || d.approved[depID]
			d.found[depID] = dep
			d.mu.Unlock()
		}
	}
	return d.List()
}

// List returns all discovered deployments, sorted by ID.
func (d *Discovery) List() []*catalog.Deployment {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*catalog.Deployment, 0, len(d.found))
	for _, dep := range d.found {
		out = append(out, dep)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Approve marks a discovered deployment usable, optionally overriding its metadata.
func (d *Discovery) Approve(id string, meta *config.Deployment) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	dep, ok := d.found[id]
	if !ok {
		return false
	}
	d.approved[id] = true
	if meta != nil {
		meta.ID, meta.Provider, meta.Model = dep.ID, dep.Provider, dep.Model
		if len(meta.Prices) == 0 {
			meta.Prices = []config.PriceCard{{}}
		}
		cfg := &config.Config{Deployments: []config.Deployment{*meta}}
		config.ApplyDefaults(cfg)
		nd := catalog.FromConfig(cfg.Deployments[0], nil)
		nd.ProviderType, nd.Discovered = dep.ProviderType, true
		dep = nd
		d.found[id] = dep
	}
	dep.Approved = true
	return true
}

func discoveredDeployment(p *catalog.Provider, model string) *catalog.Deployment {
	var base config.Deployment
	if p.Defaults != nil {
		base = *p.Defaults
	}
	base.ID, base.Provider, base.Model = p.ID+"/"+model, p.ID, model
	lower := strings.ToLower(model)
	if len(base.Capabilities) == 0 {
		switch {
		case strings.Contains(lower, "embed"):
			base.Capabilities = []string{"embeddings"}
		case strings.Contains(lower, "vl") || strings.Contains(lower, "vision"):
			base.Capabilities = []string{"chat", "vision"}
		default:
			base.Capabilities = []string{"chat", "tools", "json_schema"}
		}
	}
	if len(base.Prices) == 0 {
		base.Prices = []config.PriceCard{{}}
	}
	if len(base.Residency) == 0 && isLocalURL(p.BaseURL) {
		base.Residency = []string{"local"}
	}
	if base.Tier == 0 && p.Defaults == nil {
		base.Tier = 1
	}
	cfg := &config.Config{Deployments: []config.Deployment{base}}
	config.ApplyDefaults(cfg)
	dep := catalog.FromConfig(cfg.Deployments[0], p)
	dep.Discovered = true
	return dep
}

func isLocalURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" || h == "host.docker.internal" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}
