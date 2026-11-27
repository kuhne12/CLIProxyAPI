package auth

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestProviderConcurrencyAcrossCredentialsAndReload(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	const model = "provider-concurrency-model"
	registerInFlightAuth(t, manager, "bps-first", "bps", model)
	synctest.Test(t, func(t *testing.T) {
		set := func(bps, codex config.ProviderGenerationLimit) {
			manager.SetConfig(&config.Config{Routing: config.RoutingConfig{ProviderConcurrency: config.ProviderConcurrencyLimits{BPS: bps, Codex: codex}}})
		}
		set(1, 1)
		streamExec := &inFlightStreamExecutor{id: "bps", chunks: make(chan cliproxyexecutor.StreamChunk, 1)}
		manager.RegisterExecutor(streamExec)
		streamExec.chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: first\n\n")}
		streamCtx, cancelStream := context.WithCancel(t.Context())
		req, opts := cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}
		stream, err := manager.ExecuteStream(streamCtx, []string{"bps"}, req, opts)
		if err != nil {
			t.Fatal(err)
		}
		blocked := newInFlightBlockingExecutor("bps")
		defer close(blocked.release)
		run := func(ctx context.Context, e *inFlightBlockingExecutor, id string) <-chan error {
			done := make(chan error, 1)
			go func() { _, err := manager.executeGeneration(e, ctx, &Auth{ID: id}, req, opts); done <- err }()
			return done
		}
		assertWaiting := func(e *inFlightBlockingExecutor) {
			t.Helper()
			synctest.Wait()
			select {
			case <-e.entered:
				t.Fatal("upstream entered while full")
			default:
			}
		}
		pending, cancelPending := context.WithCancel(t.Context())
		firstWait := run(pending, blocked, "bps-second")
		assertWaiting(blocked)
		cancelPending()
		if err := <-firstWait; !errors.Is(err, context.Canceled) {
			t.Fatalf("wait cancellation = %v", err)
		}
		other := newInFlightBlockingExecutor("codex")
		otherDone := run(t.Context(), other, "codex-first")
		<-other.entered
		close(other.release)
		if err := <-otherDone; err != nil {
			t.Fatal(err)
		}
		run(t.Context(), blocked, "bps-third")
		assertWaiting(blocked)
		cancelStream()
		assertWaiting(blocked)
		set(2, 1)
		<-blocked.entered
		set(1, 1)
		run(t.Context(), blocked, "bps-fourth")
		assertWaiting(blocked)
		close(streamExec.chunks)
		for range stream.Chunks {
		}
		assertWaiting(blocked)
		set(2, 1)
		<-blocked.entered
		run(t.Context(), blocked, "bps-fifth")
		assertWaiting(blocked)
		set(0, 1)
		<-blocked.entered
	})
}

func TestCredentialConcurrencyReloadAndIsolation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil, nil, nil)
		auth := &Auth{ID: "file-a", Provider: "bps", Metadata: map[string]any{"max_concurrency": 1, "email": "a@example.test"}}
		m.auths[auth.ID] = auth.Clone()
		acquire := func(a *Auth) func() {
			t.Helper()
			release, err := m.acquireGeneration(t.Context(), "bps", a)
			if err != nil {
				t.Fatal(err)
			}
			return release
		}
		first := acquire(auth)
		other := acquire(&Auth{ID: "file-b", Provider: "bps"})
		other()
		change := func(n int) {
			m.mu.Lock()
			m.auths[auth.ID].Metadata["max_concurrency"] = n
			m.notifyAuthChangeLocked(auth.ID)
			m.mu.Unlock()
		}
		pending := make(chan func(), 1)
		go func() {
			release, err := m.acquireGeneration(t.Context(), "bps", auth)
			if err == nil {
				pending <- release
			}
		}()
		synctest.Wait()
		select {
		case <-pending:
			t.Fatal("credential cap exceeded")
		default:
		}
		row := m.CredentialConcurrency()[0]
		if row.Current != 1 || row.Limit != 1 || row.Email != "a@example.test" {
			t.Fatalf("unexpected snapshot: %+v", row)
		}
		change(2)
		second := <-pending
		change(1)
		if row = m.CredentialConcurrency()[0]; row.Current != 2 || row.Limit != 1 {
			t.Fatalf("reduction interrupted a request: %+v", row)
		}
		cancelled, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { _, err := m.acquireGeneration(cancelled, "bps", auth); done <- err }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		second()
		first()
		first()
		if row = m.CredentialConcurrency()[0]; row.Current != 0 {
			t.Fatalf("leaked slot: %+v", row)
		}
		if len(m.generations.active)+len(m.generations.waiting)+len(m.generations.providers) != 0 {
			t.Fatal("leaked counters")
		}
	})
}
