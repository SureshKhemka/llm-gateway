package store

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is an in-process Store.
// Memory is the single-node store (no Postgres). It is not durable, and it keeps only
// the most recent MaxCostRecords cost records and MaxDecisions route decisions so a
// long-running instance has bounded memory; budgets live in the counters, not here.
type Memory struct {
	mu             sync.RWMutex
	MaxCostRecords int
	MaxDecisions   int
	costs          []CostRecord
	costIndex      map[string]int
	decisions      map[string]DecisionRecord
	decisionOrder  []string
	audit          []AuditEntry
	keys           map[string]KeyRecord
	policies       map[string][]PolicyVersion
	docs           map[string]Doc
}

func NewMemory() *Memory {
	return &Memory{
		MaxCostRecords: 200000, // ~0.5 KB each
		MaxDecisions:   20000,  // ~2.5 KB each (candidates, exclusions)
		costIndex:      map[string]int{},
		decisions:      map[string]DecisionRecord{},
		keys:           map[string]KeyRecord{},
		policies:       map[string][]PolicyVersion{},
		docs:           map[string]Doc{},
	}
}

func (m *Memory) Ping(context.Context) error { return nil }
func (m *Memory) Close() error               { return nil }

func (m *Memory) InsertCostRecords(_ context.Context, recs []CostRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range recs {
		if i, ok := m.costIndex[r.RequestID]; ok {
			m.costs[i] = r // idempotent upsert by request ID
			continue
		}
		m.costIndex[r.RequestID] = len(m.costs)
		m.costs = append(m.costs, r)
	}
	// Drop the oldest tenth once over the cap, so trimming is amortised.
	if max := m.MaxCostRecords; max > 0 && len(m.costs) > max {
		drop := len(m.costs) - max + max/10
		m.costs = append([]CostRecord(nil), m.costs[drop:]...)
		m.costIndex = make(map[string]int, len(m.costs))
		for i, r := range m.costs {
			m.costIndex[r.RequestID] = i
		}
	}
	return nil
}

// Match reports whether r passes f.
func (f CostFilter) Match(r *CostRecord) bool {
	if f.Tenant != "" && r.Tenant != f.Tenant ||
		f.Project != "" && r.Project != f.Project ||
		f.KeyID != "" && r.KeyID != f.KeyID ||
		f.EndUser != "" && r.EndUser != f.EndUser ||
		f.Deployment != "" && r.Deployment != f.Deployment ||
		f.Provider != "" && r.Provider != f.Provider ||
		f.Policy != "" && r.Policy != f.Policy ||
		f.EvalRun != "" && r.EvalRun != f.EvalRun {
		return false
	}
	if !f.IncludeShadow && r.Shadow {
		return false
	}
	if !f.From.IsZero() && r.Time.Before(f.From) || !f.To.IsZero() && !r.Time.Before(f.To) {
		return false
	}
	if f.Tag != "" {
		found := false
		for _, t := range r.Tags {
			if t == f.Tag {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// GroupValue extracts a group-by dimension from a record.
func GroupValue(r *CostRecord, dim string) []string {
	switch dim {
	case "tenant":
		return []string{r.Tenant}
	case "project":
		return []string{r.Project}
	case "key":
		return []string{r.KeyID}
	case "end_user":
		return []string{r.EndUser}
	case "model":
		return []string{r.Deployment}
	case "requested_model":
		return []string{r.RequestedModel}
	case "provider":
		return []string{r.Provider}
	case "policy":
		return []string{r.Policy}
	case "cache_status":
		return []string{r.CacheStatus}
	case "tag":
		if len(r.Tags) == 0 {
			return []string{""}
		}
		return r.Tags
	}
	return []string{""}
}

// Aggregate groups records in memory; the Postgres store uses SQL with identical
// semantics, and tests assert both reconcile with the raw rows [CST-05].
func Aggregate(recs []CostRecord, q CostQuery) []CostRow {
	rows := map[string]*CostRow{}
	var order []string
	for i := range recs {
		r := &recs[i]
		if !q.Filter.Match(r) {
			continue
		}
		var bucket time.Time
		if q.Bucket > 0 {
			bucket = r.Time.UTC().Truncate(q.Bucket)
		}
		combos := [][]string{{}}
		for _, dim := range q.GroupBy {
			var next [][]string
			for _, c := range combos {
				for _, v := range GroupValue(r, dim) {
					next = append(next, append(append([]string(nil), c...), v))
				}
			}
			combos = next
		}
		for _, c := range combos {
			key := bucket.String() + "\x00" + strings.Join(c, "\x00")
			row, ok := rows[key]
			if !ok {
				row = &CostRow{Group: map[string]string{}, Bucket: bucket}
				for i, dim := range q.GroupBy {
					row.Group[dim] = c[i]
				}
				rows[key] = row
				order = append(order, key)
			}
			row.Requests++
			row.InputTokens += int64(r.InputTokens)
			row.CachedTokens += int64(r.CachedTokens)
			row.OutputTokens += int64(r.OutputTokens)
			row.ReasoningToks += int64(r.ReasoningToks)
			row.CostUSD += r.CostUSD
			row.BaselineUSD += r.BaselineUSD
			row.RoutingSavings += r.RoutingSavings()
			row.CacheSavings += r.CacheSavedUSD
			if r.CacheStatus == "exact_hit" || r.CacheStatus == "semantic_hit" {
				row.CacheHits++
			}
			if r.Status >= 400 {
				row.Errors++
			}
		}
	}
	out := make([]CostRow, 0, len(order))
	for _, k := range order {
		out = append(out, *rows[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Bucket.Before(out[j].Bucket) })
	return out
}

func (m *Memory) QueryCost(_ context.Context, q CostQuery) ([]CostRow, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return Aggregate(m.costs, q), nil
}

func (m *Memory) ListCostRecords(_ context.Context, f CostFilter, limit int) ([]CostRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []CostRecord
	for i := len(m.costs) - 1; i >= 0; i-- {
		if f.Match(&m.costs[i]) {
			out = append(out, m.costs[i])
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *Memory) PutDecision(_ context.Context, d DecisionRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.decisions[d.RequestID]; !ok {
		m.decisionOrder = append(m.decisionOrder, d.RequestID)
	}
	m.decisions[d.RequestID] = d
	if max := m.MaxDecisions; max > 0 && len(m.decisionOrder) > max {
		drop := len(m.decisionOrder) - max + max/10
		for _, id := range m.decisionOrder[:drop] {
			delete(m.decisions, id)
		}
		m.decisionOrder = append([]string(nil), m.decisionOrder[drop:]...)
	}
	return nil
}

func (m *Memory) GetDecision(_ context.Context, id string) (*DecisionRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.decisions[id]
	if !ok {
		return nil, ErrNotFound
	}
	return &d, nil
}

func (m *Memory) AppendAudit(_ context.Context, e AuditEntry) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.ID = int64(len(m.audit) + 1)
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	m.audit = append(m.audit, e)
	return e.ID, nil
}

func (m *Memory) ListAudit(_ context.Context, f AuditFilter) ([]AuditEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []AuditEntry
	for i := len(m.audit) - 1; i >= 0; i-- {
		e := m.audit[i]
		if f.Tenant != "" && e.Tenant != f.Tenant || f.ObjectKind != "" && e.ObjectKind != f.ObjectKind || !f.Since.IsZero() && e.Time.Before(f.Since) {
			continue
		}
		out = append(out, e)
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out, nil
}

func (m *Memory) PutKey(_ context.Context, k KeyRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[k.ID] = k
	return nil
}

func (m *Memory) GetKeyByHash(_ context.Context, hash []byte) (*KeyRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, k := range m.keys {
		if bytes.Equal(k.Hash, hash) {
			k := k
			return &k, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) GetKey(_ context.Context, id string) (*KeyRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.keys[id]
	if !ok {
		return nil, ErrNotFound
	}
	return &k, nil
}

func (m *Memory) ListKeys(_ context.Context, tenant string) ([]KeyRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []KeyRecord
	for _, k := range m.keys {
		if tenant == "" || k.Tenant == tenant {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) PutPolicyVersion(_ context.Context, p PolicyVersion) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	vs := m.policies[p.Name]
	for i, v := range vs {
		if v.Version == p.Version {
			vs[i] = p
			return nil
		}
	}
	m.policies[p.Name] = append(vs, p)
	sort.Slice(m.policies[p.Name], func(i, j int) bool { return m.policies[p.Name][i].Version < m.policies[p.Name][j].Version })
	return nil
}

func (m *Memory) ListPolicyVersions(_ context.Context, name string) ([]PolicyVersion, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if name == "" {
		var all []PolicyVersion
		for _, vs := range m.policies {
			all = append(all, vs...)
		}
		return all, nil
	}
	return append([]PolicyVersion(nil), m.policies[name]...), nil
}

func (m *Memory) ActivatePolicyVersion(_ context.Context, name string, version int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	vs := m.policies[name]
	found := false
	for i := range vs {
		vs[i].Active = vs[i].Version == version
		found = found || vs[i].Active
	}
	if !found {
		return ErrNotFound
	}
	return nil
}

func docKey(kind, id string) string { return kind + "\x00" + id }

func (m *Memory) PutDoc(_ context.Context, d Doc) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d.Updated.IsZero() {
		d.Updated = time.Now().UTC()
	}
	m.docs[docKey(d.Kind, d.ID)] = d
	return nil
}

func (m *Memory) GetDoc(_ context.Context, kind, id string) (*Doc, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.docs[docKey(kind, id)]
	if !ok {
		return nil, ErrNotFound
	}
	return &d, nil
}

func (m *Memory) ListDocs(_ context.Context, kind, tenant string) ([]Doc, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Doc
	for _, d := range m.docs {
		if d.Kind == kind && (tenant == "" || d.Tenant == tenant) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Memory) DeleteDoc(_ context.Context, kind, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.docs, docKey(kind, id))
	return nil
}
