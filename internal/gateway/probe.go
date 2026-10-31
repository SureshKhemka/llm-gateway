package gateway

import (
	"context"
	"sync"
	"time"
)

// ProbeOnce actively checks every enabled provider [RES-01]: an unreachable provider
// marks all its deployments failed; a reachable provider that no longer lists a model
// (unloaded in LM Studio) marks that deployment unhealthy and cold [PRV-10].
func (e *Engine) ProbeOnce(ctx context.Context) {
	s := e.Snapshot()
	byProvider := map[string][]string{}
	for _, id := range s.Catalog.Order {
		d := s.Catalog.Deployments[id]
		if d.Enabled {
			byProvider[d.Provider] = append(byProvider[d.Provider], id)
		}
	}
	var wg sync.WaitGroup
	for pid, deps := range byProvider {
		ad := s.Providers.Get(pid)
		if ad == nil {
			continue
		}
		wg.Add(1)
		go func(pid string, deps []string) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			models, err := ad.ListModels(cctx)
			listed := map[string]bool{}
			for _, m := range models {
				listed[m] = true
			}
			for _, id := range deps {
				d := s.Catalog.Deployments[id]
				switch {
				case err != nil:
					e.Health.ProbeResult(id, false, false, err.Error())
				case len(models) == 0:
					// Providers that do not list models (Anthropic) are only checked for reachability.
					e.Health.ProbeResult(id, true, true, "")
				default:
					e.Health.ProbeResult(id, true, listed[d.Model], "")
				}
			}
		}(pid, deps)
	}
	wg.Wait()
}

// RunProbes probes every interval until ctx is done.
func (e *Engine) RunProbes(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		e.ProbeOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
