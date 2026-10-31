#!/usr/bin/env bash
# Starts a throwaway Postgres cluster and runs the test suite against it.
#
#   scripts/pg-test.sh            # go test ./... with GATEWAY_TEST_PG_DSN set
#   scripts/pg-test.sh start      # just start it and print the DSN
#   scripts/pg-test.sh stop
#
# Needs PostgreSQL server binaries (initdb, pg_ctl). On Debian/Ubuntu:
#   apt-get install -y postgresql   (binaries land in /usr/lib/postgresql/<ver>/bin)
# On macOS: brew install postgresql@16
set -euo pipefail
PORT=${PG_TEST_PORT:-55432}
DATA=${PG_TEST_DATA:-/var/tmp/gw-pgdata}
BIN=${PG_BIN:-$(ls -d /usr/lib/postgresql/*/bin 2>/dev/null | sort -V | tail -1)}
[ -x "$BIN/initdb" ] || BIN=$(dirname "$(command -v initdb)")
DSN="postgres://gw@127.0.0.1:${PORT}/postgres?sslmode=disable"

as_pg() {
  if [ "$(id -u)" = 0 ]; then su postgres -s /bin/bash -c "$*"; else bash -c "$*"; fi
}

start() {
  if ! [ -f "$DATA/PG_VERSION" ]; then
    mkdir -p "$DATA"
    [ "$(id -u)" = 0 ] && chown postgres "$DATA"
    as_pg "$BIN/initdb -D $DATA -A trust -U gw >/dev/null"
  fi
  as_pg "$BIN/pg_ctl -D $DATA -o '-p $PORT -k /tmp -c listen_addresses=127.0.0.1' -l $DATA/log -w start" >/dev/null || true
  echo "$DSN"
}

stop() { as_pg "$BIN/pg_ctl -D $DATA -m fast stop" >/dev/null || true; }

case "${1:-test}" in
  start) start ;;
  stop) stop ;;
  test)
    start >/dev/null
    GATEWAY_TEST_PG_DSN="$DSN" go test ./... "${@:2}"
    ;;
esac
