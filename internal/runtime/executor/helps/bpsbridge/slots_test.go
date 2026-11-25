package bpsbridge

import (
	"context"
	"errors"
	"testing"
)

func TestBPSSlotsIsolateAccountsAndReleaseOnCancellation(t *testing.T) {
	var pool SlotPool
	var release []func()
	tasks := map[string]bool{}
	for i := 0; i < 4; i++ {
		options, done, err := pool.Acquire(t.Context(), "account-a")
		if err != nil {
			t.Fatal(err)
		}
		if tasks[options.UpstreamTaskID] {
			t.Fatal("concurrent tasks share a slot")
		}
		tasks[options.UpstreamTaskID] = true
		release = append(release, done)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := pool.Acquire(ctx, "account-a"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled queue wait did not stop")
	}
	other, done, err := pool.Acquire(t.Context(), "account-b")
	if err != nil || tasks[other.UpstreamTaskID] {
		t.Fatal("accounts share task identities")
	}
	done()
	for _, done := range release {
		done()
		done()
	}
	next, done, err := pool.Acquire(t.Context(), "account-a")
	if err != nil || !tasks[next.UpstreamTaskID] {
		t.Fatal("slot identity changed after reuse")
	}
	done()
	if len(pool.accounts) != 0 {
		t.Fatal("idle account state leaked")
	}
}
