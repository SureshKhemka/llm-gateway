#!/usr/bin/env bash
# Chaos suite [NFR-21]: provider kill and recovery, provider slowdown, 429 storm,
# mid-stream drops and a gateway instance kill, under steady mixed load against real
# processes. Exit status 0 only if every phase passes.
#
#   scripts/chaos.sh                       # 15 s per phase
#   PHASE=60s WORKERS=64 scripts/chaos.sh
set -euo pipefail
cd "$(dirname "$0")/.."
go build -o bin/ ./cmd/gateway ./cmd/simprovider ./cmd/chaos
OUT=${OUT:-/tmp/gw-chaos.json}
bin/chaos -bin bin -phase "${PHASE:-15s}" -workers "${WORKERS:-24}" -json "$OUT"
