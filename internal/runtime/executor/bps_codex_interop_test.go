package executor

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestBPSCodexToolHandoff(t *testing.T) {
	const tool = `{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec","format":{"type":"text"}}]}`
	const checkpoint = `{"type":"compaction","id":"cmp_handoff","encrypted_content":"opaque-checkpoint"}`
	const relay = `{"type":"function_call","name":"run_officejs","id":"fc_bps","call_id":"call_bps","arguments":"{\"summary\":\"codex2api.custom/functions.exec\",\"code\":\"first()\"}"}`
	const native = `{"type":"custom_tool_call","namespace":"functions","name":"exec","id":"ctc_codex","call_id":"call_codex","input":"second(\"a\\nb\")","status":"completed"}`
	event := func(output string) string {
		doc := ojson.NewDocString(bpsCompletedFixture)
		if err := doc.SetRaw("response.output", output); err != nil {
			t.Fatal(err)
		}
		return doc.String()
	}
	history := ojson.NewDocString(`{"model":"gpt-6-astra","input":[{"type":"additional_tools","id":"at_catalog","role":"developer","tools":[` + tool + `]},{"role":"user","content":"Use the tool."}]}`)
	appendItem := func(raw string) {
		if err := history.SetRaw("input.-1", raw); err != nil {
			t.Fatal(err)
		}
	}
	req, opts := bpsFixtureRequest(sdktranslator.FormatOpenAIResponse)
	refresh := func() {
		req.Payload = history.Bytes()
		opts.OriginalRequest = req.Payload
	}

	// Produce a real client-facing checkpoint and call through the BPS executor.
	first, _ := newBPSFixture(t, []string{event("[" + checkpoint + "," + relay + "]")}, nil)
	refresh()
	res, err := first.Execute(t.Context(), bpsFixtureAuth(), req, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range ojson.GetBytes(res.Payload, "output").Array() {
		appendItem(item.Raw)
	}
	appendItem(`{"type":"custom_tool_call_output","call_id":"call_bps","output":"first result"}`)
	refresh()

	nativeEvent := event("[" + native + "]")
	wire := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Error(errRead)
		}
		wire <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", nativeEvent)
	}))
	defer server.Close()
	cfg := &config.Config{}
	codex := NewCodexExecutor(cfg)
	auth := &coreauth.Auth{Provider: "codex", Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	res, err = codex.Execute(t.Context(), auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	codexInput := ojson.GetBytes(<-wire, "input").Array()
	if len(codexInput) != 6 || codexInput[2].Get("type").String() != "compaction" || codexInput[3].Get("tools.0.tools.0.name").String() != "exec" {
		t.Fatalf("missing native tool declaration after checkpoint: %v", codexInput)
	}
	if codexInput[4].Get("namespace").String() != "functions" || codexInput[4].Get("input").String() != "first()" || codexInput[5].Get("call_id").String() != codexInput[4].Get("call_id").String() {
		t.Fatal("BPS tool identity or result changed on the Codex wire")
	}
	socket := NewCodexWebsocketsExecutor(cfg)
	prepared, err := socket.prepareCodexWebsocketStream(t.Context(), auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	if ojson.GetBytes(prepared.clientBody, "input.3.tools.0.tools.0.name").String() != "exec" {
		t.Fatal("missing native tool declaration on WebSocket continuation")
	}
	appendItem(ojson.GetBytes(res.Payload, "output.0").Raw)
	appendItem(`{"type":"custom_tool_call_output","call_id":"call_codex","output":"second result"}`)
	refresh()

	// The next BPS account has no replay cache for the native Codex call.
	last, _ := newBPSFixture(t, []string{bpsCompletedFixture}, func(raw []byte) { wire <- raw })
	bpsAuth := bpsFixtureAuth()
	bpsAuth.ID = "cold-bps-account"
	if _, err = last.Execute(t.Context(), bpsAuth, req, opts); err != nil {
		t.Fatal(err)
	}
	bpsInput := ojson.GetBytes(<-wire, "input").Array()
	foundCheckpoint, foundCatalog := false, false
	calls, results := make(map[string]int), make(map[string]int)
	for _, item := range bpsInput {
		switch item.Get("type").String() {
		case "compaction":
			foundCheckpoint = true
		case "message":
			if foundCheckpoint && strings.Contains(item.Get("content.0.text").String(), `Client tool "functions.exec" (custom)`) {
				foundCatalog = true
			}
		case "function_call":
			calls[item.Get("call_id").String()]++
			if item.Get("call_id").String() == "call_codex" {
				args := ojson.Parse(item.Get("arguments").String())
				if item.Get("name").String() != "run_officejs" || args.Get("code").String() != ojson.Get(native, "input").String() || args.Get("summary").String() != "codex2api.custom/functions.exec" {
					t.Fatal("native custom call did not replay with its raw input and marker")
				}
			}
		case "function_call_output":
			results[item.Get("call_id").String()]++
			if item.Get("call_id").String() == "call_codex" && item.Get("output").String() != "second result" {
				t.Fatal("lost native tool result")
			}
		}
	}
	if !foundCatalog || len(calls) != 2 || calls["call_codex"] != 1 || calls["call_bps"] != 1 || len(results) != 2 || results["call_codex"] != 1 || results["call_bps"] != 1 {
		t.Fatalf("BPS active catalog=%t, calls=%v, results=%v", foundCatalog, calls, results)
	}

	// Filters applied to the final business payload win on a streamed continuation.
	cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: req.Model, Protocol: "codex"}}, Params: []string{"input"}}}
	stream, err := codex.ExecuteStream(t.Context(), auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	for item := range stream.Chunks {
		if item.Err != nil {
			t.Fatal(item.Err)
		}
	}
	if ojson.GetBytes(<-wire, "input").Exists() {
		t.Fatal("tool restoration reversed the configured input filter")
	}
}
