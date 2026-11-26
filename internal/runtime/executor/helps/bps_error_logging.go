package helps

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/bpsbridge"
)

// ObserverWithErrorLogging captures native failures before the bridge or public
// error mapper can replace them. It also covers corrective generations because
// their transport inherits the same observer. Event bytes are never modified.
func (o *BPSObservation) ObserverWithErrorLogging(ctx context.Context, secrets ...string) bpsbridge.Observer {
	observer := o.Observer()
	observer.Event = func(data []byte) {
		o.observeUpstream(data)
		kind := ojson.GetBytes(data, "type").String()
		if kind != "error" && kind != "response.failed" {
			return
		}
		payload, redacted := redactBPSErrorEvent(data, secrets)
		detail := ojson.GetBytes(payload, "error")
		if !detail.IsObject() {
			detail = ojson.GetBytes(payload, "response.error")
		}
		fields := o.fields()
		fields["provider"] = "bps"
		fields["upstream_error_event"] = kind
		fields["upstream_error_code"] = detail.Get("code").String()
		fields["upstream_error_type"] = detail.Get("type").String()
		fields["upstream_error_bytes"] = len(data)
		fields["upstream_error_sha256"] = bpsbridge.DiagnosticHash(string(data))
		fields["upstream_error_redacted"] = redacted
		fields["upstream_error_payload"] = string(payload)
		LogWithRequestID(ctx).WithFields(fields).Warn("BPS upstream error")
	}
	return observer
}

// Preserve the complete error event, including provider-specific limit fields,
// without logging credentials or unrelated prompt/tool/checkpoint content. The
// existing WebSocket frame limit bounds input size; messages are not truncated.
func redactBPSErrorEvent(data []byte, secrets []string) ([]byte, bool) {
	if !json.Valid(data) || !ojson.ParseBytes(data).IsObject() {
		return []byte("{\"logging_error\":\"invalid upstream error JSON\"}"), true
	}
	doc := ojson.NewDoc(data)
	pathEscaper := strings.NewReplacer("\\", "\\\\", ".", "\\.", ":", "\\:", "@", "\\@", "!", "\\!", "|", "\\|", "#", "\\#", "*", "\\*", "?", "\\?")
	redacted := false
	var mutationErr error
	set := func(path, value string) {
		if mutationErr == nil {
			mutationErr = doc.Set(path, value)
			redacted = true
		}
	}
	var visit func(ojson.Result, string, int)
	visit = func(value ojson.Result, path string, depth int) {
		if mutationErr != nil {
			return
		}
		if depth > 128 {
			set(path, "[REDACTED: nesting limit]")
			return
		}
		if value.IsObject() || value.IsArray() {
			array := value.IsArray()
			keys := make(map[string]bool)
			value.ForEach(func(key, child ojson.Result) bool {
				name := key.String()
				if !array {
					if name == "" || keys[name] {
						mutationErr = errors.New("ambiguous upstream error object key")
						return false
					}
					keys[name] = true
				}
				segment := pathEscaper.Replace(name)
				if array {
					segment = strconv.FormatInt(key.Int(), 10)
				}
				childPath := segment
				if path != "" {
					childPath = path + "." + segment
				}
				if !array && bpsPrivateErrorField(name) {
					set(childPath, "[REDACTED]")
				} else {
					visit(child, childPath, depth+1)
				}
				return mutationErr == nil
			})
			return
		}
		if value.Type != ojson.String {
			return
		}
		original := value.String()
		clean := logging.RedactDiagnosticSecrets(original)
		for _, secret := range secrets {
			if secret != "" {
				clean = strings.ReplaceAll(clean, secret, "[REDACTED]")
			}
		}
		if clean != original {
			set(path, clean)
		}
	}
	visit(ojson.ParseBytes(data), "", 0)
	if mutationErr != nil {
		return []byte("{\"logging_error\":\"upstream error redaction failed\"}"), true
	}
	return doc.Bytes(), redacted
}

func bpsPrivateErrorField(name string) bool {
	name = strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(strings.ToLower(name))
	switch name {
	case "authorization", "proxyauthorization", "cookie", "setcookie", "apikey", "xapikey",
		"accesskey", "accesstoken", "refreshtoken", "idtoken", "token", "secret", "clientsecret",
		"privatekey", "password", "credential", "credentials", "resumetoken", "basispointsresumetoken",
		"basispointsreplaycursor", "encryptedcontent", "input", "output", "messages", "prompt",
		"instructions", "arguments", "secwebsocketprotocol":
		return true
	}
	return false
}
