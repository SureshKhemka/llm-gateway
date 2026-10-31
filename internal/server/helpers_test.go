package server_test

import (
	"testing"
	"time"

	"github.com/SureshKhemka/llm-gateway/internal/store"
)

func storeFilterAll() store.CostFilter { return store.CostFilter{IncludeShadow: true} }

// waitFor polls cond for up to 3 seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within 3s")
}
