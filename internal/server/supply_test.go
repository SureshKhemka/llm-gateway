package server_test

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// High-confidence credential formats. Test fixtures use the gateway's own fake key
// format (gw-...), which is not matched here.
var secretPatterns = map[string]*regexp.Regexp{
	"private key":        regexp.MustCompile(`-----BEGIN (RSA |EC |OPENSSH |)PRIVATE KEY-----`),
	"Anthropic key":      regexp.MustCompile(`sk-ant-[a-zA-Z0-9_-]{20,}`),
	"OpenAI key":         regexp.MustCompile(`sk-(proj-)?[A-Za-z0-9]{32,}`),
	"OpenRouter key":     regexp.MustCompile(`sk-or-v1-[a-f0-9]{32,}`),
	"AWS access key":     regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	"GitHub token":       regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`),
	"Slack token":        regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
	"Slack webhook":      regexp.MustCompile(`hooks\.slack\.com/services/T[A-Z0-9]+/B[A-Z0-9]+/[A-Za-z0-9]+`),
	"Google API key":     regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),
	"inline DB password": regexp.MustCompile(`postgres(ql)?://[^:/\s]+:[^@$\s{]{6,}@`),
}

// No credentials are committed: configuration holds env:/file: references only, and the
// repository passes a scan for common credential formats. scripts/supply-chain.sh adds
// govulncheck, gitleaks over the history and the SBOM.
// Covers: NFR-15, CFG-04, PRV-08
func TestNFR15_NoSecretsInRepository(t *testing.T) {
	root := "../.."
	skipDirs := map[string]bool{".git": true, "bin": true, "dist": true, "node_modules": true, ".run": true, "__pycache__": true}
	var hits []string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if info, _ := d.Info(); info == nil || info.Size() > 5<<20 {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			for name, re := range secretPatterns {
				if re.MatchString(line) && !strings.Contains(line, "secretPatterns") {
					hits = append(hits, path+":"+itoa(n)+" ("+name+")")
				}
			}
		}
		return nil
	})
	for _, h := range hits {
		t.Errorf("possible credential: %s", h)
	}
	// Every secret-bearing config field in the shipped configs is a reference.
	for _, f := range []string{"../../deploy/demo/gateway.yaml", "../../deploy/demo/overlays/lmstudio.yaml"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			l := strings.TrimSpace(line)
			for _, k := range []string{"secret:", "api_key:", "redis_url:", "postgres_dsn:"} {
				if strings.HasPrefix(l, k) || strings.Contains(l, " "+k) {
					v := strings.TrimSpace(l[strings.Index(l, k)+len(k):])
					if v != "" && !strings.HasPrefix(v, "env:") && !strings.HasPrefix(v, "file:") {
						t.Errorf("%s: %q is not a secret reference", f, l)
					}
				}
			}
		}
	}
}

func itoa(n int) string {
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
