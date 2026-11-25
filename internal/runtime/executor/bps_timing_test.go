package executor

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestBPSExecutorPublishesTTFTAndTransportTiming(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, kind := range []string{"text", "reasoning", "tool", "terminal_only", "error"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, kind), func(t *testing.T) {
				events := []string{
					`{"type":"basispoints.response.metadata","private":"do-not-publish"}`,
					`{"type":"response.created","response":{"id":"resp_bps","status":"in_progress"}}`,
				}
				terminal := ojson.NewDocString(bpsCompletedFixture)
				switch kind {
				case "text":
					events = append(events, `{"type":"response.output_text.delta","delta":"BPS_OK"}`)
				case "reasoning":
					events = append(events, `{"type":"response.reasoning_summary_text.delta","delta":"Considering the request."}`)
				case "tool":
					_ = terminal.SetRaw("response.output", `[{"type":"function_call","id":"fc_test","call_id":"call_test","name":"client_echo","arguments":"{}","status":"completed"}]`)
					events = append(events, `{"type":"response.function_call_arguments.done","item_id":"fc_test","arguments":"{}"}`)
				case "error":
					terminal = ojson.NewDocString(`{"type":"error","error":{"code":"rate_limit_exceeded"}}`)
				}
				events = append(events, terminal.String())
				var wireBytes atomic.Int64
				e, count := newBPSFixture(t, events, func(raw []byte) { wireBytes.Store(int64(len(raw))) })
				auth := bpsFixtureAuth()
				auth.ID = t.Name()
				capture := &bpsUsageCapture{authID: auth.ID, records: make(chan usage.Record, 1)}
				usage.RegisterNamedPlugin(t.Name(), capture)
				req, opts := bpsFixtureRequest(sdktranslator.FormatOpenAIResponse)
				if kind == "tool" {
					doc := ojson.NewDoc(req.Payload)
					_ = doc.SetRaw("tools", `[{"type":"function","name":"client_echo","parameters":{"type":"object"}}]`)
					req.Payload, opts.OriginalRequest = doc.Bytes(), doc.Bytes()
				}
				if stream {
					result, errStream := e.ExecuteStream(t.Context(), auth, req, opts)
					if errStream != nil {
						t.Fatal(errStream)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil && kind != "error" {
							t.Fatal(chunk.Err)
						}
					}
					if result.Headers.Get("X-BPS-Ws-Handshake-Status") != "101" || result.Headers.Get("X-BPS-Upstream-Bytes") != fmt.Sprint(wireBytes.Load()) {
						t.Fatalf("incorrect transport headers: %v", result.Headers)
					}
				} else {
					result, errExecute := e.Execute(t.Context(), auth, req, opts)
					if errExecute != nil && kind != "error" {
						t.Fatal(errExecute)
					}
					if kind != "error" && result.Headers.Get("X-BPS-Upstream-Bytes") != fmt.Sprint(wireBytes.Load()) {
						t.Fatalf("incorrect transport headers: %v", result.Headers)
					}
				}
				select {
				case record := <-capture.records:
					if record.TTFT <= 0 || record.TTFT > record.Latency || record.Failed != (kind == "error") {
						t.Fatalf("invalid timing/status: TTFT=%s latency=%s failed=%t", record.TTFT, record.Latency, record.Failed)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("usage was not published")
				}
				if count.Load() != 1 {
					t.Fatal("observation replayed the request")
				}
			})
		}
	}
}
