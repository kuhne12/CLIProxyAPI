package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const bpsDelegationTask = "Inspect the synthetic sample; do not use tools."

func bpsDelegationOptions(t *testing.T, child bool) coreexecutor.Options {
	t.Helper()
	session, parent := "parent", ""
	if child {
		session, parent = "child", "parent"
	}
	return coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Metadata: map[string]any{
		coreexecutor.CallerScopeMetadataKey: t.Name(), coreexecutor.CanonicalSessionIDMetadataKey: session, coreexecutor.ParentSessionIDMetadataKey: parent,
	}}
}

func bpsDelegationChildRequest(t *testing.T) coreexecutor.Request {
	t.Helper()
	doc := ojson.NewDocString(`{"model":"gpt-6.1-sol","reasoning":{"effort":"low"},"input":[{"type":"agent_message","id":"amsg_fixture","author":"/root","recipient":"/root/worker","content":[{"type":"input_text","text":"Payload:"},{"type":"encrypted_content","encrypted_content":""},{"type":"encrypted_content","encrypted_content":"opaque-synthetic-state"}]}]}`)
	_ = doc.Set("input.0.content.1.encrypted_content", bpsDelegationTask)
	return coreexecutor.Request{Model: "gpt-6.1-sol", Format: sdktranslator.FormatOpenAIResponse, Payload: doc.Bytes()}
}

// Exercise real BPS transport decoding and full-response validation, rather than
// seeding the proof store directly from a fabricated client-facing tool call.
func bpsDelegationParent(t *testing.T, stream, invalid bool, observe ...func([]byte)) {
	t.Helper()
	terminal := ojson.NewDocString(bpsCompletedFixture)
	envelope := ojson.NewDocString(`{"name":"collaboration.spawn_agent","arguments":{"task_name":"worker","message":""}}`)
	_ = envelope.Set("arguments.message", bpsDelegationTask)
	transport := ojson.NewDocString(`{"summary":"Delegate synthetic task","extended_summary":"Test only","destructive":false,"references":[]}`)
	_ = transport.Set("code", envelope.String())
	items := []any{map[string]any{"type": "function_call", "id": "fc_delegation", "call_id": "call_delegation", "name": "run_officejs", "status": "completed", "arguments": transport.String()}}
	if invalid {
		items = append(items, map[string]any{"type": "function_call", "id": "fc_bad", "call_id": "call_bad", "name": "run_officejs", "status": "completed", "arguments": "not-json"})
	}
	_ = terminal.Set("response.output", items)
	e, _ := newBPSFixture(t, []string{terminal.String()}, nil)
	req, _ := bpsFixtureRequest(sdktranslator.FormatOpenAIResponse)
	doc := ojson.NewDoc(req.Payload)
	_ = doc.SetRaw("tools", `[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"task_name":{"type":"string"},"message":{"type":"string"}},"required":["task_name","message"],"additionalProperties":false}}]}]`)
	req.Payload = doc.Bytes()
	opts := bpsDelegationOptions(t, false)
	opts.OriginalRequest = req.Payload
	var err error
	var output bytes.Buffer
	if stream {
		var result *coreexecutor.StreamResult
		result, err = e.ExecuteStream(t.Context(), bpsFixtureAuth(), req, opts)
		if err == nil {
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					err = chunk.Err
				}
				output.Write(chunk.Payload)
				for _, callback := range observe {
					callback(chunk.Payload)
				}
			}
		}
	} else {
		var result coreexecutor.Response
		result, err = e.Execute(t.Context(), bpsFixtureAuth(), req, opts)
		output.Write(result.Payload)
	}
	if invalid {
		if err == nil {
			t.Fatal("invalid terminal accepted")
		}
		if strings.Contains(output.String(), `"name":"spawn_agent"`) {
			t.Fatal("tool escaped invalid terminal")
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), `"name":"spawn_agent"`) {
			t.Fatalf("missing client tool: %s", output.String())
		}
	}
}

func TestBPSDelegationExecutorRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"known", "unknown", "other caller", "invalid terminal", "override", "filter"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, mode), func(t *testing.T) {
				bpsDelegationParent(t, stream, mode == "invalid terminal")
				req := bpsDelegationChildRequest(t)
				opts := bpsDelegationOptions(t, true)
				if mode == "other caller" {
					opts.Metadata[coreexecutor.CallerScopeMetadataKey] = "other/" + t.Name()
				}
				if mode == "unknown" {
					doc := ojson.NewDoc(req.Payload)
					_ = doc.Set("input.0.content.1.encrypted_content", "unknown")
					req.Payload = doc.Bytes()
				}
				opts.OriginalRequest = req.Payload
				captured := make(chan []byte, 1)
				e, _ := newBPSFixture(t, []string{bpsCompletedFixture}, func(raw []byte) { captured <- bytes.Clone(raw) })
				rules := []config.PayloadModelRule{{Name: req.Model, Protocol: "codex"}}
				if mode == "override" {
					e.cfg.Payload.Override = []config.PayloadRule{{Models: rules, Params: map[string]any{"input": []any{map[string]any{"role": "user", "content": "configured input"}}}}}
				}
				if mode == "filter" {
					e.cfg.Payload.Filter = []config.PayloadFilterRule{{Models: rules, Params: []string{"input"}}}
				}
				auth := bpsFixtureAuth()
				auth.ID = "different-account/" + t.Name()
				if _, err := e.Execute(t.Context(), auth, req, opts); err != nil {
					t.Fatal(err)
				}
				raw := <-captured
				doc := ojson.ParseBytes(raw)
				switch mode {
				case "override":
					if !strings.Contains(doc.Get("input").Raw, "configured input") || strings.Contains(doc.Get("input").Raw, "agent_message") {
						t.Fatal("override lost")
					}
				case "filter":
					if doc.Get("input").Exists() {
						t.Fatal("filtered input restored")
					}
				default:
					var message ojson.Result
					for _, item := range doc.Get("input").Array() {
						if item.Get("type").String() == "agent_message" {
							message = item
							break
						}
					}
					want := "encrypted_content"
					if mode == "known" {
						want = "input_text"
					}
					if message.Get("content.1.type").String() != want {
						t.Fatalf("wrong normalization: %s", message.Raw)
					}
					if message.Get("content.2.encrypted_content").String() != "opaque-synthetic-state" {
						t.Fatal("opaque content changed")
					}
				}
			})
		}
	}
}

func TestBPSDelegationCodexPaths(t *testing.T) {
	for _, mode := range []string{"http", "stream", "compact", "websocket", "websocket preparation"} {
		for _, rule := range []string{"none", "override", "filter"} {
			t.Run(mode+"/"+rule, func(t *testing.T) {
				bpsDelegationParent(t, true, false)
				captured := make(chan []byte, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if websocket.IsWebSocketUpgrade(r) {
						conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
						if err != nil {
							t.Error(err)
							return
						}
						defer func() { _ = conn.Close() }()
						_, body, err := conn.ReadMessage()
						if err != nil {
							t.Error(err)
							return
						}
						captured <- body
						_ = conn.WriteMessage(websocket.TextMessage, []byte(bpsCompletedFixture))
						return
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					captured <- body
					if strings.HasSuffix(r.URL.Path, "/compact") {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"id":"resp_compact","object":"response.compaction","output":[]}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+bpsCompletedFixture+"\n\n")
				}))
				defer server.Close()
				cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
				req := bpsDelegationChildRequest(t)
				opts := bpsDelegationOptions(t, true)
				protocol := "codex"
				if mode == "compact" {
					protocol = "openai-response"
				}
				rules := []config.PayloadModelRule{{Name: req.Model, Protocol: protocol}}
				if rule == "override" {
					cfg.Payload.Override = []config.PayloadRule{{Models: rules, Params: map[string]any{"input.0.content.1.text": "configured text"}}}
				}
				if rule == "filter" {
					cfg.Payload.Filter = []config.PayloadFilterRule{{Models: rules, Params: []string{"input"}}}
				}
				auth := codexSIWCAuthForUpdateTest(server.URL)
				e := NewCodexExecutor(cfg)
				var err error
				switch mode {
				case "stream":
					var result *coreexecutor.StreamResult
					result, err = e.ExecuteStream(t.Context(), auth, req, opts)
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				case "compact":
					opts.Alt = "responses/compact"
					_, err = e.Execute(t.Context(), auth, req, opts)
				case "websocket":
					_, err = NewCodexWebsocketsExecutor(cfg).Execute(t.Context(), auth, req, opts)
				case "websocket preparation":
					var prepared *codexWebsocketPrepared
					prepared, err = NewCodexWebsocketsExecutor(cfg).prepareCodexWebsocketStream(t.Context(), auth, req, opts)
					if err == nil {
						captured <- prepared.clientBody
					}
				default:
					_, err = e.Execute(t.Context(), auth, req, opts)
				}
				if err != nil {
					t.Fatal(err)
				}
				doc := ojson.ParseBytes(<-captured)
				if rule == "filter" {
					if doc.Get("input").Exists() {
						t.Fatal("filtered input restored")
					}
					return
				}
				if doc.Get("input.0.content.1.type").String() != "input_text" {
					t.Fatalf("not normalized: %s", doc.Get("input").Raw)
				}
				want := bpsDelegationTask
				if rule == "override" {
					want = "configured text"
				}
				if doc.Get("input.0.content.1.text").String() != want {
					t.Fatal("payload override lost")
				}
				if doc.Get("input.0.content.2.encrypted_content").String() != "opaque-synthetic-state" {
					t.Fatal("opaque content changed")
				}
			})
		}
	}
}

func TestBPSDelegationUnknownHistoryCannotSeedProof(t *testing.T) {
	req := bpsDelegationChildRequest(t)
	doc := ojson.NewDoc(req.Payload)
	if helps.NormalizeBPSDelegationInputDoc(doc, bpsDelegationOptions(t, true)) != 0 {
		t.Fatal("unknown input seeded its own provenance")
	}
}

func TestBPSDelegationImmediateChild(t *testing.T) {
	checked := false
	bpsDelegationParent(t, true, false, func(frame []byte) {
		if checked || !bytes.Contains(frame, []byte("response.output_item.added")) {
			return
		}
		checked = true
		doc := ojson.NewDoc(bpsDelegationChildRequest(t).Payload)
		if helps.NormalizeBPSDelegationInputDoc(doc, bpsDelegationOptions(t, true)) != 1 {
			t.Fatal("first client tool frame preceded provenance")
		}
	})
	if !checked {
		t.Fatal("tool frame not observed")
	}
}

func TestBPSDelegationCanceledParentCannotAuthorizeChild(t *testing.T) {
	args := ojson.NewDocString(`{"task_name":"worker"}`)
	_ = args.Set("message", bpsDelegationTask)
	event := ojson.NewDocString(`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_pending","call_id":"call_pending","name":"collaboration.spawn_agent","status":"completed"}}`)
	_ = event.Set("item.arguments", args.String())
	requestSeen := make(chan struct{})
	e, _ := newBPSFixture(t, []string{event.String()}, func([]byte) { close(requestSeen) })
	req, _ := bpsFixtureRequest(sdktranslator.FormatOpenAIResponse)
	doc := ojson.NewDoc(req.Payload)
	_ = doc.SetRaw("tools", `[{"type":"function","name":"collaboration.spawn_agent","parameters":{"type":"object"}}]`)
	req.Payload = doc.Bytes()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	response, err := e.ExecuteStream(ctx, bpsFixtureAuth(), req, bpsDelegationOptions(t, false))
	if err != nil {
		t.Fatal(err)
	}
	<-requestSeen
	cancel()
	for range response.Chunks {
	}
	child := ojson.NewDoc(bpsDelegationChildRequest(t).Payload)
	before := child.String()
	if helps.NormalizeBPSDelegationInputDoc(child, bpsDelegationOptions(t, true)) != 0 || child.String() != before {
		t.Fatal("canceled unvalidated output authorized a rewrite")
	}
}

func TestBPSDelegationInvalidEncryptedContentIsRequestScoped(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, kind := range []string{"error", "response.failed"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, kind), func(t *testing.T) {
				event := ojson.NewDocString(`{"type":"error","error":{"code":"invalid_encrypted_content","message":"private synthetic upstream content"}}`)
				if kind == "response.failed" {
					_ = event.Set("type", kind)
					_ = event.SetRaw("response.error", event.Get("error").Raw)
					_ = event.Delete("error")
				}
				e, calls := newBPSFixture(t, []string{event.String()}, nil)
				req := bpsDelegationChildRequest(t)
				opts := bpsDelegationOptions(t, true)
				var err error
				if stream {
					var response *coreexecutor.StreamResult
					response, err = e.ExecuteStream(t.Context(), bpsFixtureAuth(), req, opts)
					if err == nil {
						for chunk := range response.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					_, err = e.Execute(t.Context(), bpsFixtureAuth(), req, opts)
				}
				var failure *pluginabi.Error
				if !errors.As(err, &failure) || !failure.IsRequestScoped() || failure.HTTPStatus != 400 || failure.Retryable || failure.CredentialScoped || failure.RetryAfterSeconds != 0 {
					t.Fatalf("wrong error classification: %T", err)
				}
				if calls.Load() != 1 {
					t.Fatal("invalid content was automatically retried")
				}
				if strings.Contains(failure.Message, "private") || !strings.Contains(failure.Message, "valid history") {
					t.Fatal("missing safe recovery guidance")
				}
			})
		}
	}
}
