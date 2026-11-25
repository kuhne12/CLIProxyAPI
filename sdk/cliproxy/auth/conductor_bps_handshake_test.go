package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/bpsbridge"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func TestManagerBPSHandshakeCooldownUsesRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "120"); w.WriteHeader(429) }))
	defer server.Close()
	_, errOpen := bpsbridge.Open(t.Context(), server.Client(), server.URL, "fixture-token", []byte("{}"))
	var failure *pluginabi.Error
	if !errors.As(errOpen, &failure) || failure.Code != "bps_upstream_rejected" {
		t.Fatalf("unexpected rejection: %v", errOpen)
	}
	hint := retryAfterFromError(errOpen)
	if hint == nil || *hint != 120*time.Second {
		t.Fatalf("scheduler hint=%v", hint)
	}
	if got := SafeResponseHeaders(errOpen).Get("Retry-After"); got != "120" {
		t.Fatalf("client Retry-After=%q, want 120", got)
	}
	m := NewManager(nil, nil, nil)
	credential := &Auth{ID: "bps-handshake-cooldown", Provider: "bps", Status: StatusActive}
	if _, errRegister := m.Register(WithSkipPersist(t.Context()), credential); errRegister != nil {
		t.Fatal(errRegister)
	}
	before := time.Now()
	m.MarkResult(t.Context(), Result{AuthID: credential.ID, Provider: "bps", Model: "gpt-6-astra", Error: resultErrorFromError(errOpen), RetryAfter: hint})
	after, _ := m.GetByID(credential.ID)
	state := after.ModelStates["gpt-6-astra"]
	if state == nil || state.NextRetryAfter.Before(before.Add(120*time.Second)) {
		t.Fatalf("missing upstream cooldown: %+v", state)
	}
}

func TestManagerBPSHandshakeBudgetDoesNotMultiply(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(5, 0, 0)
	stopped := &pluginabi.Error{Code: pluginabi.ErrorCodeRequestScoped, HTTPStatus: 502, Message: "BPS WebSocket connection failed before sending the request (network_eof; attempts=3)"}
	if !isRequestInvalidError(stopped) || !isRequestScopedResultError(resultErrorFromError(stopped)) {
		t.Fatal("bounded handshake failure lost its stop marker")
	}
	if wait, retry := m.shouldRetryAfterError(stopped, 0, []string{"bps"}, "gpt-6-astra", time.Minute); retry {
		t.Fatalf("outer retry would multiply transport attempts: wait=%s", wait)
	}
}
