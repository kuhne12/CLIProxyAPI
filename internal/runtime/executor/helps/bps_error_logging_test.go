package helps

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/bpsbridge"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestBPSUpstreamErrorLogPreservesOriginalLimits(t *testing.T) {
	logger := log.StandardLogger()
	previousHooks, previousLevel, previousOutput := logger.Hooks, logger.GetLevel(), logger.Out
	logger.ReplaceHooks(make(log.LevelHooks))
	hook := logtest.NewGlobal()
	var output bytes.Buffer
	logger.SetOutput(&output)
	logger.SetLevel(log.WarnLevel)
	t.Cleanup(func() {
		logger.ReplaceHooks(previousHooks)
		logger.SetLevel(previousLevel)
		logger.SetOutput(previousOutput)
	})
	for _, kind := range []string{"error", "response.failed"} {
		for _, code := range []string{"rate_limit_exceeded", "too_many_requests", "concurrent_request_limit"} {
			t.Run(kind+"/"+code, func(t *testing.T) {
				hook.Reset()
				message := strings.Repeat("provider capacity detail ", 200) + "end-of-original-message"
				detail := map[string]any{"code": code, "type": "rate_limit_error", "message": message,
					"retry_after_seconds": 17, "limits": map[string]any{"concurrent": 4, "active": 5}}
				event := map[string]any{"type": kind, "sequence_number": 54, "request_id": "upstream-fixture"}
				path := "error"
				if kind == "error" {
					event["error"] = detail
				} else {
					event["response"] = map[string]any{"id": "response-fixture", "error": detail, "output": []any{"private response"}}
					path = "response.error"
				}
				raw, errMarshal := json.Marshal(event)
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				before := bytes.Clone(raw)
				ctx := logging.WithRequestID(t.Context(), "local-fixture-request")
				observation := NewBPSObservation(123)
				observation.RecordToolRecovery("started")
				observation.ObserverWithErrorLogging(ctx).Event(raw)
				entries := hook.AllEntries()
				if len(entries) != 1 {
					t.Fatalf("expected one error log, got %d", len(entries))
				}
				entry := entries[0]
				if entry.Message != "BPS upstream error" || entry.Level != log.WarnLevel || entry.Data["request_id"] != "local-fixture-request" || entry.Data["upstream_error_code"] != code {
					t.Fatalf("missing original error identity: %v", entry.Data)
				}
				if entry.Data["upstream_error_bytes"] != len(raw) || entry.Data["upstream_error_sha256"] != bpsbridge.DiagnosticHash(string(raw)) || entry.Data["tool_recovery_attempts"] != 1 {
					t.Fatal("missing original size, hash or recovery correlation")
				}
				payload := entry.Data["upstream_error_payload"].(string)
				if ojson.Get(payload, path+".message").String() != message || ojson.Get(payload, path+".retry_after_seconds").Int() != 17 || ojson.Get(payload, path+".limits.active").Int() != 5 {
					t.Fatal("original limit details were truncated or discarded")
				}
				if strings.Contains(payload, "private response") || !bytes.Equal(raw, before) {
					t.Fatal("private output leaked or source event was mutated")
				}
				var public *pluginabi.Error
				if !errors.As(bpsbridge.EventError(raw), &public) || public.HTTPStatus != 429 || public.Code != pluginabi.ErrorCodeTransientRateLimit || public.Message != "Service is temporarily rate limited; retry later" || strings.Contains(public.Message, message) {
					t.Fatalf("public rate-limit contract changed: %v", public)
				}
				if _, retained := observation.fields()["upstream_error_payload"]; retained {
					t.Fatal("full error retained for duplicate timing log")
				}
			})
		}
	}
	hook.Reset()
	NewBPSObservation(0).ObserverWithErrorLogging(t.Context()).Event([]byte("{\"type\":\"response.completed\",\"response\":{\"output\":[\"private output\"]}}"))
	if len(hook.AllEntries()) != 0 {
		t.Fatal("successful response contents were logged")
	}
}

func TestBPSErrorRedactionPreservesDetailsAndSource(t *testing.T) {
	message := strings.Repeat("capacity detail ", 100) + "\nAuthorization: Bearer credential-one; api_key=credential-two; https://user:credential-three@example.test/ token=credential-four; opaque-credential-five"
	event := map[string]any{
		"type": "error", "basispoints_replay_cursor": "cursor-private",
		"error": map[string]any{"message": message, "limit": 4, "retry_after_seconds": 19,
			"headers":         map[string]any{"Authorization": "credential-six", "Set-Cookie": "cookie-private", "Retry-After": "19"},
			"details":         []any{map[string]any{"access_token": "credential-seven", "encrypted_content": "checkpoint-private", "arguments": "code-private", "input": "prompt-private", "output": "output-private", "count": 5}},
			"detail.with.dot": map[string]any{"api_key": "credential-eight", "count": 6},
		},
	}
	raw, errMarshal := json.Marshal(event)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	before := bytes.Clone(raw)
	sanitized, redacted := redactBPSErrorEvent(raw, []string{"opaque-credential-five"})
	if !redacted || !bytes.Equal(raw, before) || !json.Valid(sanitized) {
		t.Fatal("redaction must preserve the source and produce valid JSON")
	}
	for _, secret := range []string{"credential-one", "credential-two", "credential-three", "credential-four", "opaque-credential-five", "credential-six", "credential-seven", "credential-eight", "cursor-private", "cookie-private", "checkpoint-private", "code-private", "prompt-private", "output-private"} {
		if bytes.Contains(sanitized, []byte(secret)) {
			t.Fatalf("diagnostic retained private value %q", secret)
		}
	}
	if !strings.HasPrefix(ojson.GetBytes(sanitized, "error.message").String(), strings.Repeat("capacity detail ", 100)) || ojson.GetBytes(sanitized, "error.details.0.count").Int() != 5 || ojson.GetBytes(sanitized, "error.detail\\.with\\.dot.count").Int() != 6 || ojson.GetBytes(sanitized, "error.headers.Retry-After").String() != "19" {
		t.Fatalf("non-secret details were lost: %s", sanitized)
	}
}

func TestBPSErrorRedactionFailsClosedOnAmbiguousJSON(t *testing.T) {
	for _, raw := range []string{
		"{\"type\":\"error\",\"error\":{\"token\":\"first-secret\",\"token\":\"second-secret\"}}",
		"{\"type\":\"error\",\"error\":{\"\":{\"token\":\"secret\"}}}",
		"{\"type\":\"error\",\"error\":{\"token\":\"secret\"}",
	} {
		sanitized, redacted := redactBPSErrorEvent([]byte(raw), nil)
		if !redacted || !ojson.GetBytes(sanitized, "logging_error").Exists() || bytes.Contains(sanitized, []byte("secret")) {
			t.Fatalf("ambiguous input did not fail closed: %s", sanitized)
		}
	}
}

func TestBPSErrorRedactionHandlesLiteralObjectKeys(t *testing.T) {
	for _, key := range []string{"a\\b", "a.b", "a:b", "0", "#", "*", "?", "@this", "!x", "a|@this", "-1", "#(token)", "a[0]"} {
		t.Run(key, func(t *testing.T) {
			raw, errMarshal := json.Marshal(map[string]any{"type": "error", "error": map[string]any{key: map[string]any{"token": "private-fixture", "count": 4}}})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			payload, redacted := redactBPSErrorEvent(raw, nil)
			if !redacted || bytes.Contains(payload, []byte("private-fixture")) || ojson.GetBytes(payload, "logging_error").Exists() {
				t.Fatalf("literal object key was not redacted safely: %s", payload)
			}
		})
	}
}
