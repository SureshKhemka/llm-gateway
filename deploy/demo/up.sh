#!/usr/bin/env bash
# One command to bring up the full demo [ENV-07, NFR-17]: gateway, Redis, Postgres, two
# simulated providers, Prometheus + Grafana dashboards, and seeded tenants and keys.
#
#   deploy/demo/up.sh                    # Docker Compose (default when Docker is running)
#   LMSTUDIO=1 deploy/demo/up.sh         # also route to LM Studio on the host
#   DEMO_NO_DOCKER=1 deploy/demo/up.sh   # local binaries; Redis if redis-server exists
#
# Secrets are generated once into deploy/demo/.run/env (git-ignored) and never logged
# by the gateway. Tear down with deploy/demo/down.sh.
set -euo pipefail
cd "$(dirname "$0")"
DEMO=$(pwd)
ROOT=$(cd ../.. && pwd)
RUN=$DEMO/.run
mkdir -p "$RUN/conf" "$RUN/logs"

rand() { head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
if [ ! -f "$RUN/env" ]; then
  (umask 077; : >"$RUN/env")
  cat >>"$RUN/env" <<ENV
DEMO_KEY_A=gw-demo-a_$(rand)
DEMO_KEY_B=gw-demo-b_$(rand)
DEMO_ADMIN_KEY=gw-demo-admin_$(rand)
POSTGRES_PASSWORD=$(rand)
GRAFANA_PASSWORD=$(rand)
ENV
fi
set -a; . "$RUN/env"; set +a

rm -f "$RUN/conf/"*.yaml
cp "$DEMO/gateway.yaml" "$RUN/conf/gateway.yaml"
if [ "${LMSTUDIO:-0}" = 1 ]; then cp "$DEMO/overlays/lmstudio.yaml" "$RUN/conf/lmstudio.yaml"; fi
chmod 755 "$RUN/conf"; chmod 644 "$RUN/conf/"*.yaml   # read by the image's nonroot user

PORT=${GATEWAY_PORT:-8080}
use_docker() { [ "${DEMO_NO_DOCKER:-0}" != 1 ] && command -v docker >/dev/null && docker info >/dev/null 2>&1; }

wait_ready() {
  for _ in $(seq 1 120); do
    curl -fsS "http://127.0.0.1:$PORT/readyz" >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  echo "gateway did not become ready; logs in $RUN/logs" >&2
  return 1
}

if use_docker; then
  echo docker >"$RUN/mode"
  if [ -z "${GATEWAY_IMAGE:-}" ]; then
    secret=()
    [ -n "${BUILD_CA:-}" ] && secret=(--secret "id=ca,src=$BUILD_CA")
    docker build -q "${secret[@]}" --build-arg HTTPS_PROXY --build-arg HTTP_PROXY --build-arg NO_PROXY \
      -t llm-gateway:dev "$ROOT" >/dev/null
  fi
  DEMO_CONF_DIR=$RUN/conf docker compose -f "$DEMO/docker-compose.yaml" --env-file "$RUN/env" up -d --quiet-pull >/dev/null
  wait_ready || { docker compose -f "$DEMO/docker-compose.yaml" --env-file "$RUN/env" logs gateway >"$RUN/logs/gateway.log" 2>&1; exit 1; }
  DASH="Grafana    http://localhost:${GRAFANA_PORT:-3000}/d/llm-gateway (anonymous viewer)
  Prometheus http://localhost:${PROMETHEUS_PORT:-9090}"
else
  echo local >"$RUN/mode"
  : >"$RUN/pids"
  (cd "$ROOT" && go build -o bin/ ./cmd/gateway ./cmd/simprovider ./cmd/gwctl)
  SIMA=${SIMA_PORT:-9001} SIMB=${SIMB_PORT:-9002}
  "$ROOT/bin/simprovider" -instance "sima=127.0.0.1:$SIMA" -instance "simb=127.0.0.1:$SIMB" >"$RUN/logs/sim.log" 2>&1 &
  echo $! >>"$RUN/pids"
  export SIMA_URL=http://127.0.0.1:$SIMA/v1 SIMB_URL=http://127.0.0.1:$SIMB/v1 WAL_DIR=$RUN/wal
  if command -v redis-server >/dev/null && [ -z "${REDIS_URL:-}" ]; then
    RPORT=${REDIS_PORT:-6380}
    redis-server --port "$RPORT" --bind 127.0.0.1 --save "" --appendonly no >"$RUN/logs/redis.log" 2>&1 &
    echo $! >>"$RUN/pids"
    export REDIS_URL=redis://127.0.0.1:$RPORT/0
  fi
  # Postgres only if the caller provides one (POSTGRES_DSN); otherwise the in-memory
  # store with the write-ahead log.
  GATEWAY_LISTEN=127.0.0.1:$PORT "$ROOT/bin/gateway" -config "$RUN/conf" >"$RUN/logs/gateway.log" 2>&1 &
  echo $! >>"$RUN/pids"
  wait_ready
  DASH="Dashboards need Docker (Prometheus/Grafana); metrics are at http://localhost:$PORT/metrics"
fi

cat <<OUT
LLM gateway demo is up ($(cat "$RUN/mode")).
  OpenAI     base_url=http://localhost:$PORT/v1   api_key=$DEMO_KEY_A   (tenant team-a)
  Anthropic  ANTHROPIC_BASE_URL=http://localhost:$PORT  ANTHROPIC_API_KEY=$DEMO_KEY_B   (tenant team-b)
  Console    http://localhost:$PORT/console/   admin key: $DEMO_ADMIN_KEY
  CLI        GWCTL_URL=http://localhost:$PORT GWCTL_KEY=\$DEMO_ADMIN_KEY bin/gwctl health
  $DASH
Keys are in $RUN/env. Stop with deploy/demo/down.sh
OUT
