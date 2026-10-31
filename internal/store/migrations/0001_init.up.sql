-- 0001: durable gateway state.
CREATE TABLE IF NOT EXISTS cost_records (
    request_id       text PRIMARY KEY,
    ts               timestamptz NOT NULL,
    tenant           text NOT NULL,
    project          text NOT NULL,
    key_id           text NOT NULL DEFAULT '',
    end_user         text NOT NULL DEFAULT '',
    tags             text[] NOT NULL DEFAULT '{}',
    endpoint         text NOT NULL DEFAULT '',
    requested_model  text NOT NULL DEFAULT '',
    deployment       text NOT NULL DEFAULT '',
    provider         text NOT NULL DEFAULT '',
    policy           text NOT NULL DEFAULT '',
    policy_version   int  NOT NULL DEFAULT 0,
    route_reason     text NOT NULL DEFAULT '',
    cache_status     text NOT NULL DEFAULT '',
    input_tokens     int  NOT NULL DEFAULT 0,
    cached_tokens    int  NOT NULL DEFAULT 0,
    output_tokens    int  NOT NULL DEFAULT 0,
    reasoning_tokens int  NOT NULL DEFAULT 0,
    cost_usd         numeric(20,12) NOT NULL DEFAULT 0,
    input_usd        numeric(20,12) NOT NULL DEFAULT 0,
    cached_input_usd numeric(20,12) NOT NULL DEFAULT 0,
    output_usd       numeric(20,12) NOT NULL DEFAULT 0,
    reasoning_usd    numeric(20,12) NOT NULL DEFAULT 0,
    overhead_usd     numeric(20,12) NOT NULL DEFAULT 0,
    baseline_usd     numeric(20,12) NOT NULL DEFAULT 0,
    cache_saved_usd  numeric(20,12) NOT NULL DEFAULT 0,
    estimated        boolean NOT NULL DEFAULT false,
    attempts         int NOT NULL DEFAULT 0,
    status           int NOT NULL DEFAULT 0,
    error_code       text NOT NULL DEFAULT '',
    latency_ms       double precision NOT NULL DEFAULT 0,
    ttft_ms          double precision NOT NULL DEFAULT 0,
    shadow           boolean NOT NULL DEFAULT false,
    eval_run         text NOT NULL DEFAULT '',
    extra            jsonb
);
CREATE INDEX IF NOT EXISTS cost_records_tenant_ts ON cost_records (tenant, ts);
CREATE INDEX IF NOT EXISTS cost_records_ts ON cost_records (ts);

-- Hourly rollups outlive raw rows (raw ≥ 13 months, rollups ≥ 3 years) [CST-06].
CREATE TABLE IF NOT EXISTS cost_rollups_hourly (
    bucket          timestamptz NOT NULL,
    tenant          text NOT NULL,
    project         text NOT NULL,
    key_id          text NOT NULL,
    end_user        text NOT NULL,
    deployment      text NOT NULL,
    provider        text NOT NULL,
    policy          text NOT NULL,
    requests        bigint NOT NULL,
    input_tokens    bigint NOT NULL,
    cached_tokens   bigint NOT NULL,
    output_tokens   bigint NOT NULL,
    reasoning_tokens bigint NOT NULL,
    cost_usd        numeric(24,12) NOT NULL,
    baseline_usd    numeric(24,12) NOT NULL,
    routing_savings numeric(24,12) NOT NULL,
    cache_savings   numeric(24,12) NOT NULL,
    cache_hits      bigint NOT NULL,
    errors          bigint NOT NULL,
    PRIMARY KEY (bucket, tenant, project, key_id, end_user, deployment, provider, policy)
);

CREATE TABLE IF NOT EXISTS decision_records (
    request_id text PRIMARY KEY,
    ts         timestamptz NOT NULL,
    tenant     text NOT NULL,
    body       jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS decision_records_ts ON decision_records (ts);

CREATE TABLE IF NOT EXISTS audit_log (
    id          bigserial PRIMARY KEY,
    ts          timestamptz NOT NULL DEFAULT now(),
    actor       text NOT NULL,
    role        text NOT NULL,
    tenant      text NOT NULL DEFAULT '',
    action      text NOT NULL,
    object_kind text NOT NULL,
    object_id   text NOT NULL,
    before      jsonb,
    after       jsonb,
    reason      text NOT NULL DEFAULT ''
);
-- Append-only [OBS-06]: updates and deletes are rejected even for the owner role.
CREATE OR REPLACE FUNCTION audit_log_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only';
END $$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS audit_log_no_change ON audit_log;
CREATE TRIGGER audit_log_no_change BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_immutable();

CREATE TABLE IF NOT EXISTS keys (
    id            text PRIMARY KEY,
    prefix        text NOT NULL,
    hash          bytea NOT NULL UNIQUE,
    tenant        text NOT NULL,
    project       text NOT NULL,
    name          text NOT NULL DEFAULT '',
    scopes        jsonb NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL,
    expires_at    timestamptz,
    revoked_at    timestamptz,
    rotated_from  text NOT NULL DEFAULT '',
    overlap_until timestamptz,
    managed       text NOT NULL DEFAULT 'api'
);

CREATE TABLE IF NOT EXISTS policy_versions (
    name            text NOT NULL,
    version         int NOT NULL,
    created_at      timestamptz NOT NULL,
    actor           text NOT NULL,
    body            jsonb NOT NULL,
    active          boolean NOT NULL DEFAULT false,
    eval_run        text NOT NULL DEFAULT '',
    override_reason text NOT NULL DEFAULT '',
    PRIMARY KEY (name, version)
);

CREATE TABLE IF NOT EXISTS docs (
    kind    text NOT NULL,
    id      text NOT NULL,
    tenant  text NOT NULL DEFAULT '',
    updated timestamptz NOT NULL,
    body    jsonb NOT NULL,
    PRIMARY KEY (kind, id)
);
