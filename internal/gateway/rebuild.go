package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/catalog"
	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/governance"
	"github.com/SureshKhemka/llm-gateway/internal/store"
)

// Stored object kinds created through the admin API.
const (
	DocTenant        = "tenant"
	DocProject       = "project"
	DocBudget        = "budget"
	DocGrant         = "budget_grant"
	DocKeyRevocation = "key_revocation"
	DocProvider      = "provider"
	DocDeployment    = "deployment"
)

// ProjectDoc is a project created through the admin API under a tenant.
type ProjectDoc struct {
	Tenant  string         `json:"tenant"`
	Project config.Project `json:"project"`
}

// KeyRevocation revokes a file-defined bootstrap key.
type KeyRevocation struct {
	KeyID string    `json:"key_id"`
	At    time.Time `json:"at"`
}

var rebuildMu sync.Mutex

// MergeStored overlays admin-API objects on the file configuration. Files win: a
// stored object whose ID also exists in a file is ignored [CFG-01].
func MergeStored(ctx context.Context, st store.Store, file *config.Config) (*config.Config, []governance.Grant, map[string]time.Time, error) {
	cfg := cloneConfig(file)
	tenantIdx := map[string]int{}
	for i, t := range cfg.Tenants {
		tenantIdx[t.ID] = i
	}
	docs, err := st.ListDocs(ctx, DocTenant, "")
	if err != nil {
		return nil, nil, nil, err
	}
	for _, d := range docs {
		var t config.Tenant
		if json.Unmarshal(d.Body, &t) != nil {
			continue
		}
		if _, exists := tenantIdx[t.ID]; exists {
			continue
		}
		tenantIdx[t.ID] = len(cfg.Tenants)
		cfg.Tenants = append(cfg.Tenants, t)
	}
	projects := map[string]bool{}
	for _, t := range cfg.Tenants {
		for _, p := range t.Projects {
			projects[p.ID] = true
		}
	}
	if docs, err = st.ListDocs(ctx, DocProject, ""); err != nil {
		return nil, nil, nil, err
	}
	for _, d := range docs {
		var pd ProjectDoc
		if json.Unmarshal(d.Body, &pd) != nil || projects[pd.Project.ID] {
			continue
		}
		if i, ok := tenantIdx[pd.Tenant]; ok {
			cfg.Tenants[i].Projects = append(cfg.Tenants[i].Projects, pd.Project)
			projects[pd.Project.ID] = true
		}
	}
	provIDs := map[string]bool{}
	for _, p := range cfg.Providers {
		provIDs[p.ID] = true
	}
	if docs, err = st.ListDocs(ctx, DocProvider, ""); err != nil {
		return nil, nil, nil, err
	}
	for _, d := range docs {
		var p config.Provider
		if json.Unmarshal(d.Body, &p) != nil || provIDs[p.ID] {
			continue
		}
		provIDs[p.ID] = true
		cfg.Providers = append(cfg.Providers, p)
	}
	depIDs := map[string]bool{}
	for _, dp := range cfg.Deployments {
		depIDs[dp.ID] = true
	}
	if docs, err = st.ListDocs(ctx, DocDeployment, ""); err != nil {
		return nil, nil, nil, err
	}
	for _, d := range docs {
		var dp config.Deployment
		if json.Unmarshal(d.Body, &dp) != nil || depIDs[dp.ID] {
			continue
		}
		depIDs[dp.ID] = true
		cfg.Deployments = append(cfg.Deployments, dp)
	}
	budgetIDs := map[string]bool{}
	for _, b := range cfg.Budgets {
		budgetIDs[b.ID] = true
	}
	if docs, err = st.ListDocs(ctx, DocBudget, ""); err != nil {
		return nil, nil, nil, err
	}
	for _, d := range docs {
		var b config.Budget
		if json.Unmarshal(d.Body, &b) != nil || budgetIDs[b.ID] {
			continue
		}
		budgetIDs[b.ID] = true
		cfg.Budgets = append(cfg.Budgets, b)
	}
	var grants []governance.Grant
	if docs, err = st.ListDocs(ctx, DocGrant, ""); err != nil {
		return nil, nil, nil, err
	}
	for _, d := range docs {
		var g governance.Grant
		if json.Unmarshal(d.Body, &g) == nil && time.Now().Before(g.ExpiresAt) {
			grants = append(grants, g)
		}
	}
	revoked := map[string]time.Time{}
	if docs, err = st.ListDocs(ctx, DocKeyRevocation, ""); err != nil {
		return nil, nil, nil, err
	}
	for _, d := range docs {
		var kr KeyRevocation
		if json.Unmarshal(d.Body, &kr) == nil {
			revoked[kr.KeyID] = kr.At
		}
	}
	config.ApplyDefaults(cfg)
	if err := config.Validate(cfg); err != nil {
		return nil, nil, nil, fmt.Errorf("stored objects do not validate against the configuration: %w", err)
	}
	return cfg, grants, revoked, nil
}

func cloneConfig(c *config.Config) *config.Config {
	b, _ := json.Marshal(c)
	var out config.Config
	_ = json.Unmarshal(b, &out)
	out.Files = append([]string(nil), c.Files...)
	out.CopyOrigins(c)
	return &out
}

// Rebuild builds a snapshot from the file configuration plus stored objects (keys,
// policy versions, tenants, budgets, grants, revocations, discovered models) and
// swaps it in. In-flight requests keep their snapshot [CFG-03, PRV-07].
func (e *Engine) Rebuild(ctx context.Context, file *config.Config, reason string) error {
	rebuildMu.Lock()
	defer rebuildMu.Unlock()
	if file == nil {
		file = e.Snapshot().FileConfig
	}
	cfg, grants, revoked, err := MergeStored(ctx, e.Store, file)
	if err != nil {
		return err
	}
	keys, err := e.Store.ListKeys(ctx, "")
	if err != nil {
		return err
	}
	pols, err := e.Store.ListPolicyVersions(ctx, "")
	if err != nil {
		return err
	}
	prev := e.Snapshot()
	var disc []*catalog.Deployment
	if e.Discovery != nil {
		disc = e.Discovery.List()
	}
	snap, err := BuildSnapshot(BuildInput{Config: cfg, APIKeys: keys, PolicyVers: pols, Discovered: disc, Grants: grants,
		Factory: e.Factory, PrevVersion: prev.Version, Reuse: prev.Providers})
	if err != nil {
		return err
	}
	snap.FileConfig = file
	for id, at := range revoked {
		if k := snap.Keys.ByID(id); k != nil {
			at := at
			k.Record.RevokedAt = &at
		}
	}
	e.Swap(snap)
	e.Log.Info("snapshot swapped", "version", snap.Version, "reason", reason, "deployments", len(snap.Catalog.Deployments), "keys", snap.Keys.Len())
	return nil
}
