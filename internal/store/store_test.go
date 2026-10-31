package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// stores returns the Store implementations under test: memory always, Postgres when
// GATEWAY_TEST_PG_DSN is set (scripts/pg-test.sh starts a throwaway cluster).
func stores(t *testing.T) map[string]Store {
	out := map[string]Store{"memory": NewMemory()}
	if dsn := os.Getenv("GATEWAY_TEST_PG_DSN"); dsn != "" {
		ctx := context.Background()
		pg, err := OpenPostgres(ctx, dsn)
		if err != nil {
			t.Fatalf("postgres: %v", err)
		}
		for _, tbl := range []string{"cost_records", "cost_rollups_hourly", "decision_records", "keys", "policy_versions", "docs"} {
			if _, err := pg.pool.Exec(ctx, "TRUNCATE "+tbl); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { pg.Close() })
		out["postgres"] = pg
	}
	return out
}

func sampleRecords(n int) []CostRecord {
	r := rand.New(rand.NewPCG(1, 2))
	tenants := []string{"a", "b", "c"}
	models := []string{"m0", "m1", "m2"}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	recs := make([]CostRecord, n)
	for i := range recs {
		c := float64(r.IntN(100000)) / 1e9
		cache := []string{"miss", "miss", "exact_hit", "semantic_hit", "bypass"}[r.IntN(5)]
		recs[i] = CostRecord{RequestID: fmt.Sprintf("req-%05d", i), Time: base.Add(time.Duration(i) * 37 * time.Second),
			Tenant: tenants[i%3], Project: "p" + tenants[i%3], KeyID: "k" + tenants[i%3], Deployment: models[r.IntN(3)], Provider: "sim",
			Policy: "default", CacheStatus: cache, InputTokens: r.IntN(500), OutputTokens: r.IntN(200), CostUSD: c, BaselineUSD: c * 3,
			CacheSavedUSD: map[bool]float64{true: c * 2, false: 0}[cache == "exact_hit" || cache == "semantic_hit"],
			Status:        []int{200, 200, 200, 402}[r.IntN(4)], Tags: []string{"t" + fmt.Sprint(i%2)}}
	}
	return recs
}

// Covers: CST-04, CST-05
func TestCST05_AggregatesReconcileWithRecords(t *testing.T) {
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			recs := sampleRecords(900)
			if err := st.InsertCostRecords(ctx, recs); err != nil {
				t.Fatal(err)
			}
			// Re-inserting is idempotent.
			if err := st.InsertCostRecords(ctx, recs[:10]); err != nil {
				t.Fatal(err)
			}
			var total, baseline float64
			var tokens int64
			for _, r := range recs {
				total += r.CostUSD
				baseline += r.BaselineUSD
				tokens += int64(r.InputTokens)
			}
			for _, q := range []CostQuery{
				{GroupBy: []string{"tenant"}},
				{GroupBy: []string{"tenant", "model"}},
				{GroupBy: []string{"tag"}},
				{GroupBy: []string{"provider"}, Bucket: time.Hour},
				{},
			} {
				rows, err := st.QueryCost(ctx, q)
				if err != nil {
					t.Fatal(err)
				}
				var sum, bsum float64
				var n, tok int64
				for _, r := range rows {
					sum += r.CostUSD
					bsum += r.BaselineUSD
					n += r.Requests
					tok += r.InputTokens
				}
				if n != int64(len(recs)) || math.Abs(sum-total) > 1e-9 || math.Abs(bsum-baseline) > 1e-9 || tok != tokens {
					t.Errorf("group %v bucket %v: n=%d sum=%.12f (want %d %.12f)", q.GroupBy, q.Bucket, n, sum, len(recs), total)
				}
			}
			// Filtered aggregate equals the filtered rows.
			rows, _ := st.QueryCost(ctx, CostQuery{Filter: CostFilter{Tenant: "b"}, GroupBy: []string{"tenant"}})
			var want float64
			for _, r := range recs {
				if r.Tenant == "b" {
					want += r.CostUSD
				}
			}
			if len(rows) != 1 || math.Abs(rows[0].CostUSD-want) > 1e-9 {
				t.Fatalf("tenant b: %+v want %.12f", rows, want)
			}
		})
	}
}

// Covers: OBS-06
func TestOBS06_AuditLogAppendOnly(t *testing.T) {
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			before, _ := json.Marshal(map[string]any{"hard_usd": 10})
			after, _ := json.Marshal(map[string]any{"hard_usd": 20})
			id, err := st.AppendAudit(ctx, AuditEntry{Actor: "admin-1", Role: "org_admin", Tenant: "a", Action: "update",
				ObjectKind: "budget", ObjectID: "b1", Before: before, After: after, Reason: "quarter end"})
			if err != nil || id == 0 {
				t.Fatalf("append: %v", err)
			}
			list, _ := st.ListAudit(ctx, AuditFilter{ObjectKind: "budget", Limit: 5})
			if len(list) == 0 || list[0].Actor != "admin-1" || string(list[0].After) == "" || list[0].Reason != "quarter end" {
				t.Fatalf("audit entry: %+v", list)
			}
			if pg, ok := st.(*Postgres); ok {
				if _, err := pg.pool.Exec(ctx, `UPDATE audit_log SET actor='x' WHERE id=$1`, id); err == nil {
					t.Fatal("audit_log update succeeded")
				}
				if _, err := pg.pool.Exec(ctx, `DELETE FROM audit_log WHERE id=$1`, id); err == nil {
					t.Fatal("audit_log delete succeeded")
				}
			}
		})
	}
}

// Covers: CST-06
func TestCST06_RollupKeepsTotalsAndPrunesRaw(t *testing.T) {
	st := stores(t)["postgres"]
	if st == nil {
		t.Skip("set GATEWAY_TEST_PG_DSN (scripts/pg-test.sh) to run Postgres tests")
	}
	pg := st.(*Postgres)
	ctx := context.Background()
	old := sampleRecords(200)
	for i := range old {
		old[i].RequestID = "old-" + old[i].RequestID
		old[i].Time = time.Now().AddDate(-2, 0, 0).Add(time.Duration(i) * time.Minute)
	}
	if err := pg.InsertCostRecords(ctx, old); err != nil {
		t.Fatal(err)
	}
	var want float64
	for _, r := range old {
		want += r.CostUSD
	}
	n, err := pg.Rollup(ctx, 395*24*time.Hour)
	if err != nil || n != 200 {
		t.Fatalf("rollup: %d %v", n, err)
	}
	var got float64
	_ = pg.pool.QueryRow(ctx, `SELECT coalesce(sum(cost_usd),0)::float8 FROM cost_rollups_hourly`).Scan(&got)
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("rollup total %.9f want %.9f", got, want)
	}
}

// Covers: NFR-18
func TestNFR18_MigrationsAreReversible(t *testing.T) {
	st := stores(t)["postgres"]
	if st == nil {
		t.Skip("set GATEWAY_TEST_PG_DSN (scripts/pg-test.sh) to run Postgres tests")
	}
	pg := st.(*Postgres)
	ctx := context.Background()
	if err := pg.MigrateDown(ctx); err != nil {
		t.Fatal(err)
	}
	var exists bool
	_ = pg.pool.QueryRow(ctx, `SELECT to_regclass('cost_records') IS NOT NULL`).Scan(&exists)
	if exists {
		t.Fatal("down migration left tables behind")
	}
	if err := pg.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_ = pg.pool.QueryRow(ctx, `SELECT to_regclass('cost_records') IS NOT NULL`).Scan(&exists)
	if !exists {
		t.Fatal("up migration did not recreate tables")
	}
}

// Covers: NFR-07
func TestNFR07_WriterSpillsToWALAndReplays(t *testing.T) {
	st := &flaky{Memory: NewMemory()}
	st.fail.Store(true)
	dir := t.TempDir()
	w := NewWriter(st, dir, 20*time.Millisecond, nil)
	recs := sampleRecords(50)
	for _, r := range recs {
		w.Cost(r)
	}
	w.Flush()
	if got, _ := st.Memory.ListCostRecords(context.Background(), CostFilter{IncludeShadow: true}, 0); len(got) != 0 {
		t.Fatal("records reached a failing store")
	}
	if _, err := os.Stat(dir + "/records.wal"); err != nil {
		t.Fatalf("no WAL spill: %v", err)
	}
	st.fail.Store(false)
	w.Cost(sampleRecords(51)[50])
	w.Flush()
	time.Sleep(60 * time.Millisecond)
	w.Close()
	got, _ := st.Memory.ListCostRecords(context.Background(), CostFilter{IncludeShadow: true}, 0)
	if len(got) != 51 {
		t.Fatalf("after recovery %d records, want 51", len(got))
	}
}

type flaky struct {
	*Memory
	fail atomic.Bool
}

func (f *flaky) InsertCostRecords(ctx context.Context, recs []CostRecord) error {
	if f.fail.Load() {
		return fmt.Errorf("database down")
	}
	return f.Memory.InsertCostRecords(ctx, recs)
}

// The single-node memory store keeps bounded history so a long-running instance does
// not grow without limit (the soak test found this); the newest records stay queryable.
// Covers: NFR-05
func TestNFR05_MemoryStoreIsBounded(t *testing.T) {
	m := NewMemory()
	m.MaxCostRecords, m.MaxDecisions = 1000, 500
	ctx := context.Background()
	recs := sampleRecords(5000)
	for i := 0; i < len(recs); i += 100 {
		if err := m.InsertCostRecords(ctx, recs[i:i+100]); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3000; i++ {
		_ = m.PutDecision(ctx, DecisionRecord{RequestID: fmt.Sprintf("d%d", i)})
	}
	if len(m.costs) > 1000 || len(m.decisions) > 500 || len(m.decisionOrder) != len(m.decisions) {
		t.Fatalf("kept %d costs, %d decisions (%d ordered)", len(m.costs), len(m.decisions), len(m.decisionOrder))
	}
	last := recs[len(recs)-1]
	if i, ok := m.costIndex[last.RequestID]; !ok || m.costs[i].RequestID != last.RequestID {
		t.Fatal("newest cost record lost or index stale")
	}
	// Upserts still hit the right row after trimming.
	last.CostUSD = 42
	_ = m.InsertCostRecords(ctx, []CostRecord{last})
	if m.costs[m.costIndex[last.RequestID]].CostUSD != 42 {
		t.Fatal("upsert after trim updated the wrong row")
	}
	if _, err := m.GetDecision(ctx, "d2999"); err != nil {
		t.Fatal("newest decision lost")
	}
	if _, err := m.GetDecision(ctx, "d0"); err == nil {
		t.Fatal("oldest decision kept beyond the bound")
	}
}
