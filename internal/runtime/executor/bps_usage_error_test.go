package executor

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/bpsbridge"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestBPSExecutorUsagePreservesUpstreamErrors(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, kind := range []string{"error", "response.failed"} {
			for _, code := range []string{"rate_limit_exceeded", "invalid_request_error"} {
				t.Run(fmt.Sprintf("stream=%t/%s/%s", stream, kind, code), func(t *testing.T) {
					doc := ojson.NewDocString(`{}`)
					_ = doc.Set("type", kind)
					path := "error"
					if kind == "response.failed" {
						path = "response.error"
					}
					message := "Rate limit reached on tokens per min (TPM): Limit 40000000, Used 40000000, Requested 50905. Please try again in 77ms."
					if code == "invalid_request_error" {
						message = "Invalid image input: the reference image exceeds the upstream size limit."
					}
					_ = doc.Set(path, map[string]any{"code": code, "type": "tokens", "message": message, "param": "input",
						"headers": map[string]any{"x-ratelimit-limit-tokens": "40000000", "x-ratelimit-remaining-tokens": "0", "retry-after": "1", "set-cookie": "private-cookie"}})
					e, _ := newBPSFixture(t, []string{doc.String()}, nil)
					auth := bpsFixtureAuth()
					auth.ID = t.Name()
					capture := &bpsUsageCapture{authID: auth.ID, records: make(chan usage.Record, 2)}
					usage.RegisterNamedPlugin(t.Name(), capture)
					req, opts := bpsFixtureRequest(sdktranslator.FormatOpenAIResponse)
					var generationErr error
					var output bytes.Buffer
					if stream {
						result, err := e.ExecuteStream(t.Context(), auth, req, opts)
						generationErr = err
						if err == nil {
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									generationErr = chunk.Err
								}
								output.Write(chunk.Payload)
							}
						}
					} else {
						response, err := e.Execute(t.Context(), auth, req, opts)
						generationErr = err
						output.Write(response.Payload)
					}
					var public, expected *pluginabi.Error
					if !errors.As(generationErr, &public) || !errors.As(bpsbridge.EventError(doc.Bytes()), &expected) {
						t.Fatalf("missing public mapped error: %v", generationErr)
					}
					if public.HTTPStatus != expected.HTTPStatus || public.Message != expected.Message || public.Code != expected.Code || public.RetryAfterSeconds != expected.RetryAfterSeconds || public.Retryable != expected.Retryable || public.CredentialScoped != expected.CredentialScoped {
						t.Fatalf("public or scheduler error contract changed: got=%+v want=%+v", public, expected)
					}
					if bytes.Contains(output.Bytes(), []byte(message)) || bytes.Contains(output.Bytes(), []byte("private-cookie")) {
						t.Fatal("private diagnostic leaked into downstream response")
					}
					select {
					case record := <-capture.records:
						if !record.Failed || record.Fail.StatusCode != public.HTTPStatus || ojson.Get(record.Fail.Body, "diagnostic_source").String() != "bps_upstream" || ojson.Get(record.Fail.Body, "error.message").String() != message || ojson.Get(record.Fail.Body, "error.code").String() != code || ojson.Get(record.Fail.Body, "error.param").String() != "input" {
							t.Fatalf("usage lost upstream error: %+v", record.Fail)
						}
						if record.ResponseHeaders.Get("X-Ratelimit-Limit-Tokens") != "40000000" || record.ResponseHeaders.Get("Retry-After") != "1" || record.ResponseHeaders.Get("Set-Cookie") != "" {
							t.Fatalf("unexpected usage headers: %v", record.ResponseHeaders)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("failure usage was not published")
					}
				})
			}
		}
	}
}
