package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/bpsconfig"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func TestBPSAccountRateLimitCooldown(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.bpsRateLimitPolicy = bpsconfig.RateLimitCooldown{Enabled: true, Seconds: 60, Threshold: 2}
	account := &Auth{ID: "bps-a", Provider: "bps", Status: StatusActive}
	other := &Auth{ID: "bps-b", Provider: "bps", Status: StatusActive}
	m.auths[account.ID], m.auths[other.ID] = account, other
	failure := resultErrorFromError(&pluginabi.Error{Code: pluginabi.ErrorCodeBPSRateLimit, HTTPStatus: http.StatusTooManyRequests})
	if failure.Code != pluginabi.ErrorCodeBPSRateLimit || !isTransientRateLimitResultError(failure) {
		t.Fatalf("classification = %+v", failure)
	}
	ctx := context.Background()
	m.MarkResult(ctx, Result{AuthID: account.ID, Provider: "bps", Model: "first", Error: failure})
	m.MarkResult(ctx, Result{AuthID: other.ID, Provider: "bps", Model: "first", Error: failure})
	m.MarkResult(ctx, Result{AuthID: account.ID, Provider: "bps", Model: "second", Success: true})
	m.MarkResult(ctx, Result{AuthID: account.ID, Provider: "bps", Model: "first", Error: failure})
	if !account.bpsRateLimit.until.IsZero() || !other.bpsRateLimit.until.IsZero() {
		t.Fatal("cooled below consecutive threshold")
	}
	m.MarkResult(ctx, Result{AuthID: account.ID, Provider: "bps", Model: "second", Error: failure})
	until := account.bpsRateLimit.until
	if got := time.Until(until); got <= 59*time.Second || got > time.Minute {
		t.Fatalf("cooldown duration = %s", got)
	}
	if blocked, reason, deadline := isAuthBlockedForModel(account, "sibling", until.Add(-time.Second)); !blocked || reason != blockReasonCooldown || !deadline.Equal(until) {
		t.Fatalf("sibling model block = %v %v %v", blocked, reason, deadline)
	}
	if blocked, _, _ := isAuthBlockedForModel(other, "sibling", time.Now()); blocked {
		t.Fatal("another account is blocked")
	}
	m.MarkResult(ctx, Result{AuthID: account.ID, Provider: "bps", Model: "second", Success: true})
	if !account.bpsRateLimit.until.Equal(until) {
		t.Fatal("an in-flight success ended the active cooldown")
	}
	if blocked, _, _ := isAuthBlockedForModel(account, "sibling", until); blocked {
		t.Fatal("expired cooldown still blocks sibling model")
	}
	for _, row := range m.CredentialConcurrency() {
		if row.ID == account.ID && (row.CooldownUntil == nil || !row.CooldownUntil.Equal(until)) {
			t.Fatalf("management cooldown = %v", row.CooldownUntil)
		}
	}
	m.SetConfig(&config.Config{})
	if !account.bpsRateLimit.until.IsZero() {
		t.Fatal("disabling the setting did not clear active cooldown")
	}
}

func TestBPSAccountRateLimitIgnoresOther429s(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.bpsRateLimitPolicy = bpsconfig.RateLimitCooldown{Enabled: true, Seconds: 60, Threshold: 2}
	a := &Auth{Provider: "bps"}
	now := time.Now()
	rateLimit := &Error{Code: pluginabi.ErrorCodeBPSRateLimit, HTTPStatus: http.StatusTooManyRequests}
	m.recordBPSRateLimitLocked(a, Result{Error: rateLimit}, now)
	m.recordBPSRateLimitLocked(a, Result{Error: &Error{Code: pluginabi.ErrorCodeTransientRateLimit, HTTPStatus: http.StatusTooManyRequests}}, now)
	if m.recordBPSRateLimitLocked(a, Result{Error: rateLimit}, now) {
		t.Fatal("a different 429 joined the upstream streak")
	}
	m.recordBPSRateLimitLocked(a, Result{Error: rateLimit, SkipQuotaObservation: true}, now)
	if a.bpsRateLimit.consecutive != 1 {
		t.Fatalf("streak = %d", a.bpsRateLimit.consecutive)
	}
}
