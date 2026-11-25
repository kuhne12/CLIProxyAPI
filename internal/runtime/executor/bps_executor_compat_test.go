package executor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestBPSExecutorContextManagementSurvivesTranslation(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex} {
		for _, stream := range []bool{false, true} {
			for _, rule := range []string{"preserve", "override", "filter", "default", "default_missing"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", format, stream, rule), func(t *testing.T) {
					e, count := newBPSFixture(t, []string{bpsCompletedFixture}, func(raw []byte) {
						got := ojson.GetBytes(raw, "context_management")
						if rule == "filter" {
							if got.Exists() {
								t.Error("restored a filtered field")
							}
							return
						}
						want := int64(100000)
						if rule == "override" || rule == "default_missing" {
							want = 75000
						}
						if got.Get("0.compact_threshold").Int() != want {
							t.Errorf("threshold=%s want=%d", got.Raw, want)
						}
					})
					match := []config.PayloadModelRule{{Name: "gpt-6-astra", Protocol: "codex", FromProtocol: format.String()}}
					if rule == "override" {
						e.cfg.Payload.Override = []config.PayloadRule{{Models: match, Params: map[string]any{"context_management.0.compact_threshold": 75000}}}
					}
					if rule == "filter" {
						e.cfg.Payload.Filter = []config.PayloadFilterRule{{Models: match, Params: []string{"context_management"}}}
					}
					if rule == "default" || rule == "default_missing" {
						e.cfg.Payload.Default = []config.PayloadRule{{Models: match, Params: map[string]any{"context_management.0.compact_threshold": 75000}}}
					}
					req, opts := bpsFixtureRequest(format)
					doc := ojson.NewDoc(req.Payload)
					_ = doc.Set("context_management", []any{map[string]any{"type": "compaction", "compact_threshold": 100000}})
					if rule == "default_missing" {
						_ = doc.Delete("context_management")
					}
					req.Payload = doc.Bytes()
					opts.OriginalRequest = req.Payload
					if stream {
						response, errExecute := e.ExecuteStream(t.Context(), bpsFixtureAuth(), req, opts)
						if errExecute != nil {
							t.Fatal(errExecute)
						}
						for chunk := range response.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, errExecute := e.Execute(t.Context(), bpsFixtureAuth(), req, opts); errExecute != nil {
						t.Fatal(errExecute)
					}
					if count.Load() != 1 {
						t.Fatalf("upstream requests=%d", count.Load())
					}
				})
			}
		}
	}
}

func TestBPSExecutorHostedToolAliasAndForcedChoice(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(fmt.Sprintf("forced=%t", forced), func(t *testing.T) {
			e, count := newBPSFixture(t, []string{bpsCompletedFixture}, func(raw []byte) {
				if !strings.Contains(string(raw), "These declarations were omitted") {
					t.Error("missing unsupported hosted tool disclosure")
				}
				if !strings.Contains(string(raw), "client_echo") {
					t.Error("client tool was removed")
				}
			})
			req, opts := bpsFixtureRequest(sdktranslator.FormatOpenAIResponse)
			doc := ojson.NewDoc(req.Payload)
			_ = doc.Set("tools", []any{map[string]any{"type": "web_search_2025_08_26_preview"}, map[string]any{"type": "function", "name": "client_echo", "parameters": map[string]any{"type": "object"}}})
			if forced {
				_ = doc.Set("tool_choice", map[string]any{"type": "web_search_2025_08_26_preview"})
			}
			req.Payload = doc.Bytes()
			opts.OriginalRequest = req.Payload
			_, errExecute := e.Execute(t.Context(), bpsFixtureAuth(), req, opts)
			if forced {
				if errExecute == nil || count.Load() != 0 {
					t.Fatalf("forced hosted search silently downgraded, calls=%d error=%v", count.Load(), errExecute)
				}
			} else if errExecute != nil {
				t.Fatal(errExecute)
			}
		})
	}
}
