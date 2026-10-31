#!/usr/bin/env bash
# Backup and restore of configuration, keys and cost records [NFR-19].
#
#   POSTGRES_DSN=... GATEWAY_CONFIG=deploy/demo scripts/backup.sh backup  /backups/2026-10-31
#   POSTGRES_DSN=... scripts/backup.sh restore /backups/2026-10-31 [CONFIG_DIR]
#   scripts/backup.sh verify /backups/2026-10-31
#
# What is captured: the Postgres database (API-managed objects, policy versions, hashed
# keys, cost and decision records, rollups, the audit log) as a pg_dump custom archive,
# plus the configuration files. Secret values are not in either: files hold env:/file:
# references only (CFG-04), so back up your secret store separately.
#
# PG_EXEC runs the Postgres tools somewhere else, e.g. in the demo's container:
#   PG_EXEC="docker compose -f deploy/demo/docker-compose.yaml --env-file deploy/demo/.run/env exec -T postgres"
#   POSTGRES_DSN=postgres://gw:...@localhost:5432/gateway
set -euo pipefail
cmd=${1:-}; dir=${2:-}
[ -n "$cmd" ] && [ -n "$dir" ] || { sed -n '2,16p' "$0"; exit 2; }
: "${POSTGRES_DSN:=}"
PG_EXEC=${PG_EXEC:-}
BIN=${PG_BIN:-$(ls -d /usr/lib/postgresql/*/bin 2>/dev/null | sort -V | tail -1)}
tool() {
  if [ -n "$PG_EXEC" ]; then echo "$PG_EXEC $1"; elif [ -x "$BIN/$1" ]; then echo "$BIN/$1"; else echo "$1"; fi
}
sha() { if command -v sha256sum >/dev/null; then sha256sum "$@"; else shasum -a 256 "$@"; fi; }
counts() {
  $(tool psql) "$POSTGRES_DSN" -tA -F' ' -c "SELECT 'cost_records', count(*) FROM cost_records UNION ALL SELECT 'keys', count(*) FROM keys
    UNION ALL SELECT 'audit_log', count(*) FROM audit_log UNION ALL SELECT 'policy_versions', count(*) FROM policy_versions
    UNION ALL SELECT 'docs', count(*) FROM docs UNION ALL SELECT 'cost_rollups_hourly', count(*) FROM cost_rollups_hourly ORDER BY 1"
}

case $cmd in
backup)
  [ -n "$POSTGRES_DSN" ] || { echo "POSTGRES_DSN is required" >&2; exit 2; }
  mkdir -p "$dir"
  $(tool pg_dump) --format=custom --no-owner --no-privileges "$POSTGRES_DSN" >"$dir/gateway.dump"
  if [ -n "${GATEWAY_CONFIG:-}" ]; then
    if [ -d "$GATEWAY_CONFIG" ]; then
      tar -czf "$dir/config.tar.gz" -C "$GATEWAY_CONFIG" --exclude .run .
    else
      tar -czf "$dir/config.tar.gz" -C "$(dirname "$GATEWAY_CONFIG")" "$(basename "$GATEWAY_CONFIG")"
    fi
  fi
  counts >"$dir/counts.txt"
  (cd "$dir" && sha gateway.dump counts.txt $( [ -f config.tar.gz ] && echo config.tar.gz ) >SHA256SUMS)
  echo "backup written to $dir"; cat "$dir/counts.txt"
  ;;
verify)
  (cd "$dir" && sha -c SHA256SUMS)
  ;;
restore)
  [ -n "$POSTGRES_DSN" ] || { echo "POSTGRES_DSN is required" >&2; exit 2; }
  (cd "$dir" && sha -c --quiet SHA256SUMS)
  # One transaction: a failed restore leaves the target as it was.
  $(tool pg_restore) --clean --if-exists --no-owner --no-privileges --single-transaction --exit-on-error \
    -d "$POSTGRES_DSN" <"$dir/gateway.dump"
  if [ -n "${3:-}" ] && [ -f "$dir/config.tar.gz" ]; then
    mkdir -p "$3"; tar -xzf "$dir/config.tar.gz" -C "$3"
  fi
  counts >"$dir/counts.restored.txt"
  if ! diff -u "$dir/counts.txt" "$dir/counts.restored.txt"; then
    echo "row counts differ after restore" >&2; exit 1
  fi
  echo "restored $dir into the target database (row counts match)"
  ;;
*) echo "unknown command $cmd" >&2; exit 2 ;;
esac
