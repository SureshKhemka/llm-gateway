#!/usr/bin/env bash
# Tears down what deploy/demo/up.sh started [ENV-07]. KEEP_DATA=1 keeps the Postgres
# volume and the generated keys.
set -euo pipefail
cd "$(dirname "$0")"
RUN=$(pwd)/.run
[ -f "$RUN/mode" ] || { echo "demo is not running"; exit 0; }
case $(cat "$RUN/mode") in
docker)
  vol=(-v)
  [ "${KEEP_DATA:-0}" = 1 ] && vol=()
  docker compose -f docker-compose.yaml --env-file "$RUN/env" down "${vol[@]}" --remove-orphans >/dev/null 2>&1 ;;
local)
  if [ -f "$RUN/pids" ]; then
    # Gateway first so it drains, then its dependencies.
    tac "$RUN/pids" | while read -r pid; do kill "$pid" 2>/dev/null || true; done
    for _ in $(seq 1 50); do
      alive=0
      while read -r pid; do kill -0 "$pid" 2>/dev/null && alive=1; done <"$RUN/pids"
      [ $alive = 0 ] && break
      sleep 0.2
    done
  fi ;;
esac
rm -f "$RUN/mode" "$RUN/pids"
rm -rf "$RUN/conf" "$RUN/wal"
[ "${KEEP_DATA:-0}" = 1 ] || rm -f "$RUN/env"
echo "demo stopped"
