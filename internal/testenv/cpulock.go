package testenv

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// ExclusiveCPU serialises CPU-sensitive tests across the test binaries `go test ./...`
// runs in parallel: timing assertions (NFR-01, RTE-16, NFR-10) and the tests that
// saturate the machine (load harness, demo builds) take the same file lock, so a
// latency budget is never measured while another package is running a load test.
func ExclusiveCPU(t testing.TB) {
	path := filepath.Join(os.TempDir(), "llm-gateway-cpu.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Logf("cpu lock unavailable: %v", err)
		return
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		t.Logf("cpu lock unavailable: %v", err)
		return
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	})
}
