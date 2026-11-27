package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// All counters and the change signal are protected by Manager.mu. Admission takes
// both limits together so waiters for a busy credential cannot occupy provider slots.
type generationAdmission struct {
	active    map[string]int
	providers map[string]int
	waiting   map[string]int
	changed   chan struct{}
}

func (g *generationAdmission) notifyLocked() {
	if g.changed != nil {
		close(g.changed)
		g.changed = nil
	}
}

// BPSAccountSlots is the number of stable upstream task identities per BPS account.
const BPSAccountSlots = 4

func credentialLimit(auth *Auth, provider string) int {
	var limit int
	if auth != nil {
		switch value := auth.Metadata["max_concurrency"].(type) {
		case int:
			limit = value
		case int64:
			limit = int(value)
		case float64:
			if value == float64(int(value)) {
				limit = int(value)
			}
		case json.Number:
			parsed, err := strconv.Atoi(value.String())
			if err == nil {
				limit = parsed
			}
		}
	}
	if provider == "bps" && (limit <= 0 || limit > BPSAccountSlots) {
		return BPSAccountSlots
	}
	return max(0, limit)
}

func providerLimit(cfg *config.Config, provider string) uint64 {
	if cfg == nil {
		return 0
	}
	switch provider {
	case "bps":
		return uint64(cfg.Routing.ProviderConcurrency.BPS)
	case "codex":
		return uint64(cfg.Routing.ProviderConcurrency.Codex)
	}
	return 0
}

func generationProvider(provider string) string {
	if provider == "codex-websockets" {
		return "codex"
	}
	return provider
}

func (m *Manager) acquireGeneration(ctx context.Context, provider string, auth *Auth) (func(), error) {
	provider = generationProvider(provider)
	id := ""
	if auth != nil {
		id = auth.ID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	g := &m.generations
	if g.active == nil {
		g.active = make(map[string]int)
		g.providers = make(map[string]int)
		g.waiting = make(map[string]int)
	}
	queued := false
	defer func() {
		if queued {
			decrementGenerationCount(g.waiting, id)
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		current := auth
		if registered := m.auths[id]; registered != nil {
			current = registered
		}
		fileLimit := credentialLimit(current, provider)
		cfg, _ := m.runtimeConfig.Load().(*config.Config)
		channelLimit := providerLimit(cfg, provider)
		fileFull := fileLimit > 0 && g.active[id] >= fileLimit
		if !fileFull && (channelLimit == 0 || uint64(g.providers[provider]) < channelLimit) {
			g.active[id]++
			g.providers[provider]++
			return sync.OnceFunc(func() {
				m.mu.Lock()
				defer m.mu.Unlock()
				decrementGenerationCount(g.active, id)
				decrementGenerationCount(g.providers, provider)
				g.notifyLocked()
			}), nil
		}
		if !queued {
			if provider == "bps" && fileFull && g.waiting[id] >= 64 {
				return nil, &Error{Code: ErrorCodeTransientRateLimit, Message: "Upstream request queue is full", HTTPStatus: http.StatusTooManyRequests, Retryable: true}
			}
			g.waiting[id]++
			queued = true
		}
		if g.changed == nil {
			g.changed = make(chan struct{})
		}
		changed := g.changed
		m.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		m.mu.Lock()
	}
}

func decrementGenerationCount(counts map[string]int, key string) {
	counts[key]--
	if counts[key] == 0 {
		delete(counts, key)
	}
}

// CredentialConcurrency reports only fields intended for authenticated management views.
type CredentialConcurrency struct {
	ID      string `json:"id"`
	Index   string `json:"auth_index"`
	Current int    `json:"current"`
	Limit   int    `json:"limit"`
	Email   string `json:"email,omitempty"`
}

func (m *Manager) CredentialConcurrency() []CredentialConcurrency {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rows := make([]CredentialConcurrency, 0, len(m.auths))
	for id, auth := range m.auths {
		if auth == nil {
			continue
		}
		email, _ := auth.Metadata["email"].(string)
		if email == "" {
			email = auth.Attributes["email"]
		}
		if email == "" {
			email = auth.Attributes["account_email"]
		}
		if email == "" {
			if kind, account := auth.AccountInfo(); kind == "oauth" {
				email = account
			}
		}
		rows = append(rows, CredentialConcurrency{ID: id, Index: auth.Index, Current: m.generations.active[id], Limit: credentialLimit(auth, auth.Provider), Email: strings.TrimSpace(email)})
	}
	return rows
}

func (m *Manager) executeGeneration(executor ProviderExecutor, ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	release, err := m.acquireGeneration(ctx, executor.Identifier(), auth)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer release()
	return executor.Execute(ctx, auth, req, opts)
}

// The stream consumer releases its slot after observing the original channel close.
// Keeping that channel intact preserves the bootstrap ordering of ready chunks and cancellation.
func (m *Manager) streamGeneration(executor ProviderExecutor, ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, func(), error) {
	release, err := m.acquireGeneration(ctx, executor.Identifier(), auth)
	if err != nil {
		return nil, nil, err
	}
	result, err := executor.ExecuteStream(ctx, auth, req, opts)
	if release != nil && (result == nil || result.Chunks == nil) {
		release()
		release = nil
	}
	return result, release, err
}
