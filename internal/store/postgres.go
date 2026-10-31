package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Postgres is the production Store.
type Postgres struct {
	pool *pgxpool.Pool
}

// OpenPostgres connects and applies pending migrations.
func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	p := &Postgres{pool: pool}
	if err := p.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

// Migrate applies every *.up.sql migration not yet recorded, in order [NFR-18].
func (p *Postgres) Migrate(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	ups, err := migrationFiles(".up.sql")
	if err != nil {
		return err
	}
	for _, f := range ups {
		version := strings.TrimSuffix(f, ".up.sql")
		var exists bool
		if err := p.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, version).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sql, _ := migrations.ReadFile("migrations/" + f)
		tx, err := p.pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", f, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, version); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// MigrateDown reverts the most recent migration (rollback of an upgrade) [NFR-18].
func (p *Postgres) MigrateDown(ctx context.Context) error {
	var version string
	err := p.pool.QueryRow(ctx, `SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	sql, err := migrations.ReadFile("migrations/" + version + ".down.sql")
	if err != nil {
		return err
	}
	if _, err := p.pool.Exec(ctx, string(sql)); err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version=$1`, version)
	return err
}

func migrationFiles(suffix string) ([]string, error) {
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }
func (p *Postgres) Close() error                   { p.pool.Close(); return nil }

const costCols = `request_id, ts, tenant, project, key_id, end_user, tags, endpoint, requested_model, deployment, provider, policy,
policy_version, route_reason, cache_status, input_tokens, cached_tokens, output_tokens, reasoning_tokens, cost_usd, input_usd,
cached_input_usd, output_usd, reasoning_usd, overhead_usd, baseline_usd, cache_saved_usd, estimated, attempts, status, error_code,
latency_ms, ttft_ms, shadow, eval_run, extra`

func (p *Postgres) InsertCostRecords(ctx context.Context, recs []CostRecord) error {
	if len(recs) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, r := range recs {
		tags := r.Tags
		if tags == nil {
			tags = []string{}
		}
		var extra any
		if len(r.Extra) > 0 {
			extra = r.Extra
		}
		b.Queue(`INSERT INTO cost_records (`+costCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36)
ON CONFLICT (request_id) DO UPDATE SET cost_usd=EXCLUDED.cost_usd, status=EXCLUDED.status, output_tokens=EXCLUDED.output_tokens`,
			r.RequestID, r.Time, r.Tenant, r.Project, r.KeyID, r.EndUser, tags, r.Endpoint, r.RequestedModel, r.Deployment, r.Provider,
			r.Policy, r.PolicyVersion, r.RouteReason, r.CacheStatus, r.InputTokens, r.CachedTokens, r.OutputTokens, r.ReasoningToks,
			r.CostUSD, r.InputUSD, r.CachedInputUSD, r.OutputUSD, r.ReasoningUSD, r.OverheadUSD, r.BaselineUSD, r.CacheSavedUSD,
			r.Estimated, r.Attempts, r.Status, r.ErrorCode, r.LatencyMs, r.TTFTMs, r.Shadow, r.EvalRun, extra)
	}
	return p.pool.SendBatch(ctx, b).Close()
}

// where builds a WHERE clause for a cost filter.
func where(f CostFilter) (string, []any) {
	var conds []string
	var args []any
	add := func(col string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if f.Tenant != "" {
		add("tenant", f.Tenant)
	}
	if f.Project != "" {
		add("project", f.Project)
	}
	if f.KeyID != "" {
		add("key_id", f.KeyID)
	}
	if f.EndUser != "" {
		add("end_user", f.EndUser)
	}
	if f.Deployment != "" {
		add("deployment", f.Deployment)
	}
	if f.Provider != "" {
		add("provider", f.Provider)
	}
	if f.Policy != "" {
		add("policy", f.Policy)
	}
	if f.EvalRun != "" {
		add("eval_run", f.EvalRun)
	}
	if !f.IncludeShadow {
		conds = append(conds, "NOT shadow")
	}
	if !f.From.IsZero() {
		args = append(args, f.From)
		conds = append(conds, fmt.Sprintf("ts >= $%d", len(args)))
	}
	if !f.To.IsZero() {
		args = append(args, f.To)
		conds = append(conds, fmt.Sprintf("ts < $%d", len(args)))
	}
	if f.Tag != "" {
		args = append(args, f.Tag)
		conds = append(conds, fmt.Sprintf("$%d = ANY(tags)", len(args)))
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

var dimCol = map[string]string{"tenant": "tenant", "project": "project", "key": "key_id", "end_user": "end_user",
	"model": "deployment", "requested_model": "requested_model", "provider": "provider", "policy": "policy",
	"cache_status": "cache_status", "tag": "tag"}

// QueryCost aggregates in SQL with the same semantics as Aggregate, so aggregates
// reconcile exactly with per-request rows [CST-04, CST-05].
func (p *Postgres) QueryCost(ctx context.Context, q CostQuery) ([]CostRow, error) {
	w, args := where(q.Filter)
	from := "cost_records"
	var sel, grp []string
	for _, d := range q.GroupBy {
		col, ok := dimCol[d]
		if !ok {
			return nil, fmt.Errorf("unknown group-by dimension %q", d)
		}
		if d == "tag" {
			from = "(SELECT *, unnest(CASE WHEN cardinality(tags)=0 THEN ARRAY[''] ELSE tags END) AS tag FROM cost_records) t"
		}
		sel = append(sel, col)
		grp = append(grp, col)
	}
	bucketExpr := "NULL::timestamptz"
	if q.Bucket > 0 {
		secs := int64(q.Bucket.Seconds())
		bucketExpr = fmt.Sprintf("to_timestamp(floor(extract(epoch from ts) / %d) * %d)", secs, secs)
		grp = append(grp, "1")
	}
	cols := append([]string{bucketExpr + " AS bucket"}, sel...)
	sql := `SELECT ` + strings.Join(cols, ", ") + `, count(*), sum(input_tokens), sum(cached_tokens), sum(output_tokens), sum(reasoning_tokens),
sum(cost_usd)::float8, sum(baseline_usd)::float8,
sum(CASE WHEN cache_status IN ('exact_hit','semantic_hit') THEN 0 ELSE baseline_usd - cost_usd END)::float8,
sum(cache_saved_usd)::float8, sum(CASE WHEN cache_status IN ('exact_hit','semantic_hit') THEN 1 ELSE 0 END), sum(CASE WHEN status >= 400 THEN 1 ELSE 0 END)
FROM ` + from + w
	if len(grp) > 0 {
		sql += " GROUP BY " + strings.Join(grp, ", ")
	}
	sql += " ORDER BY 1"
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CostRow
	for rows.Next() {
		var bucket *time.Time
		dims := make([]string, len(q.GroupBy))
		dest := []any{&bucket}
		for i := range dims {
			dest = append(dest, &dims[i])
		}
		var r CostRow
		dest = append(dest, &r.Requests, &r.InputTokens, &r.CachedTokens, &r.OutputTokens, &r.ReasoningToks,
			&r.CostUSD, &r.BaselineUSD, &r.RoutingSavings, &r.CacheSavings, &r.CacheHits, &r.Errors)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		if bucket != nil {
			r.Bucket = bucket.UTC()
		}
		r.Group = map[string]string{}
		for i, d := range q.GroupBy {
			r.Group[d] = dims[i]
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) ListCostRecords(ctx context.Context, f CostFilter, limit int) ([]CostRecord, error) {
	w, args := where(f)
	sql := `SELECT ` + costCols + ` FROM cost_records` + w + ` ORDER BY ts DESC`
	if limit > 0 {
		sql += " LIMIT " + strconv.Itoa(limit)
	}
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CostRecord
	for rows.Next() {
		var r CostRecord
		var extra []byte
		if err := rows.Scan(&r.RequestID, &r.Time, &r.Tenant, &r.Project, &r.KeyID, &r.EndUser, &r.Tags, &r.Endpoint, &r.RequestedModel,
			&r.Deployment, &r.Provider, &r.Policy, &r.PolicyVersion, &r.RouteReason, &r.CacheStatus, &r.InputTokens, &r.CachedTokens,
			&r.OutputTokens, &r.ReasoningToks, &r.CostUSD, &r.InputUSD, &r.CachedInputUSD, &r.OutputUSD, &r.ReasoningUSD, &r.OverheadUSD,
			&r.BaselineUSD, &r.CacheSavedUSD, &r.Estimated, &r.Attempts, &r.Status, &r.ErrorCode, &r.LatencyMs, &r.TTFTMs, &r.Shadow,
			&r.EvalRun, &extra); err != nil {
			return nil, err
		}
		r.Time = r.Time.UTC()
		if len(extra) > 0 {
			r.Extra = extra
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Rollup aggregates raw rows older than keepRaw into hourly rollups, verifies the
// rollup totals equal the raw totals, then deletes those raw rows [CST-06].
func (p *Postgres) Rollup(ctx context.Context, keepRaw time.Duration) (int64, error) {
	cut := time.Now().Add(-keepRaw)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var rawSum, before, after float64
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(cost_usd),0)::float8 FROM cost_records WHERE ts < $1 AND NOT shadow`, cut).Scan(&rawSum); err != nil {
		return 0, err
	}
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(cost_usd),0)::float8 FROM cost_rollups_hourly`).Scan(&before); err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO cost_rollups_hourly
SELECT date_trunc('hour', ts), tenant, project, key_id, end_user, deployment, provider, policy, count(*), sum(input_tokens),
 sum(cached_tokens), sum(output_tokens), sum(reasoning_tokens), sum(cost_usd), sum(baseline_usd),
 sum(CASE WHEN cache_status IN ('exact_hit','semantic_hit') THEN 0 ELSE baseline_usd - cost_usd END), sum(cache_saved_usd),
 sum(CASE WHEN cache_status IN ('exact_hit','semantic_hit') THEN 1 ELSE 0 END), sum(CASE WHEN status >= 400 THEN 1 ELSE 0 END)
FROM cost_records WHERE ts < $1 AND NOT shadow GROUP BY 1,2,3,4,5,6,7,8
ON CONFLICT (bucket, tenant, project, key_id, end_user, deployment, provider, policy) DO UPDATE SET
 requests = cost_rollups_hourly.requests + EXCLUDED.requests, cost_usd = cost_rollups_hourly.cost_usd + EXCLUDED.cost_usd,
 input_tokens = cost_rollups_hourly.input_tokens + EXCLUDED.input_tokens, output_tokens = cost_rollups_hourly.output_tokens + EXCLUDED.output_tokens,
 baseline_usd = cost_rollups_hourly.baseline_usd + EXCLUDED.baseline_usd`, cut)
	if err != nil {
		return 0, err
	}
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(cost_usd),0)::float8 FROM cost_rollups_hourly`).Scan(&after); err != nil {
		return 0, err
	}
	if d := after - before - rawSum; d > 1e-6 || d < -1e-6 {
		return 0, fmt.Errorf("rollup does not reconcile with raw rows (diff %.9f); raw rows kept", d)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM cost_records WHERE ts < $1`, cut)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}

func (p *Postgres) PutDecision(ctx context.Context, d DecisionRecord) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO decision_records (request_id, ts, tenant, body) VALUES ($1,$2,$3,$4)
ON CONFLICT (request_id) DO UPDATE SET body = EXCLUDED.body`, d.RequestID, d.Time, d.Tenant, []byte(d.Body))
	return err
}

func (p *Postgres) GetDecision(ctx context.Context, id string) (*DecisionRecord, error) {
	var d DecisionRecord
	var body []byte
	err := p.pool.QueryRow(ctx, `SELECT request_id, ts, tenant, body FROM decision_records WHERE request_id=$1`, id).Scan(&d.RequestID, &d.Time, &d.Tenant, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	d.Body = body
	return &d, err
}

func nullJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

func (p *Postgres) AppendAudit(ctx context.Context, e AuditEntry) (int64, error) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	var id int64
	err := p.pool.QueryRow(ctx, `INSERT INTO audit_log (ts, actor, role, tenant, action, object_kind, object_id, before, after, reason)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`, e.Time, e.Actor, e.Role, e.Tenant, e.Action, e.ObjectKind, e.ObjectID,
		nullJSON(e.Before), nullJSON(e.After), e.Reason).Scan(&id)
	return id, err
}

func (p *Postgres) ListAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	var conds []string
	var args []any
	if f.Tenant != "" {
		args = append(args, f.Tenant)
		conds = append(conds, fmt.Sprintf("tenant=$%d", len(args)))
	}
	if f.ObjectKind != "" {
		args = append(args, f.ObjectKind)
		conds = append(conds, fmt.Sprintf("object_kind=$%d", len(args)))
	}
	if !f.Since.IsZero() {
		args = append(args, f.Since)
		conds = append(conds, fmt.Sprintf("ts >= $%d", len(args)))
	}
	sql := `SELECT id, ts, actor, role, tenant, action, object_kind, object_id, before, after, reason FROM audit_log`
	if len(conds) > 0 {
		sql += " WHERE " + strings.Join(conds, " AND ")
	}
	sql += " ORDER BY id DESC"
	if f.Limit > 0 {
		sql += " LIMIT " + strconv.Itoa(f.Limit)
	}
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var before, after []byte
		if err := rows.Scan(&e.ID, &e.Time, &e.Actor, &e.Role, &e.Tenant, &e.Action, &e.ObjectKind, &e.ObjectID, &before, &after, &e.Reason); err != nil {
			return nil, err
		}
		e.Before, e.After = before, after
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *Postgres) PutKey(ctx context.Context, k KeyRecord) error {
	scopes := k.Scopes
	if len(scopes) == 0 {
		scopes = json.RawMessage("{}")
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO keys (id, prefix, hash, tenant, project, name, scopes, created_at, expires_at, revoked_at, rotated_from, overlap_until, managed)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, scopes=EXCLUDED.scopes, expires_at=EXCLUDED.expires_at,
 revoked_at=EXCLUDED.revoked_at, overlap_until=EXCLUDED.overlap_until`,
		k.ID, k.Prefix, k.Hash, k.Tenant, k.Project, k.Name, []byte(scopes), k.CreatedAt, k.ExpiresAt, k.RevokedAt, k.RotatedFrom, k.OverlapUntil, k.Managed)
	return err
}

const keyCols = `id, prefix, hash, tenant, project, name, scopes, created_at, expires_at, revoked_at, rotated_from, overlap_until, managed`

func scanKey(row pgx.Row) (*KeyRecord, error) {
	var k KeyRecord
	var scopes []byte
	err := row.Scan(&k.ID, &k.Prefix, &k.Hash, &k.Tenant, &k.Project, &k.Name, &scopes, &k.CreatedAt, &k.ExpiresAt, &k.RevokedAt, &k.RotatedFrom, &k.OverlapUntil, &k.Managed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	k.Scopes = scopes
	return &k, err
}

func (p *Postgres) GetKeyByHash(ctx context.Context, hash []byte) (*KeyRecord, error) {
	return scanKey(p.pool.QueryRow(ctx, `SELECT `+keyCols+` FROM keys WHERE hash=$1`, hash))
}

func (p *Postgres) GetKey(ctx context.Context, id string) (*KeyRecord, error) {
	return scanKey(p.pool.QueryRow(ctx, `SELECT `+keyCols+` FROM keys WHERE id=$1`, id))
}

func (p *Postgres) ListKeys(ctx context.Context, tenant string) ([]KeyRecord, error) {
	sql := `SELECT ` + keyCols + ` FROM keys`
	var args []any
	if tenant != "" {
		sql += ` WHERE tenant=$1`
		args = append(args, tenant)
	}
	rows, err := p.pool.Query(ctx, sql+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyRecord
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

func (p *Postgres) PutPolicyVersion(ctx context.Context, v PolicyVersion) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO policy_versions (name, version, created_at, actor, body, active, eval_run, override_reason)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (name, version) DO UPDATE SET active=EXCLUDED.active, eval_run=EXCLUDED.eval_run, override_reason=EXCLUDED.override_reason`,
		v.Name, v.Version, v.CreatedAt, v.Actor, []byte(v.Body), v.Active, v.EvalRun, v.OverrideReason)
	return err
}

func (p *Postgres) ListPolicyVersions(ctx context.Context, name string) ([]PolicyVersion, error) {
	sql := `SELECT name, version, created_at, actor, body, active, eval_run, override_reason FROM policy_versions`
	var args []any
	if name != "" {
		sql += ` WHERE name=$1`
		args = append(args, name)
	}
	rows, err := p.pool.Query(ctx, sql+` ORDER BY name, version`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PolicyVersion
	for rows.Next() {
		var v PolicyVersion
		var body []byte
		if err := rows.Scan(&v.Name, &v.Version, &v.CreatedAt, &v.Actor, &body, &v.Active, &v.EvalRun, &v.OverrideReason); err != nil {
			return nil, err
		}
		v.Body = body
		out = append(out, v)
	}
	return out, rows.Err()
}

func (p *Postgres) ActivatePolicyVersion(ctx context.Context, name string, version int) error {
	tag, err := p.pool.Exec(ctx, `UPDATE policy_versions SET active = (version = $2) WHERE name = $1`, name, version)
	if err != nil {
		return err
	}
	var found bool
	_ = p.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM policy_versions WHERE name=$1 AND version=$2)`, name, version).Scan(&found)
	if tag.RowsAffected() == 0 || !found {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) PutDoc(ctx context.Context, d Doc) error {
	if d.Updated.IsZero() {
		d.Updated = time.Now().UTC()
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO docs (kind, id, tenant, updated, body) VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (kind, id) DO UPDATE SET tenant=EXCLUDED.tenant, updated=EXCLUDED.updated, body=EXCLUDED.body`, d.Kind, d.ID, d.Tenant, d.Updated, []byte(d.Body))
	return err
}

func (p *Postgres) GetDoc(ctx context.Context, kind, id string) (*Doc, error) {
	var d Doc
	var body []byte
	err := p.pool.QueryRow(ctx, `SELECT kind, id, tenant, updated, body FROM docs WHERE kind=$1 AND id=$2`, kind, id).Scan(&d.Kind, &d.ID, &d.Tenant, &d.Updated, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	d.Body = body
	return &d, err
}

func (p *Postgres) ListDocs(ctx context.Context, kind, tenant string) ([]Doc, error) {
	sql := `SELECT kind, id, tenant, updated, body FROM docs WHERE kind=$1`
	args := []any{kind}
	if tenant != "" {
		sql += ` AND tenant=$2`
		args = append(args, tenant)
	}
	rows, err := p.pool.Query(ctx, sql+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Doc
	for rows.Next() {
		var d Doc
		var body []byte
		if err := rows.Scan(&d.Kind, &d.ID, &d.Tenant, &d.Updated, &body); err != nil {
			return nil, err
		}
		d.Body = body
		out = append(out, d)
	}
	return out, rows.Err()
}

func (p *Postgres) DeleteDoc(ctx context.Context, kind, id string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM docs WHERE kind=$1 AND id=$2`, kind, id)
	return err
}
