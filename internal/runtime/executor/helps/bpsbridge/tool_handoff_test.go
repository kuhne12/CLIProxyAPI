package bpsbridge

import (
	"bytes"
	"io"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

type toolHandoffWriter struct {
	t        *testing.T
	handoffs *int
	sawTool  bool
}

func (w *toolHandoffWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("response.output_item.added")) {
		w.sawTool = true
		if *w.handoffs != 1 {
			w.t.Fatal("tool frame preceded provenance handoff")
		}
	}
	return len(p), nil
}
func TestValidatedToolHandoffOrdering(t *testing.T) {
	_, bridge := recoveryFixture(t)
	count := 0
	bridge.OnToolHandoff = func(raw []byte) {
		count++
		event := ojson.ParseBytes(raw)
		if event.Get("type").String() != "response.output_item.done" || event.Get("item.status").String() != "completed" || event.Get("item.name").String() != "lookup" || event.Get("item.arguments").String() != "{}" {
			t.Fatal("handoff did not contain validated client tool")
		}
	}
	writer := &toolHandoffWriter{t: t, handoffs: &count}
	if err := bridge.transform(recoverySSE(t, recoveryTerminal("resp", []any{recoveryTool("lookup", "one")}, 1, 1)), writer); err != nil {
		t.Fatal(err)
	}
	if !writer.sawTool || count != 1 {
		t.Fatal("missing or duplicate handoff")
	}
}
func TestValidatedToolHandoffRejectsIncompleteAndInvalidOutput(t *testing.T) {
	for _, mode := range []string{"failed", "incomplete", "invalid second tool", "missing terminal tool", "missing terminal"} {
		t.Run(mode, func(t *testing.T) {
			_, bridge := recoveryFixture(t)
			count := 0
			bridge.OnToolHandoff = func([]byte) { count++ }
			tool := recoveryTool("lookup", "one")
			terminal := recoveryTerminal("resp", []any{tool}, 1, 1)
			events := []object{{"type": "response.output_item.done", "item": tool}}
			switch mode {
			case "failed", "incomplete":
				terminal["type"] = "response." + mode
				terminal["response"].(object)["status"] = mode
			case "invalid second tool":
				terminal["response"].(object)["output"] = []any{tool, recoveryTool("not_in_catalog", "bad")}
			case "missing terminal tool":
				terminal["response"].(object)["output"] = []any{}
			}
			if mode != "missing terminal" {
				events = append(events, terminal)
			}
			_ = bridge.transform(recoverySSE(t, events...), io.Discard)
			if count != 0 {
				t.Fatal("unvalidated output created provenance")
			}
		})
	}
}
