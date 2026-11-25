package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/coder/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const bpsCompletedFixture = `{"type":"response.completed","response":{"id":"resp_bps","object":"response","created_at":1,"model":"gpt-6-astra","status":"completed","output":[{"id":"msg_bps","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"BPS_OK","annotations":[]}]}],"usage":{"input_tokens":12,"output_tokens":2,"total_tokens":14}}}`

func bpsFixtureAuth() *coreauth.Auth {
	return &coreauth.Auth{ID: "bps-test.json", Provider: "bps", Metadata: map[string]any{"access_token": "fixture-token", "account_id": "fixture-account"}}
}
func bpsFixtureRequest(format sdktranslator.Format) (coreexecutor.Request, coreexecutor.Options) {
	payload := []byte(`{"model":"gpt-6-astra","input":"Say BPS_OK","reasoning":{"effort":"low"}}`)
	if format == sdktranslator.FormatOpenAI {
		payload = []byte(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"Say BPS_OK"}],"reasoning_effort":"low"}`)
	}
	return coreexecutor.Request{Model: "gpt-6-astra", Format: format, Payload: payload}, coreexecutor.Options{SourceFormat: format, OriginalRequest: payload}
}

func newBPSFixture(t *testing.T, events []string, inspect func([]byte)) (*BPSExecutor, *atomic.Int32) {
	t.Helper()
	count := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if r.Header.Get("Authorization") != "" || !strings.Contains(r.Header.Get("Sec-WebSocket-Protocol"), "openai-bearer.fixture-token") {
			t.Error("BPS authentication was not sent via the bearer subprotocol")
		}
		if r.URL.Query().Get("bps_auth_mode") != "chatgpt" || r.Header.Get("Origin") != "https://bps.openai.com" {
			t.Error("missing BPS handshake metadata")
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, Subprotocols: []string{"responses"}})
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, payload, errRead := conn.Read(r.Context())
		if errRead != nil {
			t.Error(errRead)
			return
		}
		if inspect != nil {
			inspect(payload)
		}
		for _, event := range events {
			if errWrite := conn.Write(r.Context(), websocket.MessageText, []byte(event)); errWrite != nil {
				return
			}
		}
		// Wait for executor closure, avoiding a server-side close race with the terminal frame.
		_, _, _ = conn.Read(r.Context())
	}))
	t.Cleanup(server.Close)
	e := NewBPSExecutor(&config.Config{})
	e.endpoint = server.URL
	return e, count
}

func TestBPSExecutorChatAndResponses(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse} {
		t.Run(format.String(), func(t *testing.T) {
			e, _ := newBPSFixture(t, []string{bpsCompletedFixture}, func(raw []byte) {
				doc := ojson.ParseBytes(raw)
				if doc.Get("model").String() != "gpt-6-astra" || doc.Get("reasoning_effort").String() != "low" || doc.Get("type").String() != "response.create" {
					t.Errorf("incorrect BPS payload: %s", raw)
				}
				if len(doc.Get("metadata.task_id").String()) != 64 || len(doc.Get("metadata.turn_id").String()) != 64 {
					t.Error("missing stable task identity")
				}
				if !strings.Contains(doc.Get("input").Raw, "Say BPS_OK") {
					t.Error("lost user prompt")
				}
			})
			req, opts := bpsFixtureRequest(format)
			response, err := e.Execute(t.Context(), bpsFixtureAuth(), req, opts)
			if err != nil {
				t.Fatal(err)
			}
			field := "output.0.content.0.text"
			if format == sdktranslator.FormatOpenAI {
				field = "choices.0.message.content"
			}
			if got := ojson.GetBytes(response.Payload, field).String(); got != "BPS_OK" {
				t.Fatalf("incorrect response: %s", response.Payload)
			}
		})
	}
}

func TestBPSExecutorFinalPayloadRules(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			calls := 0
			e, _ := newBPSFixture(t, []string{bpsCompletedFixture}, func(raw []byte) {
				calls++
				doc := ojson.ParseBytes(raw)
				if doc.Get("reasoning_effort").String() != "high" || doc.Get("metadata.task_id").String() != "user-configured-task" || !doc.Get("store").Bool() {
					t.Errorf("BPS conversion overwrote final rules: %s", raw)
				}
				if doc.Get("context_management").Exists() || doc.Get("input").Exists() {
					t.Errorf("BPS restored filtered fields: %s", raw)
				}
			})
			match := []config.PayloadModelRule{{Name: "gpt-6-astra", Protocol: "codex", FromProtocol: "openai-response"}}
			e.cfg.Payload = config.PayloadConfig{Override: []config.PayloadRule{{Models: match, Params: map[string]any{"reasoning_effort": "high", "metadata.task_id": "user-configured-task", "store": true}}}, Filter: []config.PayloadFilterRule{{Models: match, Params: []string{"context_management", "input"}}}}
			req, opts := bpsFixtureRequest(sdktranslator.FormatOpenAIResponse)
			if stream {
				response, err := e.ExecuteStream(t.Context(), bpsFixtureAuth(), req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range response.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else if _, err := e.Execute(t.Context(), bpsFixtureAuth(), req, opts); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("expected one upstream request, got %d", calls)
			}
		})
	}
}

func TestBPSExecutorStreamsTextAndFiltersTransportMetadata(t *testing.T) {
	e, _ := newBPSFixture(t, []string{
		`{"type":"basispoints.response.resume_token","token":"private-resume"}`,
		`{"type":"response.created","response":{"id":"resp_bps","model":"gpt-6-astra","status":"in_progress"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_bps","output_index":0,"content_index":0,"delta":"BPS_OK","basispoints_replay_cursor":"private-cursor"}`,
		bpsCompletedFixture,
	}, nil)
	req, opts := bpsFixtureRequest(sdktranslator.FormatOpenAI)
	result, err := e.ExecuteStream(t.Context(), bpsFixtureAuth(), req, opts)
	if err != nil {
		t.Fatal(err)
	}
	var all bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		all.Write(chunk.Payload)
	}
	if !strings.Contains(all.String(), "BPS_OK") || strings.Contains(all.String(), "private-") {
		t.Fatalf("bad stream: %s", all.String())
	}
}

func TestBPSExecutorReportsLimitsAndDoesNotReplay(t *testing.T) {
	for _, stream := range []bool{false, true} {
		e, count := newBPSFixture(t, []string{`{"type":"error","error":{"code":"rate_limit_exceeded","message":"echo fixture-token"}}`}, nil)
		req, opts := bpsFixtureRequest(sdktranslator.FormatOpenAIResponse)
		var err error
		if stream {
			result, errOpen := e.ExecuteStream(t.Context(), bpsFixtureAuth(), req, opts)
			if errOpen != nil {
				t.Fatal(errOpen)
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					err = chunk.Err
				}
			}
		} else {
			_, err = e.Execute(t.Context(), bpsFixtureAuth(), req, opts)
		}
		var failure *pluginabi.Error
		if !errors.As(err, &failure) || failure.HTTPStatus != 429 || failure.Code != pluginabi.ErrorCodeTransientRateLimit || strings.Contains(err.Error(), "fixture-token") {
			t.Fatalf("limit was lost or leaked: %v", err)
		}
		if count.Load() != 1 {
			t.Fatal("sent generation was replayed")
		}
	}
}

func TestBPSExecutorCancellationClosesSocket(t *testing.T) {
	received := make(chan struct{})
	closed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, Subprotocols: []string{"responses"}})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow(); close(closed) }()
		_, _, _ = conn.Read(r.Context())
		close(received)
		_, _, _ = conn.Read(r.Context())
	}))
	defer server.Close()
	e := NewBPSExecutor(&config.Config{})
	e.endpoint = server.URL
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, opts := bpsFixtureRequest(sdktranslator.FormatOpenAIResponse)
	result, err := e.ExecuteStream(ctx, bpsFixtureAuth(), req, opts)
	if err != nil {
		t.Fatal(err)
	}
	<-received
	cancel()
	for range result.Chunks {
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not close upstream socket")
	}
}

func TestBPSExecutorReasoningSuffixUsesCanonicalPipeline(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("bps-thinking-test", "bps", []*registry.ModelInfo{{ID: "gpt-6-astra", Type: "bps", Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}}}})
	defer reg.UnregisterClient("bps-thinking-test")
	e, _ := newBPSFixture(t, []string{bpsCompletedFixture}, func(raw []byte) {
		if got := ojson.GetBytes(raw, "reasoning_effort").String(); got != "high" {
			t.Errorf("suffix effort = %q", got)
		}
	})
	req, opts := bpsFixtureRequest(sdktranslator.FormatOpenAIResponse)
	req.Model += "(high)"
	if _, err := e.Execute(t.Context(), bpsFixtureAuth(), req, opts); err != nil {
		t.Fatal(err)
	}
}

func TestBPSExecutorRestoresDeclaredClientTool(t *testing.T) {
	native := `{"type":"response.completed","response":{"id":"resp_tool","model":"gpt-6-astra","status":"completed","output":[{"type":"function_call","id":"fc_native","call_id":"call_echo","name":"run_officejs","arguments":"{\"code\":\"{\\\"name\\\":\\\"echo\\\",\\\"arguments\\\":{\\\"text\\\":\\\"hello\\\"}}\"}"}]}}`
	e, _ := newBPSFixture(t, []string{native}, nil)
	req, opts := bpsFixtureRequest(sdktranslator.FormatOpenAIResponse)
	req.Payload = []byte(`{"model":"gpt-6-astra","input":"Use echo","tools":[{"type":"function","name":"echo","parameters":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}]}`)
	opts.OriginalRequest = req.Payload
	result, err := e.Execute(t.Context(), bpsFixtureAuth(), req, opts)
	if err != nil {
		t.Fatal(err)
	}
	if ojson.GetBytes(result.Payload, "output.0.name").String() != "echo" || ojson.GetBytes(result.Payload, "output.0.call_id").String() != "call_echo" {
		t.Fatalf("native tool was not translated: %s", result.Payload)
	}
}
