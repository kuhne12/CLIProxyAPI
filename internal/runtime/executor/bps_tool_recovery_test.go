package executor

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/coder/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/bpsbridge"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func bpsRecoveryEvent(t *testing.T, tool, suffix string, tokens int) []byte {
	t.Helper()
	doc := ojson.NewDocString(bpsCompletedFixture)
	_ = doc.Set("response.id", "resp_"+suffix)
	_ = doc.Set("response.output", []any{map[string]any{"type": "function_call", "id": "fc_" + suffix, "call_id": "call_" + suffix, "name": tool, "arguments": "{}", "status": "completed"}})
	_ = doc.Set("response.usage", map[string]any{"input_tokens": tokens, "output_tokens": 1, "total_tokens": tokens + 1, "input_tokens_details": map[string]any{"cached_tokens": 2, "cache_write_tokens": 3, "cache_write_tokens_details": map[string]any{"ttl_30m_tokens": 1, "ttl_12h_tokens": 2}}})
	return doc.Bytes()
}

func TestBPSExecutorToolRecovery(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"success", "second_invalid", "open_rejected", "open_rate_limited", "upstream_error", "upstream_rate_limited", "rules", "input_override", "correction_input_filter"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, mode), func(t *testing.T) {
				var mu sync.Mutex
				var requests [][]byte
				connections := 0
				first := bpsRecoveryEvent(t, "list_connectors", "first", 10)
				second := bpsRecoveryEvent(t, "lookup", "second", 20)
				if mode == "second_invalid" {
					second = bpsRecoveryEvent(t, "list_connectors", "second", 20)
				}
				if mode == "upstream_error" {
					second = []byte("{\"type\":\"error\",\"error\":{\"code\":\"server_error\",\"message\":\"upstream unavailable\"}}")
				}
				if mode == "upstream_rate_limited" {
					second = []byte("{\"type\":\"error\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"rate limited\"}}")
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					connections++
					n := connections
					mu.Unlock()
					if n > 2 {
						t.Error("more than one corrective generation")
						http.Error(w, "unexpected", 400)
						return
					}
					if n == 2 && (mode == "open_rejected" || mode == "open_rate_limited") {
						status := 400
						if mode == "open_rate_limited" {
							status = 429
							w.Header().Set("Retry-After", "7")
						}
						http.Error(w, "rejected", status)
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
					mu.Lock()
					requests = append(requests, bytes.Clone(raw))
					mu.Unlock()
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
				e := NewBPSExecutor(&config.Config{})
				e.endpoint = server.URL
				req, opts := bpsFixtureRequest(sdktranslator.FormatOpenAIResponse)
				doc := ojson.NewDoc(req.Payload)
				_ = doc.Set("tools", []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}})
				req.Payload = doc.Bytes()
				opts.OriginalRequest = req.Payload
				finalizations := 0
				opts.Metadata = map[string]any{coreexecutor.FinalizedResponseFormatCallbackMetadataKey: func(string) { finalizations++ }}
				match := []config.PayloadModelRule{{Name: req.Model, Protocol: "codex"}}
				if mode == "rules" {
					e.cfg.Payload.Override = []config.PayloadRule{{Models: match, Params: map[string]any{"metadata.agent_iteration": "91", "metadata.audit": "configured"}}}
					e.cfg.Payload.Filter = []config.PayloadFilterRule{{Models: match, Params: []string{"context_management", "basispoints_resume_token"}}}
				}
				if mode == "input_override" {
					e.cfg.Payload.Override = []config.PayloadRule{{Models: match, Params: map[string]any{"input": []any{map[string]any{"role": "user", "content": "configured input"}}}}}
				}
				if mode == "correction_input_filter" {
					prepared, _, err := bpsbridge.Prepare(req.Payload, "filter-test", &bpsbridge.ReplayCache{})
					if err != nil {
						t.Fatal(err)
					}
					// This path is absent initially, but matches the appended native call.
					path := fmt.Sprintf("input.%d.arguments", len(ojson.GetBytes(prepared, "input").Array()))
					e.cfg.Payload.Filter = []config.PayloadFilterRule{{Models: match, Params: []string{path}}}
				}
				auth := bpsFixtureAuth()
				auth.ID = t.Name()
				capture := &bpsUsageCapture{authID: auth.ID, records: make(chan usage.Record, 4)}
				usage.RegisterNamedPlugin(t.Name(), capture)
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
					result, err := e.Execute(t.Context(), auth, req, opts)
					generationErr = err
					output.Write(result.Payload)
				}
				wantSuccess := mode == "success" || mode == "rules"
				if (generationErr == nil) != wantSuccess {
					t.Fatalf("unexpected outcome: %v", generationErr)
				}
				if !wantSuccess {
					var failure *pluginabi.Error
					if !errors.As(generationErr, &failure) || !failure.IsStreamTruncated() || failure.Retryable {
						t.Fatalf("failed correction allows an outer replay: %v", generationErr)
					}
					if mode == "open_rate_limited" && (failure.HTTPStatus != 429 || failure.RetryAfterSeconds != 7) {
						t.Fatal("correction lost upstream rejection details")
					}
				}
				mu.Lock()
				sent := append([][]byte(nil), requests...)
				dials := connections
				mu.Unlock()
				wantDials := 2
				wantFinalizations := 2
				if mode == "input_override" || mode == "correction_input_filter" {
					wantDials = 1
				}
				if mode == "input_override" {
					wantFinalizations = 1
				}
				if dials != wantDials || finalizations != wantFinalizations {
					t.Fatalf("dials=%d want=%d finalizations=%d want=%d", dials, wantDials, finalizations, wantFinalizations)
				}
				if len(sent) == 2 {
					a, b := ojson.ParseBytes(sent[0]), ojson.ParseBytes(sent[1])
					if a.Get("metadata.task_id").String() != b.Get("metadata.task_id").String() || a.Get("metadata.turn_id").String() != b.Get("metadata.turn_id").String() {
						t.Error("correction changed task or turn")
					}
					if a.Get("basispoints_request_id").String() == b.Get("basispoints_request_id").String() {
						t.Error("correction reused transport request ID")
					}
					items := b.Get("input").Array()
					tail := items[len(items)-3:]
					if tail[0].Get("call_id").String() != "call_first" || tail[1].Get("type").String() != "function_call_output" || tail[1].Get("call_id").String() != "call_first" || !strings.Contains(tail[1].Get("output").String(), "client_tool_unavailable") {
						t.Error("correction did not continue the actual unavailable call")
					}
					if mode == "rules" {
						for _, raw := range sent {
							v := ojson.ParseBytes(raw)
							if v.Get("metadata.agent_iteration").String() != "91" || v.Get("metadata.audit").String() != "configured" || v.Get("context_management").Exists() || v.Get("basispoints_resume_token").Exists() {
								t.Error("internal correction overwrote final payload rules")
							}
						}
					} else if a.Get("metadata.agent_iteration").String() != "1" || b.Get("metadata.agent_iteration").String() != "2" {
						t.Error("agent iteration not advanced once")
					}
				}
				if wantSuccess && (!bytes.Contains(output.Bytes(), []byte("lookup")) || bytes.Contains(output.Bytes(), []byte("list_connectors"))) {
					t.Error("wrong tool delivered")
				}
				wantInput := int64(30)
				if mode == "open_rejected" || mode == "open_rate_limited" || mode == "upstream_error" || mode == "upstream_rate_limited" || mode == "correction_input_filter" {
					wantInput = 10
				}
				if mode == "input_override" {
					// Recovery is disabled, preserving ordinary bridge failure accounting.
					wantInput = 0
				}
				select {
				case record := <-capture.records:
					if record.Detail.InputTokens != wantInput || record.Failed == wantSuccess {
						t.Fatalf("incorrect accounting: failed=%t input=%d want=%d", record.Failed, record.Detail.InputTokens, wantInput)
					}
					wantWrite := int64(6)
					if wantInput == 10 {
						wantWrite = 3
					}
					if wantInput == 0 {
						wantWrite = 0
					}
					if record.Detail.CacheCreationTokens != wantWrite {
						t.Errorf("cache writes=%d want=%d", record.Detail.CacheCreationTokens, wantWrite)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("usage record missing")
				}
				select {
				case <-capture.records:
					t.Error("duplicate usage record")
				default:
				}
			})
		}
	}
}
