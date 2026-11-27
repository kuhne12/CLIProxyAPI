package helps

import (
	"net/http"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const maxUsageFailureDiagnosticBytes = 4 << 10

type usageFailureDiagnostic struct {
	body    string
	headers http.Header
}

// SetFailureDiagnostic stores an already-sanitized diagnostic for usage sinks
// only. Each reporter belongs to one upstream attempt; no context is mutated.
func (r *UsageReporter) SetFailureDiagnostic(body string, headers http.Header) {
	if r == nil {
		return
	}
	diagnostic := usageFailureDiagnostic{}
	if body != "" && len(body) < maxUsageFailureDiagnosticBytes && json.Valid([]byte(body)) {
		diagnostic.body = body
		diagnostic.headers = headers.Clone()
	}
	r.failureDiagnosticMu.Lock()
	r.failureDiagnostic = diagnostic
	r.failureDiagnosticMu.Unlock()
}

func (r *UsageReporter) applyFailureDiagnostic(record *usage.Record) {
	if r == nil || record == nil || !record.Failed {
		return
	}
	r.failureDiagnosticMu.RLock()
	defer r.failureDiagnosticMu.RUnlock()
	if r.failureDiagnostic.body == "" {
		return
	}
	record.Fail.Body = r.failureDiagnostic.body
	if len(r.failureDiagnostic.headers) == 0 {
		return
	}
	if record.ResponseHeaders == nil {
		record.ResponseHeaders = make(http.Header)
	}
	for key, values := range r.failureDiagnostic.headers {
		record.ResponseHeaders[key] = append([]string(nil), values...)
	}
}
