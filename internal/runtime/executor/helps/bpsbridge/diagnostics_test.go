package bpsbridge

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
)

func TestProtocolDiagnosticsPreserveFailureAndRedactContent(t *testing.T) {
	const marker = `{"type":"compaction","id":"private-checkpoint","encrypted_content":"private-state"}`
	const valid = `{"type":"function_call","name":"lookup","call_id":"private-call","id":"private-item","arguments":"{}"}`
	const unsupported = `{"type":"function_call","name":"read_ranges","call_id":"private-call","id":"private-item","arguments":"private-arguments"}`
	for _, tc := range []struct {
		name, tool, reason, stage string
		omitted, structured       bool
	}{
		{name: "valid", tool: valid},
		{name: "unsupported native", tool: unsupported, reason: "unsupported native tool", stage: "tool_translation"},
		{name: "malformed transport", tool: `{"type":"function_call","name":"run_officejs","arguments":"private-arguments"}`, reason: "invalid tool transport arguments", stage: "tool_translation"},
		{name: "malformed envelope", tool: `{"type":"function_call","name":"run_officejs","arguments":"{\"code\":\"private-code(\"}"}`, reason: "one JSON client-tool envelope", stage: "tool_translation"},
		{name: "outside catalog", tool: `{"type":"function_call","name":"run_officejs","arguments":"{\"code\":\"{\\\"name\\\":\\\"unknown_tool\\\",\\\"arguments\\\":{}}\"}"}`, reason: "outside the client's catalog", stage: "tool_translation"},
		{name: "missing call identity", tool: `{"type":"function_call","name":"lookup","arguments":"{}"}`, reason: "missing call_id", stage: "tool_translation"},
		{name: "kind mismatch", tool: `{"type":"custom_tool_call","name":"lookup","input":"private-code"}`, reason: "incompatible call type", stage: "tool_translation"},
		{name: "omitted tool", tool: unsupported, omitted: true, reason: "omitted an original tool item", stage: "terminal_tool_completeness"},
		{name: "structured output", structured: true, reason: "not one valid JSON value", stage: "structured_output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := `{"model":"gpt-6-astra","input":"private-prompt","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`
			if tc.structured {
				request = `{"model":"gpt-6-astra","input":"private-prompt","text":{"format":{"type":"json_object"}}}`
			}
			var upstream strings.Builder
			fmt.Fprintf(&upstream, "data: {\"type\":\"response.output_item.done\",\"item\":%s}\n\n", marker)
			output := marker
			if tc.tool != "" {
				fmt.Fprintf(&upstream, "data: {\"type\":\"response.output_item.done\",\"item\":%s}\n\n", tc.tool)
				if !tc.omitted {
					output += "," + tc.tool
				}
			}
			if tc.structured {
				output += `,{"type":"message","content":[{"type":"output_text","text":"private-answer"}]}`
			}
			fmt.Fprintf(&upstream, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[%s]}}\n\n", output)
			var observed []ProtocolDiagnostic
			var outputs [][]byte
			for _, enabled := range []bool{false, true} {
				_, bridge, err := Prepare([]byte(request), "diagnostic", nil)
				if err != nil {
					t.Fatal(err)
				}
				confirmed := 0
				bridge.OnInlineCompaction = func(_, _ string) { confirmed++ }
				if enabled {
					bridge.OnProtocolError = func(d ProtocolDiagnostic) { observed = append(observed, d) }
				}
				body := bridge.Stream(io.NopCloser(strings.NewReader(upstream.String())))
				raw, errRead := io.ReadAll(body)
				if errClose := body.Close(); errClose != nil {
					t.Fatal(errClose)
				}
				if errRead != nil {
					t.Fatal(errRead)
				}
				outputs = append(outputs, raw)
				wantConfirmed := 0
				if tc.reason == "" {
					wantConfirmed = 1
				}
				if confirmed != wantConfirmed {
					t.Fatalf("checkpoint confirmation = %d, want %d", confirmed, wantConfirmed)
				}
				if tc.reason != "" {
					var failed bool
					if errRead := readEvents(bytes.NewReader(raw), func(_ string, data []byte) error {
						if errEvent := EventError(data); errEvent != nil {
							failed = errEvent.Error() == "Upstream returned an unsupported response or tool call"
						}
						return nil
					}); errRead != nil || !failed {
						t.Fatalf("public failure changed: %v", errRead)
					}
				}
			}
			if !bytes.Equal(outputs[0], outputs[1]) {
				t.Fatal("diagnostic callback changed the client response")
			}
			if tc.reason == "" {
				if len(observed) != 0 {
					t.Fatal("success reported a validation failure")
				}
				return
			}
			if len(observed) != 1 {
				t.Fatalf("diagnostic count = %d", len(observed))
			}
			d := observed[0]
			if !strings.Contains(d.Reason, tc.reason) || d.Stage != tc.stage || !d.CompactionSeen || d.EventType != "response.completed" {
				t.Fatalf("incorrect diagnostic: %+v", d)
			}
			if tc.omitted && (d.PendingTools != 1 || len(d.MissingTools) != 1) {
				t.Fatal("missing tool identity was not diagnosed")
			}
			if tc.name == "outside catalog" && d.Tool.ClientName != "unknown_tool" {
				t.Fatalf("lost target tool: %+v", d.Tool)
			}
			raw, _ := json.Marshal(d)
			if strings.Contains(string(raw), "private-") {
				t.Fatalf("diagnostic leaked content: %s", raw)
			}
		})
	}
}

func TestProtocolDiagnosticBoundsAndUnknownErrors(t *testing.T) {
	b := &Bridge{tools: make(map[string]tool)}
	for i := 0; i < 80; i++ {
		b.tools[fmt.Sprintf("tool_%03d", i)] = tool{Kind: "function", Definition: "private-schema"}
	}
	first := b.CatalogDiagnostic()
	if first.Count != 80 || len(first.Names) != 64 || len(first.Hash) != 64 || b.CatalogDiagnostic().Hash != first.Hash {
		t.Fatal("catalog diagnostic is unbounded or unstable")
	}
	b.tools["tool_079"] = tool{Kind: "custom", Definition: "other-schema"}
	if first.Hash == b.CatalogDiagnostic().Hash {
		t.Fatal("catalog fingerprint ignored a contract outside the visible sample")
	}
	var d ProtocolDiagnostic
	b.OnProtocolError = func(value ProtocolDiagnostic) { d = value }
	pending := make(map[string]string)
	for i := 0; i < 20; i++ {
		pending[fmt.Sprintf("private-call-%d", i)] = "lookup"
	}
	b.reportProtocolError(errors.New("private-parser-error"), ProtocolDiagnostic{}, pending)
	if len(d.MissingTools) != 8 || d.PendingTools != 20 || strings.Contains(d.Reason, "private-") {
		t.Fatalf("unsafe unknown diagnostic: %+v", d)
	}
	for _, label := range []string{"private-name\nforged log", strings.Repeat("x", 129)} {
		if value := diagnosticLabel(label); !strings.HasPrefix(value, "sha256:") || len(value) != 71 {
			t.Fatal("unbounded or multiline label was not hashed")
		}
	}
}

func TestCatalogDiagnosticIncludesHelperDescriptions(t *testing.T) {
	var hashes []string
	for _, helper := range []string{"exec_command", "write_stdin"} {
		request := fmt.Sprintf(`{"model":"gpt-6-astra","input":"test","tools":[{"type":"custom","name":"exec","description":"private helper %s"}]}`, helper)
		_, bridge, err := Prepare([]byte(request), "scope", nil)
		if err != nil {
			t.Fatal(err)
		}
		catalog := bridge.CatalogDiagnostic()
		hashes = append(hashes, catalog.Hash)
		raw, _ := json.Marshal(catalog)
		if strings.Contains(string(raw), "private helper") {
			t.Fatal("catalog retained a tool description")
		}
	}
	if hashes[0] == hashes[1] {
		t.Fatal("catalog fingerprint ignored changed exec helper declarations")
	}
}
