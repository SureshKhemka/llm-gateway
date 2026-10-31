// Package guard implements request guardrails: size caps, PII and secret detection
// and redaction, and encryption of stored bodies [GRD-01, GRD-03, GRD-04].
package guard

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"regexp"
	"sort"
	"strings"
)

// Finding is one detected PII or secret span.
type Finding struct {
	Type       string `json:"type"`
	Start, End int    `json:"-"`
}

type detector struct {
	typ   string
	re    *regexp.Regexp
	valid func(string) bool
}

var detectors = []detector{
	{"email", regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`), nil},
	// API keys and tokens before generic numbers so their digits are not double counted.
	{"api_key", regexp.MustCompile(`\b(sk-(ant-|proj-)?[A-Za-z0-9_\-]{20,}|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{36,}|xox[baprs]-[A-Za-z0-9\-]{10,}|gw-[a-z]+-[a-z0-9]{8}_[a-z0-9]{20,}|AIza[0-9A-Za-z_\-]{35})\b`), nil},
	{"pan", regexp.MustCompile(`\b[A-Z]{3}[ABCFGHLJPTK][A-Z][0-9]{4}[A-Z]\b`), nil},
	{"card", regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`), luhn},
	{"aadhaar", regexp.MustCompile(`\b[2-9]\d{3}[ -]?\d{4}[ -]?\d{4}\b`), verhoeff},
	{"phone", regexp.MustCompile(`(?:\+?91[ -]?)?\b[6-9]\d{4}[ -]?\d{5}\b|\+\d{1,3}[ -]?\(?\d{2,4}\)?[ -]?\d{3,4}[ -]?\d{3,4}\b`), nil},
}

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func luhn(s string) bool {
	d := digits(s)
	if len(d) < 13 || len(d) > 19 {
		return false
	}
	sum, alt := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return sum%10 == 0
}

var (
	vD = [10][10]int{{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, {1, 2, 3, 4, 0, 6, 7, 8, 9, 5}, {2, 3, 4, 0, 1, 7, 8, 9, 5, 6}, {3, 4, 0, 1, 2, 8, 9, 5, 6, 7},
		{4, 0, 1, 2, 3, 9, 5, 6, 7, 8}, {5, 9, 8, 7, 6, 0, 4, 3, 2, 1}, {6, 5, 9, 8, 7, 1, 0, 4, 3, 2}, {7, 6, 5, 9, 8, 2, 1, 0, 4, 3},
		{8, 7, 6, 5, 9, 3, 2, 1, 0, 4}, {9, 8, 7, 6, 5, 4, 3, 2, 1, 0}}
	vP = [8][10]int{{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, {1, 5, 7, 6, 2, 8, 3, 0, 9, 4}, {5, 8, 0, 3, 7, 9, 6, 1, 4, 2}, {8, 9, 1, 6, 0, 4, 3, 5, 2, 7},
		{9, 4, 5, 3, 1, 2, 6, 8, 7, 0}, {4, 2, 8, 6, 5, 7, 3, 9, 0, 1}, {2, 7, 9, 3, 8, 0, 6, 4, 1, 5}, {7, 0, 4, 6, 9, 1, 3, 2, 5, 8}}
)

// verhoeff validates Aadhaar's checksum, which keeps random 12-digit numbers out.
func verhoeff(s string) bool {
	d := digits(s)
	if len(d) != 12 {
		return false
	}
	c := 0
	for i := 0; i < len(d); i++ {
		c = vD[c][vP[i%8][int(d[len(d)-1-i]-'0')]]
	}
	return c == 0
}

// Detect returns non-overlapping findings in text.
func Detect(text string) []Finding {
	var out []Finding
	taken := func(s, e int) bool {
		for _, f := range out {
			if s < f.End && e > f.Start {
				return true
			}
		}
		return false
	}
	for _, d := range detectors {
		for _, loc := range d.re.FindAllStringIndex(text, -1) {
			if taken(loc[0], loc[1]) {
				continue
			}
			if d.valid != nil && !d.valid(text[loc[0]:loc[1]]) {
				continue
			}
			out = append(out, Finding{Type: d.typ, Start: loc[0], End: loc[1]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// Redact replaces every finding with a typed placeholder.
func Redact(text string) (string, []Finding) {
	fs := Detect(text)
	if len(fs) == 0 {
		return text, nil
	}
	var b strings.Builder
	last := 0
	for _, f := range fs {
		b.WriteString(text[last:f.Start])
		b.WriteString("[REDACTED_" + strings.ToUpper(f.Type) + "]")
		last = f.End
	}
	b.WriteString(text[last:])
	return b.String(), fs
}

// Types lists the distinct finding types, sorted.
func Types(fs []Finding) []string {
	set := map[string]bool{}
	for _, f := range fs {
		set[f.Type] = true
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// ---------- body encryption [NFR-12, GRD-03] ----------

// Sealer encrypts stored bodies with AES-256-GCM.
type Sealer struct{ aead cipher.AEAD }

// NewSealer derives a 256-bit key from secret.
func NewSealer(secret string) (*Sealer, error) {
	if len(secret) < 16 {
		return nil, errors.New("body encryption key must be at least 16 characters")
	}
	k := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

func (s *Sealer) Seal(plain []byte) []byte {
	nonce := make([]byte, s.aead.NonceSize())
	_, _ = rand.Read(nonce)
	return s.aead.Seal(nonce, nonce, plain, nil)
}

func (s *Sealer) Open(sealed []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("ciphertext too short")
	}
	return s.aead.Open(nil, sealed[:n], sealed[n:], nil)
}
