package executor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/coder/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"gopkg.in/yaml.v3"
)

func bpsWireTierConfig(t *testing.T, text string) *config.Config {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(text), &node); err != nil {
		t.Fatal(err)
	}
	return &config.Config{Plugins: config.PluginsConfig{Configs: map[string]config.PluginInstanceConfig{"bps": {Raw: node}}}}
}

func bpsRunTierRequest(t *testing.T, e *BPSExecutor, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options, stream bool) error {
	t.Helper()
	if !stream {
		_, err := e.Execute(t.Context(), auth, req, opts)
		return err
	}
	result, err := e.ExecuteStream(t.Context(), auth, req, opts)
	if err != nil {
		return err
	}
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	return streamErr
}

func TestBPSServiceTierWirePolicy(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name   string
				policy string
				caller string
				want   string
			}{
				{"off_preserves_priority", "force-ultrafast: false", "priority", "priority"},
				{"off_preserves_flex", "force-ultrafast: false", "flex", "flex"},
				{"off_preserves_default", "force-ultrafast: false", "default", "default"},
				{"off_preserves_auto", "force-ultrafast: false", "auto", "auto"},
				{"off_normalizes_caller", "force-ultrafast: false", "  PRIORITY  ", "priority"},
				{"off_preserves_ultrafast", "force-ultrafast: false", "ultrafast", "ultrafast"},
				{"off_preserves_absence", "force-ultrafast: false", "", ""},
				{"global_overrides_caller", "force-ultrafast: true", "flex", "ultrafast"},
				{"global_adds_tier", "force-ultrafast: true", "", "ultrafast"},
				{"selected_id", "force-ultrafast-auth-ids: [bps-test.json]", "priority", "ultrafast"},
				{"selected_filename", "force-ultrafast-auth-ids: [selected-file.json]", "priority", "ultrafast"},
				{"unselected", "force-ultrafast-auth-ids: [other-file.json]", "flex", "flex"},
				{"substring", "force-ultrafast-auth-ids: [bps-test]", "priority", "priority"},
				{"empty_selection", "force-ultrafast-auth-ids: []", "priority", "priority"},
				{"null_config", "null", "priority", "priority"},
				{"invalid_config", "force-ultrafast: true\nforce-ultrafast-auth-ids: [id, id]", "priority", "priority"},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", format, stream, tc.name), func(t *testing.T) {
					e, calls := newBPSFixture(t, []string{bpsCompletedFixture}, func(raw []byte) {
						tier := ojson.GetBytes(raw, "service_tier")
						if tier.String() != tc.want || tier.Exists() != (tc.want != "") {
							t.Errorf("wire service_tier=%s, want %q: %s", tier.Raw, tc.want, raw)
						}
						if got := ojson.GetBytes(raw, "reasoning_effort").String(); got != "low" {
							t.Errorf("service tier changed reasoning effort: %q", got)
						}
					})
					e.cfg = bpsWireTierConfig(t, tc.policy)
					req, opts := bpsFixtureRequest(format)
					if tc.caller != "" {
						doc := ojson.NewDoc(req.Payload)
						if err := doc.Set("service_tier", tc.caller); err != nil {
							t.Fatal(err)
						}
						req.Payload = doc.Bytes()
						opts.OriginalRequest = req.Payload
					}
					auth := bpsFixtureAuth()
					auth.FileName = "selected-file.json"
					if err := bpsRunTierRequest(t, e, auth, req, opts, stream); err != nil {
						t.Fatal(err)
					}
					if got := calls.Load(); got != 1 {
						t.Fatalf("upstream requests=%d, want 1", got)
					}
				})
			}
		}
	}
}

func TestBPSServiceTierFinalPayloadRules(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI} {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"override", "filter"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", format, stream, mode), func(t *testing.T) {
					e, calls := newBPSFixture(t, []string{bpsCompletedFixture}, func(raw []byte) {
						tier := ojson.GetBytes(raw, "service_tier")
						if mode == "override" && tier.String() != "flex" {
							t.Errorf("forced tier overwrote administrator override: %s", raw)
						}
						if mode == "filter" && tier.Exists() {
							t.Errorf("forced tier restored filtered service_tier: %s", raw)
						}
					})
					e.cfg = bpsWireTierConfig(t, "force-ultrafast: true")
					match := []config.PayloadModelRule{{Name: "gpt-6-astra", Protocol: "codex", FromProtocol: format.String()}}
					e.cfg.Payload.Override = []config.PayloadRule{{Models: match, Params: map[string]any{"service_tier": "flex"}}}
					if mode == "filter" {
						e.cfg.Payload.Filter = []config.PayloadFilterRule{{Models: match, Params: []string{"service_tier"}}}
					}
					req, opts := bpsFixtureRequest(format)
					finalizations := 0
					opts.Metadata = map[string]any{coreexecutor.FinalizedResponseFormatCallbackMetadataKey: func(string) { finalizations++ }}
					if err := bpsRunTierRequest(t, e, bpsFixtureAuth(), req, opts, stream); err != nil {
						t.Fatal(err)
					}
					if calls.Load() != 1 || finalizations != 1 {
						t.Fatalf("requests=%d, finalizations=%d; want one each", calls.Load(), finalizations)
					}
				})
			}
		}
	}
}

func TestBPSServiceTierToolRecovery(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"forced", "override", "filter"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, mode), func(t *testing.T) {
				var connections atomic.Int32
				first := bpsRecoveryEvent(t, "list_connectors", "tier_first", 10)
				second := bpsRecoveryEvent(t, "lookup", "tier_second", 20)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n := connections.Add(1)
					if n > 2 {
						t.Error("unexpected extra corrective generation")
						http.Error(w, "unexpected", http.StatusBadRequest)
						return
					}
					conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, Subprotocols: []string{"responses"}})
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = conn.CloseNow() }()
					_, raw, err := conn.Read(r.Context())
					if err != nil {
						t.Error(err)
						return
					}
					tier := ojson.GetBytes(raw, "service_tier")
					want := "ultrafast"
					if mode == "override" {
						want = "priority"
					}
					if mode == "filter" {
						want = ""
					}
					if tier.String() != want || tier.Exists() != (want != "") {
						t.Errorf("generation %d tier=%s, want %q", n, tier.Raw, want)
					}
					event := first
					if n == 2 {
						event = second
					}
					if err := conn.Write(r.Context(), websocket.MessageText, event); err != nil {
						return
					}
					_, _, _ = conn.Read(r.Context())
				}))
				t.Cleanup(server.Close)
				e := NewBPSExecutor(bpsWireTierConfig(t, "force-ultrafast: true"))
				e.endpoint = server.URL
				match := []config.PayloadModelRule{{Name: "gpt-6-astra", Protocol: "codex"}}
				if mode == "override" {
					e.cfg.Payload.Override = []config.PayloadRule{{Models: match, Params: map[string]any{"service_tier": "priority"}}}
				}
				if mode == "filter" {
					e.cfg.Payload.Filter = []config.PayloadFilterRule{{Models: match, Params: []string{"service_tier"}}}
				}
				req, opts := bpsFixtureRequest(sdktranslator.FormatOpenAIResponse)
				doc := ojson.NewDoc(req.Payload)
				if err := doc.Set("tools", []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}); err != nil {
					t.Fatal(err)
				}
				req.Payload = doc.Bytes()
				opts.OriginalRequest = req.Payload
				finalizations := 0
				opts.Metadata = map[string]any{coreexecutor.FinalizedResponseFormatCallbackMetadataKey: func(string) { finalizations++ }}
				auth := bpsFixtureAuth()
				auth.ID = t.Name()
				if err := bpsRunTierRequest(t, e, auth, req, opts, stream); err != nil {
					t.Fatal(err)
				}
				if connections.Load() != 2 || finalizations != 2 {
					t.Fatalf("requests=%d, finalizations=%d; want two each", connections.Load(), finalizations)
				}
			})
		}
	}
}

func TestBPSServiceTierRejectionDoesNotDowngrade(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI} {
		for _, stream := range []bool{false, true} {
			for _, code := range []string{"unsupported_service_tier", "rate_limit_exceeded"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", format, stream, code), func(t *testing.T) {
					event := fmt.Sprintf(`{"type":"error","error":{"code":%q,"message":"requested service tier rejected"}}`, code)
					e, calls := newBPSFixture(t, []string{event}, func(raw []byte) {
						if got := ojson.GetBytes(raw, "service_tier").String(); got != "ultrafast" {
							t.Errorf("rejected tier was downgraded to %q", got)
						}
					})
					e.cfg = bpsWireTierConfig(t, "force-ultrafast: true")
					req, opts := bpsFixtureRequest(format)
					if err := bpsRunTierRequest(t, e, bpsFixtureAuth(), req, opts, stream); err == nil {
						t.Fatal("tier rejection was hidden")
					}
					if got := calls.Load(); got != 1 {
						t.Fatalf("rejection triggered %d requests; want one without downgrade", got)
					}
				})
			}
		}
	}
}
