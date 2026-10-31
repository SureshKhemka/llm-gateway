DROP TABLE IF EXISTS docs;
DROP TABLE IF EXISTS policy_versions;
DROP TABLE IF EXISTS keys;
DROP TRIGGER IF EXISTS audit_log_no_change ON audit_log;
DROP FUNCTION IF EXISTS audit_log_immutable();
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS decision_records;
DROP TABLE IF EXISTS cost_rollups_hourly;
DROP TABLE IF EXISTS cost_records;
