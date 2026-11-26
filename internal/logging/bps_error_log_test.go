package logging

import (
	"strconv"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

func TestLogFormatterPreservesAndEscapesCompleteUpstreamError(t *testing.T) {
	payload := "{\"message\":\"" + strings.Repeat("complete detail ", 300) + "\"}\nforged log line\r\x1b[31m"
	entry := log.NewEntry(log.New())
	entry.Level = log.WarnLevel
	entry.Message = "BPS upstream error"
	entry.Data = log.Fields{"provider": "bps", "request_id": "0123456789", "upstream_error_event": "error", "upstream_error_payload": payload, "upstream_error_code": "concurrent_request_limit\nforged", "upstream_error_type": "rate_limit_error", "upstream_error_bytes": 8192, "upstream_error_sha256": "fixture-hash", "upstream_error_redacted": true}
	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatal(errFormat)
	}
	line := string(formatted)
	for _, want := range []string{"upstream_error_payload=" + strconv.Quote(payload), "upstream_error_code=" + strconv.Quote("concurrent_request_limit\nforged"), "upstream_error_bytes=8192", "upstream_error_sha256=fixture-hash", "upstream_error_redacted=true"} {
		if !strings.Contains(line, want) {
			t.Fatal("complete diagnostic field was missing or truncated")
		}
	}
	if strings.Count(line, "\n") != 1 || strings.ContainsAny(line, "\r\x1b") {
		t.Fatal("upstream text injected a new log line or terminal control")
	}
}

func TestRedactDiagnosticSecretsPreservesLongMessages(t *testing.T) {
	prefix := strings.Repeat("original detail ", 100)
	sanitized := RedactDiagnosticSecrets(prefix + "Authorization: Bearer test-secret; api_key=key-secret; https://user:password-secret@example.test/; complete-tail")
	for _, forbidden := range []string{"test-secret", "key-secret", "password-secret"} {
		if strings.Contains(sanitized, forbidden) {
			t.Fatalf("credential retained: %s", forbidden)
		}
	}
	for _, want := range []string{prefix, "Authorization: \"[REDACTED]\"", "api_key=", "https://[REDACTED]@example.test/", "complete-tail"} {
		if !strings.Contains(sanitized, want) {
			t.Fatalf("diagnostic detail was lost: %s", want)
		}
	}
}
