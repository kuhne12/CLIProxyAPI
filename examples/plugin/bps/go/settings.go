package main

import (
	_ "embed"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const settingsPath = "/v0/resource/plugins/bps/settings"

//go:embed web/settings.html
var settingsHTML []byte

// The public resource serves only static assets. Settings are read and written
// by the authenticated CPA v8 configuration API.
func serveSettings(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if req.Method != http.MethodGet || req.Path != settingsPath {
		return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound}, nil
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":            {"text/html; charset=utf-8"},
			"Cache-Control":           {"no-store"},
			"X-Content-Type-Options":  {"nosniff"},
			"Content-Security-Policy": {"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"},
		},
		Body: settingsHTML,
	}, nil
}
