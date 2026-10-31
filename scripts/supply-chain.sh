#!/usr/bin/env bash
# Release supply-chain checks [NFR-15]: vulnerability scan, SBOM, secret scan, and
# (when a signing key is configured) a signed image.
#
#   scripts/supply-chain.sh                         # scans + SBOM into dist/
#   IMAGE=ghcr.io/org/llm-gateway:0.8.0 COSIGN_KEY=env://COSIGN_PRIVATE_KEY scripts/supply-chain.sh
#
# Fails on any finding: govulncheck reports only vulnerabilities the code can reach, and
# the release gate is "no unpatched critical or high CVEs".
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p dist
GOBIN=$(go env GOPATH)/bin
need() { command -v "$1" >/dev/null || [ -x "$GOBIN/$1" ]; }
run() { if command -v "$1" >/dev/null; then "$@"; else "$GOBIN/$1" "${@:2}"; fi; }

echo "== govulncheck"
need govulncheck || go install golang.org/x/vuln/cmd/govulncheck@v1.1.4
run govulncheck ./...

echo "== SBOM (CycloneDX)"
need cyclonedx-gomod || go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.9.0
for c in gateway simprovider gwctl; do
  run cyclonedx-gomod app -json -licenses -main "cmd/$c" -output "dist/sbom-$c.cdx.json" . 2>/dev/null
done
ls -1 dist/sbom-*.cdx.json

echo "== secret scan"
if need gitleaks; then
  run gitleaks git --no-banner --redact --config .gitleaks.toml .
  run gitleaks dir --no-banner --redact --config .gitleaks.toml .
else
  echo "gitleaks not installed; running the built-in scan (go test -run NFR15)"
fi
go test ./internal/server -run NFR15_NoSecretsInRepository -count=1

if [ -n "${IMAGE:-}" ]; then
  echo "== image $IMAGE"
  docker build -t "$IMAGE" .
  if [ -n "${COSIGN_KEY:-}" ]; then
    docker push "$IMAGE"
    cosign sign --yes --key "$COSIGN_KEY" "$IMAGE"
    cosign attest --yes --key "$COSIGN_KEY" --type cyclonedx --predicate dist/sbom-gateway.cdx.json "$IMAGE"
  else
    echo "COSIGN_KEY not set: image built but not signed (release images must be signed)"
  fi
fi
echo "supply-chain checks passed"
