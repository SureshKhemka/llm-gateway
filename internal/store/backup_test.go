package store

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// scripts/backup.sh round-trips configuration, keys, policy versions, API-managed
// objects, cost records, rollups and the audit log into a fresh database, and the
// restored audit log is still append-only.
// Covers: NFR-19, OBS-06
func TestNFR19_BackupAndRestore(t *testing.T) {
	dsn := os.Getenv("GATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set GATEWAY_TEST_PG_DSN (scripts/pg-test.sh) to run Postgres tests")
	}
	ctx := context.Background()
	src, dst := tempDB(t, dsn, "gw_backup_src"), tempDB(t, dsn, "gw_backup_dst")
	a, err := OpenPostgres(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	recs := sampleRecords(500)
	if err := a.InsertCostRecords(ctx, recs); err != nil {
		t.Fatal(err)
	}
	scopes, _ := json.Marshal(map[string]any{"models": []string{"auto"}})
	key := KeyRecord{ID: "k1", Prefix: "gw-live-k1", Hash: []byte("hash-bytes"), Tenant: "a", Project: "pa", Name: "ci", Scopes: scopes, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if err := a.PutKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"objective": "min_cost"})
	if err := a.PutPolicyVersion(ctx, PolicyVersion{Name: "default", Version: 2, CreatedAt: time.Now(), Actor: "org", Body: body, Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.PutDoc(ctx, Doc{Kind: "budget", ID: "b1", Body: []byte(`{"id":"b1","hard_usd":10}`)}); err != nil {
		t.Fatal(err)
	}
	auditID, err := a.AppendAudit(ctx, AuditEntry{Actor: "org", Role: "org_admin", Action: "create", ObjectKind: "budget", ObjectID: "b1"})
	if err != nil {
		t.Fatal(err)
	}
	cfgDir := t.TempDir()
	os.WriteFile(filepath.Join(cfgDir, "gateway.yaml"), []byte("providers: []\n"), 0o644)

	out := t.TempDir()
	run := func(env []string, args ...string) {
		t.Helper()
		cmd := exec.Command("../../scripts/backup.sh", args...)
		cmd.Env = append(os.Environ(), env...)
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		if err := cmd.Run(); err != nil {
			t.Fatalf("backup.sh %v: %v\n%s", args, err, buf.String())
		}
	}
	run([]string{"POSTGRES_DSN=" + src, "GATEWAY_CONFIG=" + cfgDir}, "backup", out)
	run(nil, "verify", out)
	restoredCfg := t.TempDir()
	run([]string{"POSTGRES_DSN=" + dst}, "restore", out, restoredCfg)

	b, err := OpenPostgres(ctx, dst) // also proves the restored schema passes migrations
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	got, err := b.QueryCost(ctx, CostQuery{})
	want, _ := a.QueryCost(ctx, CostQuery{})
	if err != nil || len(got) != 1 || got[0].Requests != want[0].Requests || got[0].CostUSD != want[0].CostUSD {
		t.Fatalf("cost totals after restore: %+v want %+v (%v)", got, want, err)
	}
	k, err := b.GetKey(ctx, "k1")
	if err != nil || k == nil || !bytes.Equal(k.Hash, key.Hash) || k.Tenant != "a" {
		t.Fatalf("key after restore: %+v %v", k, err)
	}
	pv, _ := b.ListPolicyVersions(ctx, "default")
	if len(pv) != 1 || !pv[0].Active {
		t.Fatalf("policy versions after restore: %+v", pv)
	}
	if d, _ := b.GetDoc(ctx, "budget", "b1"); d == nil {
		t.Fatal("API-managed budget lost")
	}
	if _, err := os.Stat(filepath.Join(restoredCfg, "gateway.yaml")); err != nil {
		t.Fatalf("config files not restored: %v", err)
	}
	if _, err := b.pool.Exec(ctx, `DELETE FROM audit_log WHERE id=$1`, auditID); err == nil {
		t.Fatal("restored audit_log is no longer append-only")
	}
	// A corrupted archive is refused before anything is touched.
	f, _ := os.OpenFile(filepath.Join(out, "gateway.dump"), os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte("x"))
	f.Close()
	cmd := exec.Command("../../scripts/backup.sh", "restore", out)
	cmd.Env = append(os.Environ(), "POSTGRES_DSN="+dst)
	if err := cmd.Run(); err == nil {
		t.Fatal("restore accepted a corrupted archive")
	}
}

// tempDB creates an empty database next to the one in dsn and returns its DSN.
func tempDB(t *testing.T, dsn, name string) string {
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	for _, q := range []string{"DROP DATABASE IF EXISTS " + name + " WITH (FORCE)", "CREATE DATABASE " + name} {
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, dsn)
		if err == nil {
			c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(ctx)
		}
	})
	u, _ := url.Parse(dsn)
	u.Path = "/" + name
	return u.String()
}
