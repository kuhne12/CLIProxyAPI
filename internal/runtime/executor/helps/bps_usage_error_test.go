package helps

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestBPSUsageFailureDiagnosticAllowlist(t *testing.T) {
	for _, kind := range []string{"error", "response.failed"} {
		t.Run(kind, func(t *testing.T) {
			doc := ojson.NewDocString(`{"headers":{"x-request-id":"upstream-request-fixture","cookie":"private-cookie"}}`)
			_ = doc.Set("type", kind)
			path := "error"
			if kind == "response.failed" {
				path = "response.error"
			}
			message := "Rate limit reached on tokens per min (TPM): Limit 40000000, Used 40000000, Requested 50905. Please try again in 77ms."
			_ = doc.Set(path, map[string]any{"code": "rate_limit_exceeded", "type": "tokens", "message": message, "param": nil,
				"input": "private-prompt", "output": "private-output", "details": map[string]any{"count": 4},
				"headers": map[string]any{"retry-after": "1", "x-ratelimit-limit-requests": 15000, "x-ratelimit-remaining-requests": "14999", "x-ratelimit-reset-requests": "4ms",
					"x-ratelimit-limit-tokens": "40000000", "x-ratelimit-remaining-tokens": "0", "x-ratelimit-reset-tokens": []string{"1m0.071s"},
					"authorization": "private-authorization", "set-cookie": "private-cookie", "x-unrecognized": "private-custom-header"},
			})
			payload, _ := redactBPSErrorEvent(doc.Bytes(), nil)
			body, headers := bpsUsageFailureDiagnostic(payload)
			if !json.Valid([]byte(body)) || ojson.Get(body, "diagnostic_source").String() != "bps_upstream" || ojson.Get(body, "error.message").String() != message || ojson.Get(body, "error.code").String() != "rate_limit_exceeded" || ojson.Get(body, "error.type").String() != "tokens" {
				t.Fatalf("upstream diagnostic lost useful fields: %s", body)
			}
			if ojson.Get(body, "error.param").Raw != "null" || len(ojson.Get(body, "error").Map()) != 4 {
				t.Fatalf("error object escaped allowlist: %s", body)
			}
			if len(headers) != 8 || headers.Get("X-Request-Id") != "upstream-request-fixture" || headers.Get("Retry-After") != "1" || headers.Get("X-Ratelimit-Limit-Requests") != "15000" || headers.Get("X-Ratelimit-Limit-Tokens") != "40000000" || headers.Get("X-Ratelimit-Reset-Tokens") != "1m0.071s" {
				t.Fatalf("incorrect diagnostic headers: %v", headers)
			}
			if strings.Contains(body+fmt.Sprint(headers), "private-") {
				t.Fatal("private event fields leaked to usage")
			}
		})
	}
}

func TestBPSUsageFailureDiagnosticRedactsMessageAndHeaders(t *testing.T) {
	message := "Rejected content for account fixture@example.test. https://example.test/diagnostic?arbitrary=query-private#fragment-private\n" +
		"Bearer bearer-private\napi_key=api-private\nopaque-token-private\nCookie: session=cookie-private; xsrf=xsrf-private\n" +
		"prompt: prompt-private\noutput=output-private\narguments: {\"code\":\"argument-private\"}\ninstructions: instruction-private"
	doc := ojson.NewDocString(`{"type":"error","error":{"code":"invalid_request","type":"invalid_request_error","param":"input"}}`)
	_ = doc.Set("error.message", message)
	_ = doc.Set("error.headers", map[string]any{"x-request-id": "opaque-token-private", "retry-after": "1\r\nCookie: header-private", "set-cookie": "cookie-header-private"})
	payload, _ := redactBPSErrorEvent(doc.Bytes(), []string{"opaque-token-private"})
	body, headers := bpsUsageFailureDiagnostic(payload)
	all := body + fmt.Sprint(headers)
	for _, private := range []string{"fixture@example.test", "query-private", "fragment-private", "bearer-private", "api-private", "opaque-token-private", "cookie-private", "xsrf-private", "prompt-private", "output-private", "argument-private", "instruction-private", "header-private", "cookie-header-private"} {
		if strings.Contains(all, private) {
			t.Fatalf("private value %q leaked to diagnostic", private)
		}
	}
	if !strings.Contains(ojson.Get(body, "error.message").String(), "Rejected content") || ojson.Get(body, "error.param").String() != "input" {
		t.Fatal("diagnostic lost useful error context")
	}
	for _, values := range headers {
		for _, value := range values {
			if strings.ContainsAny(value, "\r\n") {
				t.Fatal("diagnostic header retained a line break")
			}
		}
	}
}

func TestBPSUsageFailureDiagnosticBoundedJSON(t *testing.T) {
	for _, fragment := range []string{"limit ", "中文限制🙂", "\x01\n\r\t\"\\<>&\u2028\u2029", string([]byte{0xff, 0xfe})} {
		doc := ojson.NewDocString(`{"type":"error","error":{}}`)
		for _, field := range []string{"code", "type", "message", "param"} {
			_ = doc.Set("error."+field, strings.Repeat(fragment, 5000))
		}
		payload, _ := redactBPSErrorEvent(doc.Bytes(), nil)
		body, _ := bpsUsageFailureDiagnostic(payload)
		if body == "" || len(body) >= maxUsageFailureDiagnosticBytes || !json.Valid([]byte(body)) || !utf8.ValidString(body) {
			t.Fatalf("oversized/invalid diagnostic: bytes=%d validJSON=%t validUTF8=%t", len(body), json.Valid([]byte(body)), utf8.ValidString(body))
		}
		if !strings.HasSuffix(ojson.Get(body, "error.message").String(), "…") {
			t.Fatal("long message does not indicate truncation")
		}
	}
}

func TestBPSUsageFailureDiagnosticIgnoresNonScalarAndMalformedErrors(t *testing.T) {
	for _, raw := range []string{
		`{"type":"error","error":{"code":{"secret":"private-value"},"message":["private-value"]}}`,
		`{"type":"error","error":{"message":"first","message":"private-value"}}`,
		`{"type":"error","error":`,
	} {
		payload, _ := redactBPSErrorEvent([]byte(raw), nil)
		if body, headers := bpsUsageFailureDiagnostic(payload); body != "" || len(headers) != 0 {
			t.Fatal("ambiguous diagnostic did not fail closed")
		}
	}
}

func TestUsageFailureDiagnosticDoesNotChangeSuccessOrClassification(t *testing.T) {
	reporter := NewUsageReporter(t.Context(), "bps", "fixture-model", nil)
	headers := http.Header{"Retry-After": []string{"1"}}
	body := `{"diagnostic_source":"bps_upstream","error":{"code":"rate_limit_exceeded"}}`
	reporter.SetFailureDiagnostic(body, headers)
	headers.Set("Retry-After", "mutated")
	failure := usage.Record{Failed: true, Fail: usage.Failure{StatusCode: 429, Code: "unchanged-classification", Body: "generic"}, ResponseHeaders: http.Header{"X-Fixture": []string{"preserved"}}}
	reporter.applyFailureDiagnostic(&failure)
	if failure.Fail.Body != body || failure.Fail.StatusCode != 429 || failure.Fail.Code != "unchanged-classification" || failure.ResponseHeaders.Get("Retry-After") != "1" || failure.ResponseHeaders.Get("X-Fixture") != "preserved" {
		t.Fatalf("unexpected failure override: %+v", failure)
	}
	failure.ResponseHeaders.Set("Retry-After", "snapshot-mutated")
	next := usage.Record{Failed: true}
	reporter.applyFailureDiagnostic(&next)
	if next.ResponseHeaders.Get("Retry-After") != "1" {
		t.Fatal("published header mutated reporter state")
	}
	success := usage.Record{}
	reporter.applyFailureDiagnostic(&success)
	if success.Fail.Body != "" || len(success.ResponseHeaders) != 0 {
		t.Fatal("success inherited an earlier diagnostic")
	}
	for _, invalid := range []string{"", "invalid JSON", strings.Repeat("x", maxUsageFailureDiagnosticBytes)} {
		reporter.SetFailureDiagnostic(invalid, headers)
		failure = usage.Record{Failed: true, Fail: usage.Failure{Body: "generic"}}
		reporter.applyFailureDiagnostic(&failure)
		if failure.Fail.Body != "generic" || len(failure.ResponseHeaders) != 0 {
			t.Fatal("empty or invalid diagnostic failed to clear previous state")
		}
	}
}

func TestUsageFailureDiagnosticConcurrentAttemptIsolation(t *testing.T) {
	var workers sync.WaitGroup
	for _, attempt := range []string{"attempt-one", "attempt-two"} {
		reporter := NewUsageReporter(t.Context(), "bps", attempt, nil)
		body := `{"error":{"message":"` + attempt + `"}}`
		reporter.SetFailureDiagnostic(body, http.Header{"X-Request-Id": []string{attempt}})
		for worker := 0; worker < 4; worker++ {
			workers.Go(func() {
				for iteration := 0; iteration < 100; iteration++ {
					reporter.SetFailureDiagnostic(body, http.Header{"X-Request-Id": []string{attempt}})
					record := usage.Record{Failed: true}
					reporter.applyFailureDiagnostic(&record)
					if record.Fail.Body != body || record.ResponseHeaders.Get("X-Request-Id") != attempt {
						t.Errorf("attempt diagnostic was contaminated: %+v", record)
						return
					}
				}
			})
		}
	}
	workers.Wait()
}
