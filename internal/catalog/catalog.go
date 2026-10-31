// Package catalog is the immutable runtime view of providers, deployments, price cards
// and virtual models [PRV-03, PRV-04, PRV-05].
package catalog

import (
	"sort"
	"strings"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/config"
)

type Capability string

const (
	CapChat       Capability = "chat"
	CapTools      Capability = "tools"
	CapVision     Capability = "vision"
	CapJSONSchema Capability = "json_schema"
	CapEmbeddings Capability = "embeddings"
	CapReasoning  Capability = "reasoning"
)

// PriceCard holds USD per 1M tokens, effective from a date.
type PriceCard struct {
	EffectiveFrom time.Time `json:"effective_from"`
	Input         float64   `json:"input"`
	CachedInput   float64   `json:"cached_input"`
	Output        float64   `json:"output"`
	Reasoning     float64   `json:"reasoning"`
}

type Timeouts struct {
	Connect, TTFT, Idle, Total time.Duration
}

type Deployment struct {
	ID              string              `json:"id"`
	Provider        string              `json:"provider"`
	ProviderType    string              `json:"provider_type"`
	Model           string              `json:"model"`
	Capabilities    map[Capability]bool `json:"-"`
	CapList         []string            `json:"capabilities"`
	ContextWindow   int                 `json:"context_window"`
	MaxOutputTokens int                 `json:"max_output_tokens"`
	Tier            int                 `json:"tier"`
	Prices          []PriceCard         `json:"prices"`
	TTFTMs          float64             `json:"ttft_ms"`
	TokensPerSec    float64             `json:"tokens_per_sec"`
	Concurrency     int                 `json:"concurrency"`
	QueueWait       time.Duration       `json:"queue_wait_ns"`
	Residency       []string            `json:"residency"`
	Enabled         bool                `json:"enabled"`
	Pool            string              `json:"pool,omitempty"`
	Weight          int                 `json:"weight"`
	Timeouts        Timeouts            `json:"-"`
	Warm            bool                `json:"warm"`
	Discovered      bool                `json:"discovered,omitempty"`
	Approved        bool                `json:"approved"`
}

func (d *Deployment) Has(c Capability) bool { return d.Capabilities[c] }

func (d *Deployment) HasResidency(tag string) bool {
	for _, r := range d.Residency {
		if r == tag {
			return true
		}
	}
	return false
}

// IsLocal reports whether the deployment carries the "local" residency tag.
func (d *Deployment) IsLocal() bool { return d.HasResidency("local") }

// PriceAt returns the price card in effect at t [PRV-05]. Before the first card's
// effective date the first card applies.
func (d *Deployment) PriceAt(t time.Time) PriceCard {
	if len(d.Prices) == 0 {
		return PriceCard{}
	}
	idx := sort.Search(len(d.Prices), func(i int) bool { return d.Prices[i].EffectiveFrom.After(t) })
	if idx == 0 {
		return d.Prices[0]
	}
	return d.Prices[idx-1]
}

type Provider struct {
	ID          string
	Type        string
	BaseURL     string
	APIKey      config.SecretRef
	Headers     map[string]string
	Discover    bool
	AutoApprove bool
	Enabled     bool
	Timeouts    Timeouts
	ProbeEvery  time.Duration
	Defaults    *config.Deployment
}

type VirtualModel struct {
	Name        string
	Policy      string
	Deployments []string
	Description string
}

// Catalog is immutable once built; reloads build a new one.
type Catalog struct {
	Providers     map[string]*Provider
	Deployments   map[string]*Deployment
	Order         []string // deployment IDs in config order
	VirtualModels map[string]*VirtualModel
}

func toTimeouts(t config.Timeouts) Timeouts {
	return Timeouts{Connect: t.Connect.D(), TTFT: t.TTFT.D(), Idle: t.Idle.D(), Total: t.Total.D()}
}

// Build constructs a catalog from validated configuration.
func Build(cfg *config.Config) *Catalog {
	c := &Catalog{
		Providers:     map[string]*Provider{},
		Deployments:   map[string]*Deployment{},
		VirtualModels: map[string]*VirtualModel{},
	}
	for _, p := range cfg.Providers {
		c.Providers[p.ID] = &Provider{
			ID: p.ID, Type: p.Type, BaseURL: strings.TrimRight(p.BaseURL, "/"), APIKey: p.APIKey,
			Headers: p.Headers, Discover: p.Discover, AutoApprove: p.AutoApprove,
			Enabled: p.Enabled == nil || *p.Enabled, Timeouts: toTimeouts(p.Timeouts),
			ProbeEvery: p.ProbeEvery.D(), Defaults: p.DefaultDiscovered,
		}
	}
	for _, d := range cfg.Deployments {
		dep := FromConfig(d, c.Providers[d.Provider])
		dep.Approved = true
		c.Add(dep)
	}
	for _, vm := range cfg.VirtualModels {
		c.VirtualModels[vm.Name] = &VirtualModel{Name: vm.Name, Policy: vm.Policy, Deployments: vm.Deployments, Description: vm.Description}
	}
	// Built-in virtual models map to built-in policies [API-07]; "auto" has none of its
	// own, so scope policies and the global default apply to it.
	for name, policy := range map[string]string{"auto": "", "auto-fast": "auto-fast", "auto-cheap": "auto-cheap"} {
		if _, ok := c.VirtualModels[name]; !ok {
			c.VirtualModels[name] = &VirtualModel{Name: name, Policy: policy}
		}
	}
	return c
}

// FromConfig turns one deployment definition into its runtime form.
func FromConfig(d config.Deployment, p *Provider) *Deployment {
	dep := &Deployment{
		ID: d.ID, Provider: d.Provider, Model: d.Model,
		Capabilities: map[Capability]bool{}, CapList: append([]string(nil), d.Capabilities...),
		ContextWindow: d.ContextWindow, MaxOutputTokens: d.MaxOutputTokens, Tier: d.Tier,
		TTFTMs: d.Latency.TTFTMs, TokensPerSec: d.Latency.TokensPerSec,
		Concurrency: d.Concurrency, QueueWait: d.QueueWait.D(), Residency: d.Residency,
		Enabled: d.Enabled == nil || *d.Enabled, Pool: d.Pool, Weight: d.Weight,
		Timeouts: toTimeouts(d.Timeouts), Warm: d.Warm == nil || *d.Warm,
	}
	if p != nil {
		dep.ProviderType = p.Type
		if !p.Enabled {
			dep.Enabled = false
		}
	}
	for _, cp := range d.Capabilities {
		dep.Capabilities[Capability(cp)] = true
	}
	for _, pc := range d.Prices {
		card := PriceCard{EffectiveFrom: pc.EffectiveFrom, Input: pc.Input, Output: pc.Output, CachedInput: pc.Input, Reasoning: pc.Output}
		if pc.CachedInput != nil {
			card.CachedInput = *pc.CachedInput
		}
		if pc.Reasoning != nil {
			card.Reasoning = *pc.Reasoning
		}
		dep.Prices = append(dep.Prices, card)
	}
	sort.Slice(dep.Prices, func(i, j int) bool { return dep.Prices[i].EffectiveFrom.Before(dep.Prices[j].EffectiveFrom) })
	if dep.TTFTMs == 0 {
		dep.TTFTMs = 500
	}
	if dep.TokensPerSec == 0 {
		dep.TokensPerSec = 50
	}
	return dep
}

// Add inserts or replaces a deployment (used while building and for discovery).
func (c *Catalog) Add(d *Deployment) {
	if _, exists := c.Deployments[d.ID]; !exists {
		c.Order = append(c.Order, d.ID)
	}
	c.Deployments[d.ID] = d
}

// Usable returns enabled, approved deployments in config order.
func (c *Catalog) Usable() []*Deployment {
	out := make([]*Deployment, 0, len(c.Order))
	for _, id := range c.Order {
		if d := c.Deployments[id]; d.Enabled && d.Approved {
			out = append(out, d)
		}
	}
	return out
}

// Lookup finds a concrete deployment by ID, or by "provider/upstream-model".
func (c *Catalog) Lookup(name string) *Deployment {
	if d, ok := c.Deployments[name]; ok {
		return d
	}
	if prov, model, ok := strings.Cut(name, "/"); ok {
		for _, id := range c.Order {
			d := c.Deployments[id]
			if d.Provider == prov && d.Model == model {
				return d
			}
		}
	}
	return nil
}
