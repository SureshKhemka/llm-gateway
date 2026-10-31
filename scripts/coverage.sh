#!/usr/bin/env bash
# Line coverage of the routing, budget, cost and cache logic [NFR-21]; fails below MIN
# (default 85%). Coverage counts statements reached from any package's tests
# (-coverpkg), since most of this logic is exercised through the HTTP server tests.
#
#   scripts/coverage.sh            # writes coverage.out and prints a per-group table
set -euo pipefail
cd "$(dirname "$0")/.."
MIN=${MIN:-85}
PROFILE=${PROFILE:-coverage.out}
# The timing assertions (NFR-01, NFR-10) are skipped: instrumentation distorts them.
go test ./internal/... -count=1 -skip 'NFR01|NFR10' \
  -coverpkg=./internal/gateway,./internal/governance,./internal/cost,./internal/cache \
  -coverprofile="$PROFILE" >/dev/null
python3 - "$PROFILE" "$MIN" <<'PY'
import collections, sys
prof, minimum = sys.argv[1], float(sys.argv[2])
blocks = {}
for line in open(prof):
    if line.startswith("mode:"):
        continue
    loc, stmts, cnt = line.rsplit(" ", 2)
    s, c = int(stmts), int(cnt)
    blocks[loc] = (s, max(c, blocks.get(loc, (0, 0))[1]))
per = collections.defaultdict(lambda: [0, 0])
for loc, (s, c) in blocks.items():
    f = loc.split(":")[0].split("llm-gateway/", 1)[1]
    per[f][0] += s
    per[f][1] += s if c else 0
groups = {
    "routing": ["internal/gateway/router.go", "internal/gateway/difficulty.go", "internal/gateway/similarity.go", "internal/gateway/experiments.go"],
    "budget": ["internal/governance/budget.go", "internal/governance/counters.go", "internal/governance/ratelimit.go"],
    "cost": [f for f in per if f.startswith("internal/cost/")],
    "cache": [f for f in per if f.startswith("internal/cache/")] + ["internal/gateway/cachelayer.go"],
}
ok = True
for g, files in groups.items():
    total = sum(per[f][0] for f in files)
    hit = sum(per[f][1] for f in files)
    pct = 100 * hit / total
    ok &= pct >= minimum
    print(f"{g:8s} {pct:5.1f}%  ({hit}/{total})  " + "  ".join(f"{f.rsplit('/', 1)[1]} {100 * per[f][1] / max(1, per[f][0]):.0f}%" for f in files))
print("PASS" if ok else f"FAIL: a group is below {minimum}%")
sys.exit(0 if ok else 1)
PY
