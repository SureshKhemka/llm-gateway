package guard

import (
	"strings"
	"testing"
)

// Covers: GRD-04
func TestGRD04_DetectsIndianAndGenericPII(t *testing.T) {
	text := "Mail asha.k@example.in or call +91 98765 43210. PAN ABCPE1234F, card 4111 1111 1111 1111, " +
		"Aadhaar 2341 2341 2346, key sk-ant-abcdefghijklmnopqrstuvwxyz012345. Invoice INV-2041 total 12,400."
	got := Types(Detect(text))
	want := []string{"aadhaar", "api_key", "card", "email", "pan", "phone"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("types %v want %v", got, want)
	}
	// Checksums keep ordinary numbers out.
	for _, s := range []string{"order 1234 5678 9012 3456", "ref 234123412345", "total 12,400 for 3 items"} {
		if fs := Detect(s); len(fs) != 0 {
			t.Errorf("false positive in %q: %v", s, fs)
		}
	}
	red, fs := Redact(text)
	if len(fs) != 6 || strings.Contains(red, "asha.k@example.in") || strings.Contains(red, "ABCPE1234F") || !strings.Contains(red, "[REDACTED_EMAIL]") || !strings.Contains(red, "INV-2041") {
		t.Fatalf("redaction: %s", red)
	}
}

// Covers: NFR-12, GRD-03
func TestNFR12_SealedBodiesRoundTrip(t *testing.T) {
	s, err := NewSealer("a-very-secret-body-key")
	if err != nil {
		t.Fatal(err)
	}
	sealed := s.Seal([]byte("prompt text"))
	if strings.Contains(string(sealed), "prompt text") {
		t.Fatal("ciphertext contains plaintext")
	}
	plain, err := s.Open(sealed)
	if err != nil || string(plain) != "prompt text" {
		t.Fatalf("open: %v %q", err, plain)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := s.Open(sealed); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}
