GO ?= go

.PHONY: build test vet race scenarios lmstudio traceability bench demo-up demo-down clean load chaos soak coverage supply-chain image helm-lint

build:
	$(GO) build -o bin/ ./cmd/...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

# Unattended demo scenarios against real binaries using the official SDKs.
scenarios: build
	python3 -m pytest scenarios -q

# LM Studio integration suite (run on the Mac with LM Studio serving).
lmstudio:
	LMSTUDIO_BASE_URL=$${LMSTUDIO_BASE_URL:-http://localhost:1234/v1} $(GO) test -tags lmstudio ./integration/...

traceability:
	$(GO) run ./cmd/traceability

bench:
	$(GO) test -run '^$$' -bench . -benchmem ./internal/...

# Release suites (record pass/fail from real runs in docs/PROGRESS.md).
load:
	./scripts/load.sh

chaos:
	./scripts/chaos.sh

soak:
	./scripts/soak.sh

coverage:
	./scripts/coverage.sh

supply-chain:
	./scripts/supply-chain.sh

image:
	docker build -t llm-gateway:dev .

helm-lint:
	helm lint deploy/helm/llm-gateway

demo-up:
	./deploy/demo/up.sh

demo-down:
	./deploy/demo/down.sh

clean:
	rm -rf bin dist coverage.out
