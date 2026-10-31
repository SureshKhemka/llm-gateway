#!/usr/bin/env bash
# Load test against the simulated provider [NFR-01..NFR-04, NFR-09].
# Pins the gateway to 4 CPUs (the spec's reference instance size is 4 vCPU / 8 GB) and
# runs the simulator and the load generator on other cores when taskset is available.
#
#   scripts/load.sh                 # non-streaming throughput + latency, then 5,000 streams
#   INSTANCES=2 scripts/load.sh     # horizontal scaling check (two gateways)
#   OUT=docs/benchmarks scripts/load.sh
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=${OUT:-/tmp/gw-load}
DUR=${DUR:-30s}
INSTANCES=${INSTANCES:-1}
mkdir -p "$OUT" bin
go build -o bin/ ./cmd/gateway ./cmd/simprovider ./cmd/loadtest

# Pin only when there are enough cores to separate gateway, simulator and load generator.
if command -v nproc >/dev/null && [ "$(nproc)" -ge 16 ] && command -v taskset >/dev/null; then
  pin() { exec taskset -c "$1" "${@:2}"; }
else
  pin() { exec "${@:2}"; }
  echo "note: $(nproc 2>/dev/null || echo '?') CPUs: gateway, simulator and load generator share them (results are a lower bound)"
fi
cleanup() { kill $(jobs -p) 2>/dev/null || true; }
trap cleanup EXIT

export SIMA_URL=http://127.0.0.1:19101/v1 SIMB_URL=http://127.0.0.1:19102/v1
export DEMO_KEY_A=gw-load-a_aaaaaaaaaaaaaaaa DEMO_KEY_B=gw-load-b_bbbbbbbbbbbbbbbb DEMO_ADMIN_KEY=gw-load-admin
pin 4-7 bin/simprovider -instance sima=127.0.0.1:19101 -instance simb=127.0.0.1:19102 >/dev/null 2>&1 &
sleep 0.5
URLS=""
for i in $(seq 1 "$INSTANCES"); do
  port=$((18180 + i))
  cpus=$(( (i-1)*4 ))-$(( (i-1)*4 + 3 ))
  [ "$INSTANCES" -gt 1 ] && cpus=$(( 8 + (i-1)*2 ))-$(( 8 + (i-1)*2 + 1 ))
  GOMAXPROCS=4 GATEWAY_LISTEN=127.0.0.1:$port pin "${CPUS:-$cpus}" bin/gateway -config deploy/demo/gateway.yaml >"$OUT/gateway-$i.log" 2>&1 &
  URLS="${URLS:+$URLS,}http://127.0.0.1:$port"
done
sleep 1.5
# Request logging to stdout costs throughput; the load numbers use the default config.
# The load generator runs in a subshell so pin's exec doesn't replace this script.
(pin 12-15 bin/loadtest -url "$URLS" -key "$DEMO_KEY_A" -concurrency "${CONC:-256}" -duration "$DUR" -json "$OUT/nonstream.json") || true
(pin 12-15 bin/loadtest -url "$URLS" -key "$DEMO_KEY_A" -streams "${STREAMS:-5000}" -duration "$DUR" -json "$OUT/streams.json") || true
echo "results in $OUT"
