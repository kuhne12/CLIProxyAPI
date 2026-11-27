package helps

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/MoeForge/moego/ojson"
	"github.com/MoeForge/moego/regexp"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

var (
	bpsUsageURLPattern         = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)
	bpsUsageEmailPattern       = regexp.MustCompile(`(?i)\b[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}\b`)
	bpsUsagePrivateTextPattern = regexp.MustCompile(`(?im)((?:^|[{,;])\s*["']?(?:authorization|proxy[ _-]*authorization|cookie|set[ _-]*cookie|input|output|messages|prompt|instructions|arguments|encrypted[ _-]*content)["']?\s*[:=]\s*)[^\r\n]*`)
)

// bpsUsageFailureDiagnostic accepts the recursively redacted native error event
// and narrows it to bounded scalar fields, never retaining provider payloads.
func bpsUsageFailureDiagnostic(payload []byte) (string, http.Header) {
	detail := ojson.GetBytes(payload, "error")
	if !detail.IsObject() {
		detail = ojson.GetBytes(payload, "response.error")
	}
	if !detail.IsObject() {
		return "", nil
	}
	doc := ojson.NewDoc([]byte(`{"diagnostic_source":"bps_upstream","error":{}}`))
	found := false
	for _, field := range []string{"code", "type", "message", "param"} {
		value := detail.Get(field)
		if !value.Exists() {
			continue
		}
		if value.Type == ojson.String {
			budget := 160
			if field == "message" {
				budget = 3000
			}
			if err := doc.Set("error."+field, boundedBPSUsageText(value.String(), budget)); err != nil {
				return "", nil
			}
			found = found || field != "param"
		} else if field == "param" && !value.IsObject() && !value.IsArray() && len(value.Raw) <= 160 {
			if err := doc.SetRaw("error.param", value.Raw); err != nil {
				return "", nil
			}
		}
	}
	if !found {
		return "", nil
	}
	headers := make(http.Header)
	for _, source := range []ojson.Result{ojson.GetBytes(payload, "headers"), ojson.GetBytes(payload, "response.headers"), detail.Get("headers")} {
		if !source.IsObject() {
			continue
		}
		source.ForEach(func(key, value ojson.Result) bool {
			if !bpsUsageDiagnosticHeader(key.String()) {
				return true
			}
			if value.IsArray() {
				value = value.Get("0")
			}
			if value.Type == ojson.String || value.Type == ojson.Number {
				clean := boundedBPSUsageText(value.String(), 128)
				clean = strings.Join(strings.Fields(clean), " ")
				if clean != "" {
					headers.Set(key.String(), clean)
				}
			}
			return true
		})
	}
	body := doc.String()
	if len(body) >= maxUsageFailureDiagnosticBytes {
		return "", nil
	}
	return body, headers
}

func bpsUsageDiagnosticHeader(name string) bool {
	switch strings.ToLower(name) {
	case "retry-after", "x-ratelimit-limit-requests", "x-ratelimit-remaining-requests", "x-ratelimit-reset-requests",
		"x-ratelimit-limit-tokens", "x-ratelimit-remaining-tokens", "x-ratelimit-reset-tokens",
		"x-request-id", "request-id", "openai-request-id":
		return true
	}
	return false
}

// Budget encoded JSON bytes, including worst-case escaping, before serializing
// once. Truncation is rune-safe and cannot leave partial JSON or UTF-8 behind.
func boundedBPSUsageText(text string, budget int) string {
	text = strings.ToValidUTF8(text, "�")
	text = logging.RedactDiagnosticSecrets(text)
	text = bpsUsageURLPattern.ReplaceAllString(text, "[REDACTED URL]")
	text = bpsUsageEmailPattern.ReplaceAllString(text, "[REDACTED EMAIL]")
	text = bpsUsagePrivateTextPattern.ReplaceAllString(text, "${1}[REDACTED]")
	used := 0
	for index, r := range text {
		size := utf8.RuneLen(r)
		if r < 0x20 || r == '"' || r == '\\' || r == '<' || r == '>' || r == '&' || r == '\u2028' || r == '\u2029' {
			size = 6
		}
		if used+size > budget-3 {
			return text[:index] + "…"
		}
		used += size
	}
	return text
}
