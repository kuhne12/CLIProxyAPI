package auth

import (
	"context"
	"sync"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// A gate lives with the manager, independently of credential rotation and executor reloads.
type providerGenerationGate struct {
	mu      sync.Mutex
	active  int
	limit   uint64
	changed chan struct{}
}

func (g *providerGenerationGate) notifyLocked() {
	if g.changed != nil {
		close(g.changed)
		g.changed = nil
	}
}

func (g *providerGenerationGate) setLimit(limit uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.limit != limit {
		g.limit = limit
		g.notifyLocked()
	}
}

func (g *providerGenerationGate) acquire(ctx context.Context) (func(), error) {
	for {
		g.mu.Lock()
		if err := ctx.Err(); err != nil {
			g.mu.Unlock()
			return nil, err
		}
		if g.limit == 0 || uint64(g.active) < g.limit {
			g.active++
			g.mu.Unlock()
			return g.release, nil
		}
		if g.changed == nil {
			g.changed = make(chan struct{})
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (g *providerGenerationGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.limit == 0 || uint64(g.active) < g.limit {
		g.notifyLocked()
	}
}

func (m *Manager) generationGate(executor ProviderExecutor) *providerGenerationGate {
	switch executor.Identifier() {
	case "bps":
		return &m.bpsGenerations
	case "codex", "codex-websockets":
		return &m.codexGenerations
	default:
		return nil
	}
}

func (m *Manager) executeGeneration(executor ProviderExecutor, ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if gate := m.generationGate(executor); gate != nil {
		release, err := gate.acquire(ctx)
		if err != nil {
			return cliproxyexecutor.Response{}, err
		}
		defer release()
	}
	return executor.Execute(ctx, auth, req, opts)
}

// The stream consumer releases its slot after observing the original channel close.
// Keeping that channel intact preserves the bootstrap ordering of ready chunks and cancellation.
func (m *Manager) streamGeneration(executor ProviderExecutor, ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, func(), error) {
	var release func()
	if gate := m.generationGate(executor); gate != nil {
		var err error
		release, err = gate.acquire(ctx)
		if err != nil {
			return nil, nil, err
		}
	}
	result, err := executor.ExecuteStream(ctx, auth, req, opts)
	if release != nil && (result == nil || result.Chunks == nil) {
		release()
		release = nil
	}
	return result, release, err
}
