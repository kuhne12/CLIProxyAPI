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
	synctest.Test(t, func(t *testing.T) {
		manager := NewManager(nil, nil, nil)
		set := func(bps, codex config.ProviderGenerationLimit) {
			manager.SetConfig(&config.Config{Routing: config.RoutingConfig{ProviderConcurrency: config.ProviderConcurrencyLimits{BPS: bps, Codex: codex}}})
		}
		set(1, 1)
		streamExec := &inFlightStreamExecutor{id: "bps", chunks: make(chan cliproxyexecutor.StreamChunk)}
		streamCtx, cancelStream := context.WithCancel(t.Context())
		req, opts := cliproxyexecutor.Request{}, cliproxyexecutor.Options{}
		stream, err := manager.streamGeneration(streamExec, streamCtx, &Auth{ID: "bps-first"}, req, opts)
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
		set(2, 1)
		<-blocked.entered
		set(1, 1)
		run(t.Context(), blocked, "bps-fourth")
		cancelStream()
		assertWaiting(blocked)
		close(streamExec.chunks)
		for range stream.Chunks {
		}
		assertWaiting(blocked)
		set(0, 1)
		<-blocked.entered
	})
}
