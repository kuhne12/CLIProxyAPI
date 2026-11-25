package cliproxy

import (
	"context"
	"time"
)

const bpsCatalogRefreshInterval = 5 * time.Minute

type bpsCatalogRefresher struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Initial discovery and credential updates use the normal registration path.
// This loop also discovers additions/removals for long-lived credentials and
// retries failed startup discovery without adding work to inference requests.
func (s *Service) startBPSCatalogRefresh(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if s.bpsCatalogs != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &bpsCatalogRefresher{cancel: cancel, done: make(chan struct{})}
	s.bpsCatalogs = r
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(bpsCatalogRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.refreshBPSCatalogs(ctx)
			}
		}
	}()
}

func (s *Service) refreshBPSCatalogs(ctx context.Context) {
	if s == nil || s.coreManager == nil || s.pluginHost == nil || ctx.Err() != nil {
		return
	}
	var tasks []modelRegistrationTask
	for _, id := range s.coreManager.IDsByProvider("bps") {
		auth, ok := s.coreManager.GetByID(id)
		if !ok || auth.Disabled {
			continue
		}
		tasks = append(tasks, modelRegistrationTask{
			phase: modelRegistrationPhase(auth), category: modelRegistrationCategory(auth),
			run: func(cache *openAICompatibilityRegistrationCache) {
				s.refreshModelRegistrationForAuthWithContext(ctx, auth, cache)
			},
		})
	}
	s.runModelRegistrationTasks(ctx, tasks)
}
