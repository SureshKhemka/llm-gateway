#!/usr/bin/env bash
# Soak test [NFR-21, NFR-05]: steady mixed load for DURATION (24 h by default) against a
# gateway with Redis, sampling memory, goroutines, open file descriptors and errors.
# Passes if there were no client errors and nothing grows without bound: memory,
# goroutines and FDs in the last quarter of the run stay within GROWTH (default 1.25x)
# of the second quarter (the first quarter is warm-up).
#
#   scripts/soak.sh                     # 24 h at 200 req/s
#   DURATION=15m RPS=300 scripts/soak.sh
set -euo pipefail
cd "$(dirname "$0")/.."
DURATION=${DURATION:-24h}
RPS=${RPS:-200}
EVERY=${EVERY:-10}
GROWTH=${GROWTH:-1.25}
OUT=${OUT:-/tmp/gw-soak}
mkdir -p "$OUT" bin
go build -o bin/ ./cmd/gateway ./cmd/simprovider ./cmd/loadtest
cleanup() { kill $(jobs -p) 2>/dev/null || true; }
trap cleanup EXIT

export SIMA_URL=http://127.0.0.1:19301/v1 SIMB_URL=http://127.0.0.1:19302/v1
export DEMO_KEY_A=gw-soak-a_aaaaaaaaaaaaaaaa DEMO_KEY_B=gw-soak-b_bbbbbbbbbbbbbbbb DEMO_ADMIN_KEY=gw-soak-admin
export WAL_DIR=$OUT/wal
bin/simprovider -instance sima=127.0.0.1:19301 -instance simb=127.0.0.1:19302 >"$OUT/sim.log" 2>&1 &
if command -v redis-server >/dev/null && [ -z "${REDIS_URL:-}" ]; then
  redis-server --port 16390 --bind 127.0.0.1 --save "" --appendonly no >"$OUT/redis.log" 2>&1 &
  export REDIS_URL=redis://127.0.0.1:16390/0
fi
sleep 0.5
GATEWAY_LISTEN=127.0.0.1:18390 bin/gateway -config deploy/demo/gateway.yaml >"$OUT/gateway.log" 2>&1 &
GW=$!
for _ in $(seq 1 50); do curl -fsS http://127.0.0.1:18390/readyz >/dev/null 2>&1 && break; sleep 0.2; done

# Mixed load: non-streaming at RPS, plus a smaller streaming share; cache allowed so the
# cache's memory is exercised too.
bin/loadtest -url http://127.0.0.1:18390 -key "$DEMO_KEY_A" -rps "$RPS" -concurrency 64 -duration "$DURATION" -cache \
  -json "$OUT/nonstream.json" >/dev/null 2>&1 &
NS=$!
bin/loadtest -url http://127.0.0.1:18390 -key "$DEMO_KEY_A" -streams 50 -rps "$((RPS / 10 + 1))" -duration "$DURATION" \
  -json "$OUT/streams.json" >/dev/null 2>&1 &
ST=$!

echo "t,rss_bytes,goroutines,open_fds,requests_total" >"$OUT/samples.csv"
start=$(date +%s)
while kill -0 $NS 2>/dev/null; do
  m=$(curl -fsS http://127.0.0.1:18390/metrics 2>/dev/null || true)
  rss=$(awk '/^process_resident_memory_bytes/ {print $2}' <<<"$m")
  gor=$(awk '/^go_goroutines/ {print $2}' <<<"$m")
  fds=$(awk '/^process_open_fds/ {print $2}' <<<"$m")
  req=$(awk '/^gw_requests_total/ {s+=$2} END {print s+0}' <<<"$m")
  echo "$(( $(date +%s) - start )),$rss,$gor,$fds,$req" >>"$OUT/samples.csv"
  kill -0 $GW 2>/dev/null || { echo "gateway exited during the soak" >&2; exit 1; }
  sleep "$EVERY"
done
wait $NS || true
wait $ST || true

python3 - "$OUT" "$GROWTH" <<'PY'
import csv, json, sys
out, growth = sys.argv[1], float(sys.argv[2])
rows = [r for r in csv.DictReader(open(f"{out}/samples.csv")) if r["rss_bytes"]]
q = len(rows) // 4
if q < 2:
    sys.exit("too few samples; run longer")
def mean(rs, k): return sum(float(r[k]) for r in rs) / len(rs)
ok = True
report = {"samples": len(rows), "duration_s": int(rows[-1]["t"])}
for k in ("rss_bytes", "goroutines", "open_fds"):
    early, late = mean(rows[q:2 * q], k), mean(rows[3 * q:], k)
    report[k] = {"q2_mean": round(early), "q4_mean": round(late), "ratio": round(late / early, 3)}
    if late > early * growth:
        ok = False
for name in ("nonstream", "streams"):
    s = json.load(open(f"{out}/{name}.json"))
    report[name] = {"requests": s["requests"], "errors": s["errors"], "p95_ms": s["latency_ms"]["p95"]}
    if s["errors"] > 0:
        ok = False
report["pass"] = ok
print(json.dumps(report, indent=2))
json.dump(report, open(f"{out}/soak.json", "w"), indent=2)
sys.exit(0 if ok else 1)
PY
