package cliproxy

import (
	"context"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

type bpsQuotaRefresher struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Query the read-only account usage endpoint even when no panel is open. The
// plugin deduplicates concurrent panel queries and retains its last good sample.
func (s *Service) startBPSQuotaRefresh(ctx context.Context) {
	if s == nil || s.coreManager == nil || s.pluginHost == nil {
		return
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if s.bpsQuotas != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &bpsQuotaRefresher{cancel: cancel, done: make(chan struct{})}
	s.bpsQuotas = r
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			s.refreshBPSQuotas(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Service) refreshBPSQuotas(ctx context.Context) {
	if s == nil || s.coreManager == nil || s.pluginHost == nil || ctx.Err() != nil || !s.pluginHost.HasQuotaProviderForPlugin("bps") {
		return
	}
	jobs := make(chan pluginapi.QuotaFetchRequest)
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for req := range jobs {
				if ctx.Err() != nil {
					continue
				}
				if _, _, errFetch := s.pluginHost.FetchQuotaByPlugin(ctx, "bps", req); errFetch != nil && ctx.Err() == nil {
					log.WithField("auth_index", req.AuthIndex).Debug("BPS automatic quota refresh failed; retaining last successful snapshot")
				}
			}
		})
	}
	for _, id := range s.coreManager.IDsByProvider("bps") {
		auth, ok := s.coreManager.GetByID(id)
		if !ok || auth.Disabled || ctx.Err() != nil {
			continue
		}
		auth.EnsureIndex()
		select {
		case jobs <- pluginapi.QuotaFetchRequest{AuthIndex: auth.Index, AuthID: auth.ID, Provider: "bps"}:
		case <-ctx.Done():
		}
	}
	close(jobs)
	workers.Wait()
}
