// Package governance implements tenancy, virtual keys, admin roles, budgets, rate
// limits and alerts [TEN-*, BUD-*].
package governance

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net/netip"
	"path"
	"strings"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/config"
	"github.com/SureshKhemka/llm-gateway/internal/store"
)

// Principal is the authenticated caller of the client API.
type Principal struct {
	Tenant    *config.Tenant
	Project   *config.Project
	KeyID     string
	KeyPrefix string
	KeyName   string
	Scopes    config.Scopes
	Anonymous bool
}

func (p *Principal) TenantID() string {
	if p.Tenant == nil {
		return "default"
	}
	return p.Tenant.ID
}

func (p *Principal) ProjectID() string {
	if p.Project == nil {
		return "default"
	}
	return p.Project.ID
}

// HashKey is the stored form of a virtual key. Keys carry ≥160 bits of randomness, so a
// fast hash is non-reversible in practice and keeps auth off the latency budget [TEN-02].
func HashKey(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

// KeyPrefix is the visible, non-secret identifier of a key: everything up to and
// including the first '_' (gw-live-abcd1234_) or the first 12 characters.
func KeyPrefix(secret string) string {
	if i := strings.IndexByte(secret, '_'); i > 0 && i < 24 {
		return secret[:i+1]
	}
	if len(secret) > 12 {
		return secret[:12]
	}
	return secret
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateKey returns a new key "gw-<env>-<id>_<secret>" and its ID.
func GenerateKey(env string) (secret, id string) {
	idb := make([]byte, 5)
	sb := make([]byte, 20)
	_, _ = rand.Read(idb)
	_, _ = rand.Read(sb)
	id = strings.ToLower(b32.EncodeToString(idb))
	return fmt.Sprintf("gw-%s-%s_%s", env, id, strings.ToLower(b32.EncodeToString(sb))), "key_" + id
}

// KeyEntry is an authenticatable key in the in-memory index.
type KeyEntry struct {
	Record    store.KeyRecord
	Tenant    *config.Tenant
	Project   *config.Project
	Scopes    config.Scopes
	Bootstrap bool
}

// Valid reports whether the key may authenticate at time now.
func (k *KeyEntry) Valid(now time.Time) bool {
	r := &k.Record
	if r.RevokedAt != nil && !now.Before(*r.RevokedAt) {
		return false
	}
	if r.ExpiresAt != nil && !now.Before(*r.ExpiresAt) {
		return false
	}
	if r.OverlapUntil != nil && !now.Before(*r.OverlapUntil) {
		return false // rotated away and past the overlap window
	}
	return true
}

// KeyIndex maps key hashes to entries; it is rebuilt with every snapshot and updated
// in place on revocation.
type KeyIndex struct {
	byHash map[string]*KeyEntry
	byID   map[string]*KeyEntry
}

func NewKeyIndex() *KeyIndex {
	return &KeyIndex{byHash: map[string]*KeyEntry{}, byID: map[string]*KeyEntry{}}
}

func (ix *KeyIndex) Add(e *KeyEntry) {
	ix.byHash[string(e.Record.Hash)] = e
	ix.byID[e.Record.ID] = e
}

func (ix *KeyIndex) ByHash(h []byte) *KeyEntry { return ix.byHash[string(h)] }
func (ix *KeyIndex) ByID(id string) *KeyEntry  { return ix.byID[id] }
func (ix *KeyIndex) Len() int                  { return len(ix.byID) }

func (ix *KeyIndex) All() []*KeyEntry {
	out := make([]*KeyEntry, 0, len(ix.byID))
	for _, e := range ix.byID {
		out = append(out, e)
	}
	return out
}

// BuildKeyIndex indexes bootstrap keys from configuration plus API-created keys.
func BuildKeyIndex(cfg *config.Config, apiKeys []store.KeyRecord) (*KeyIndex, error) {
	ix := NewKeyIndex()
	tenants := map[string]*config.Tenant{}
	projects := map[string]*config.Project{}
	for ti := range cfg.Tenants {
		t := &cfg.Tenants[ti]
		tenants[t.ID] = t
		for pi := range t.Projects {
			p := &t.Projects[pi]
			projects[p.ID] = p
			for _, k := range p.Keys {
				secret, err := k.Secret.Resolve()
				if err != nil {
					return nil, fmt.Errorf("tenant %s key %s: %w", t.ID, k.ID, err)
				}
				scopes, _ := json.Marshal(k.Scopes)
				ix.Add(&KeyEntry{
					Record: store.KeyRecord{ID: k.ID, Prefix: KeyPrefix(secret), Hash: HashKey(secret), Tenant: t.ID,
						Project: p.ID, Name: k.Name, Scopes: scopes, ExpiresAt: k.ExpiresAt, Managed: "file"},
					Tenant: t, Project: p, Scopes: k.Scopes, Bootstrap: true,
				})
			}
		}
	}
	for _, r := range apiKeys {
		t, p := tenants[r.Tenant], projects[r.Project]
		if t == nil {
			continue // tenant removed: its keys stop working
		}
		var sc config.Scopes
		_ = json.Unmarshal(r.Scopes, &sc)
		ix.Add(&KeyEntry{Record: r, Tenant: t, Project: p, Scopes: sc})
	}
	return ix, nil
}

// ModelAllowed checks key scopes and tenant allow-lists (glob patterns) [TEN-03].
func (p *Principal) ModelAllowed(model string) bool {
	if !matchAny(p.Scopes.Models, model) {
		return false
	}
	if p.Tenant != nil && !matchAny(p.Tenant.AllowedModels, model) {
		return false
	}
	return true
}

// ProviderAllowed checks key provider scopes.
func (p *Principal) ProviderAllowed(provider string) bool {
	return matchAny(p.Scopes.Providers, provider)
}

// EndpointAllowed checks key endpoint scopes ("chat", "messages", "embeddings", "responses").
func (p *Principal) EndpointAllowed(ep string) bool { return matchAny(p.Scopes.Endpoints, ep) }

// IPAllowed checks the key's IP allow-list (CIDRs or addresses).
func (p *Principal) IPAllowed(ip netip.Addr) bool {
	if len(p.Scopes.IPAllow) == 0 {
		return true
	}
	for _, s := range p.Scopes.IPAllow {
		if pfx, err := netip.ParsePrefix(s); err == nil && pfx.Contains(ip) {
			return true
		}
		if a, err := netip.ParseAddr(s); err == nil && a == ip {
			return true
		}
	}
	return false
}

// matchAny: an empty pattern list allows everything.
func matchAny(patterns []string, s string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if p == "*" || p == s {
			return true
		}
		if ok, _ := path.Match(p, s); ok {
			return true
		}
	}
	return false
}
