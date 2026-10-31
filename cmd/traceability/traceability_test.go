package main

import (
	"os"
	"testing"
)

// Every MUST requirement maps to at least one automated test, every test names the IDs
// it covers, every named ID exists in the spec, and docs/TRACEABILITY.md is current.
// Covers: NFR-22
func TestNFR22_EveryMustIsTracedToATest(t *testing.T) {
	r, err := build("../..", "docs/SPEC.md")
	if err != nil {
		t.Fatal(err)
	}
	if r.mustTotal < 100 {
		t.Fatalf("parsed only %d MUST requirements from the spec", r.mustTotal)
	}
	for _, id := range r.uncovered {
		t.Errorf("MUST %s has no test", id)
	}
	for _, name := range r.untagged {
		t.Errorf("%s has no Covers: line", name)
	}
	for _, id := range r.unknown {
		t.Errorf("Covers names %s, which is not in the spec", id)
	}
	if b, err := os.ReadFile("../../docs/TRACEABILITY.md"); err != nil || string(b) != r.markdown {
		t.Error("docs/TRACEABILITY.md is stale: run go run ./cmd/traceability")
	}
}
