GO ?= go

.PHONY: build test vet race scenarios lmstudio traceability bench demo-up demo-down clean

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

demo-up:
	./deploy/demo/up.sh

demo-down:
	./deploy/demo/down.sh

clean:
	rm -rf bin
