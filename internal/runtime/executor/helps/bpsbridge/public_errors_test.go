package bpsbridge

import (
	"errors"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func TestEventErrorUsesProviderNeutralMessages(t *testing.T) {
	cases := []struct {
		codes      []string
		status     int
		code       string
		message    string
		credential bool
		retryAfter int
	}{
		{[]string{"invalid_encrypted_content"}, 400, pluginabi.ErrorCodeRequestScoped, "Encrypted conversation content could not be validated; start a new delegation with valid history", false, 0},
		{[]string{"rate_limit_exceeded"}, 429, pluginabi.ErrorCodeBPSRateLimit, "Service is temporarily rate limited; retry later", false, 1},
		{[]string{"too_many_requests", "concurrent_request_limit"}, 429, pluginabi.ErrorCodeTransientRateLimit, "Service is temporarily rate limited; retry later", false, 1},
		{[]string{"insufficient_quota", "usage_limit_reached", "quota_exceeded"}, 429, pluginabi.ErrorCodeCredentialQuota, "Upstream account quota is exhausted", true, 0},
		{[]string{"invalid_api_key", "invalid_token", "token_expired", "unauthorized"}, 401, "bps_auth_unavailable", "Upstream authorization is no longer valid; refresh or sign in again", false, 0},
		{[]string{"model_not_found", "model_not_available", "unsupported_model"}, 404, pluginabi.ErrorCodeModelNotFound, "This model is unavailable for the configured credential", false, 0},
		{[]string{"server_error", "internal_error"}, 502, pluginabi.ErrorCodeStreamTruncated, "Upstream failed after accepting the request; generation was not replayed", false, 0},
		{[]string{"basispoints_protocol_error"}, 502, pluginabi.ErrorCodeStreamTruncated, "Upstream returned an unsupported response or tool call", false, 0},
		{[]string{"unknown_code", ""}, 400, pluginabi.ErrorCodeRequestScoped, "Upstream rejected the generation request", false, 0},
	}
	for _, tc := range cases {
		for _, code := range tc.codes {
			t.Run(code, func(t *testing.T) {
				raw, errMarshal := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"code": code, "message": "BPS private provider detail and token"}})
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				var failure *pluginabi.Error
				if !errors.As(EventError(raw), &failure) {
					t.Fatal("missing structured public error")
				}
				if failure.HTTPStatus != tc.status || failure.Code != tc.code || failure.Message != tc.message || failure.CredentialScoped != tc.credential || failure.RetryAfterSeconds != tc.retryAfter || failure.Retryable {
					t.Fatalf("public classification changed: %#v", failure)
				}
				for _, private := range []string{"bps", "basispoints", "private", "token"} {
					if strings.Contains(strings.ToLower(failure.Message), private) {
						t.Fatalf("public message disclosed provider detail: %s", failure.Message)
					}
				}
			})
		}
	}
}
