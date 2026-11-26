package executor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestBPSExecutorCompactionDiagnostics(t *testing.T) {
	previousLevel := log.GetLevel()
	previousHooks := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	t.Cleanup(func() { log.SetLevel(previousLevel); log.StandardLogger().ReplaceHooks(previousHooks) })
	log.SetLevel(log.InfoLevel)
	hook := logtest.NewGlobal()
	const marker = `{"type":"compaction","id":"private-checkpoint","encrypted_content":"private-state"}`
	const tool = `{"type":"function_call","name":"read_ranges","id":"private-item","call_id":"private-call","arguments":"private-arguments"}`
	for _, stream := range []bool{false, true} {
		for _, kind := range []string{"valid", "unsupported", "omitted", "payload_changed"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, kind), func(t *testing.T) {
				hook.Reset()
				events := []string{`{"type":"response.output_item.done","item":` + marker + `}`}
				output := marker
				failed := kind == "unsupported" || kind == "omitted"
				if failed {
					events = append(events, `{"type":"response.output_item.done","item":`+tool+`}`)
					if kind == "unsupported" {
						output += "," + tool
					}
				}
				events = append(events, `{"type":"response.completed","response":{"id":"resp","status":"completed","output":[`+output+`]}}`)
				e, requests := newBPSFixture(t, events, nil)
				auth := bpsFixtureAuth()
				auth.ID = t.Name()
				req, opts := bpsFixtureRequest(sdktranslator.FormatOpenAIResponse)
				doc := ojson.NewDoc(req.Payload)
				_ = doc.SetRaw("input", `[{"role":"user","content":"private-prompt"}]`)
				_ = doc.SetRaw("tools", `[{"type":"function","name":"lookup","parameters":{"type":"object"}}]`)
				req.Payload, opts.OriginalRequest = doc.Bytes(), doc.Bytes()
				if kind == "payload_changed" {
					e.cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: req.Model, Protocol: "codex"}}, Params: []string{"input"}}}
				}
				ctx := logging.WithRequestID(t.Context(), "diag-request-id")
				var generationErr error
				if stream {
					result, errStream := e.ExecuteStream(ctx, auth, req, opts)
					if errStream != nil {
						t.Fatal(errStream)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							generationErr = chunk.Err
						}
					}
				} else {
					_, generationErr = e.Execute(ctx, auth, req, opts)
				}
				if (generationErr != nil) != failed || (failed && generationErr.Error() != "BPS returned an unsupported response or tool call") {
					t.Fatalf("unexpected generation error: %v", generationErr)
				}
				if requests.Load() != 1 {
					t.Fatal("diagnostics replayed generation")
				}
				var timing, warning *log.Entry
				warningCount := 0
				for _, entry := range hook.AllEntries() {
					if entry.Data["request_id"] != "diag-request-id" {
						continue
					}
					switch entry.Message {
					case "BPS request timing":
						timing = entry
					case "BPS response validation failed":
						warning, warningCount = entry, warningCount+1
					}
					formatted, errFormat := (&logging.LogFormatter{}).Format(entry)
					if errFormat != nil {
						t.Fatal(errFormat)
					}
					for _, secret := range []string{"private-", "fixture-token", "fixture-account"} {
						if strings.Contains(string(formatted), secret) {
							t.Fatalf("log leaked %s", secret)
						}
					}
					for key := range entry.Data {
						if key != "request_id" && !strings.Contains(string(formatted), key+"=") {
							t.Errorf("log formatter dropped %s", key)
						}
					}
				}
				if timing == nil || timing.Data["compaction_seen"] != true || timing.Data["tool_catalog_count"] != 1 || timing.Data["history_status"] != "no_checkpoint" {
					t.Fatalf("missing request diagnostics: %+v", timing)
				}
				confirmation := "confirmed"
				if failed {
					confirmation = "not_confirmed"
					if warningCount != 1 || warning.Level != log.WarnLevel || warning.Data["validation_reason"] == "" || warning.Data["validation_compaction_seen"] != true {
						t.Fatalf("missing failure diagnostic: %+v", warning)
					}
					stage := "tool_translation"
					if kind == "omitted" {
						stage = "terminal_tool_completeness"
					}
					if warning.Data["validation_stage"] != stage {
						t.Fatalf("incorrect stage: %v", warning.Data["validation_stage"])
					}
				} else if warningCount != 0 {
					t.Fatal("successful response logged a failure")
				}
				if kind == "payload_changed" {
					confirmation = "payload_input_changed"
				}
				if timing.Data["compaction_confirmation"] != confirmation {
					t.Fatalf("confirmation = %v, want %s", timing.Data["compaction_confirmation"], confirmation)
				}
			})
		}
	}
}
