package helps

import (
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/bpsbridge"
)

func TestBPSHistoryRequiresMatchingInlineCheckpointAndPrefix(t *testing.T) {
	old := `{"role":"assistant","content":"old"}`
	marker := `{"type":"compaction","id":"checkpoint","encrypted_content":"opaque"}`
	doc := func(items string) *ojson.Doc { return ojson.NewDocString(`{"input":[` + items + `]}`) }
	remember, err := PrepareBPSHistory(doc(old), t.Name(), &bpsbridge.PrepareOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	remember("checkpoint", "opaque")
	for _, tc := range []struct{ name, scope, input string }{
		{"unseen", t.Name(), old + "," + strings.ReplaceAll(marker, "checkpoint", "unknown")},
		{"different caller", t.Name() + "/other", old + "," + marker},
		{"modified checkpoint", t.Name(), old + "," + strings.ReplaceAll(marker, "opaque", "different")},
		{"unfinished checkpoint", t.Name(), old + "," + strings.Replace(marker, "{", `{"status":"in_progress",`, 1)},
		{"edited prefix", t.Name(), strings.ReplaceAll(old, "old", "edited") + "," + marker},
		{"standalone window", t.Name(), `{"role":"user","content":"retained"},` + marker},
		{"unknown later boundary", t.Name(), old + "," + marker + "," + strings.ReplaceAll(marker, "checkpoint", "later")},
		{"trigger only", t.Name(), old + `,{"type":"compaction_trigger"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := doc(tc.input)
			before := d.String()
			if _, err := PrepareBPSHistory(d, tc.scope, &bpsbridge.PrepareOptions{}, nil); err != nil {
				t.Fatal(err)
			}
			if d.String() != before {
				t.Fatal("unverified history was modified")
			}
		})
	}
}

func TestBPSHistoryPreservesUncoveredOutputAcrossCheckpoints(t *testing.T) {
	const prefix = `{"role":"user","content":"KEEP_USER"},{"role":"assistant","content":"COVERED_OUTPUT"}`
	const checkpoint = `{"type":"compaction","id":"first","encrypted_content":"first-state"}`
	const uncovered = `{"role":"assistant","content":"UNCOVERED_OUTPUT"}`
	makeDoc := func(items string) *ojson.Doc { return ojson.NewDocString(`{"input":[` + items + `]}`) }
	rememberFirst, err := PrepareBPSHistory(makeDoc(prefix), t.Name(), &bpsbridge.PrepareOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rememberFirst("first", "first-state")
	// The generation's own output is outside its checkpoint's verified input.
	followup := prefix + "," + uncovered + "," + checkpoint
	doc := makeDoc(followup)
	rememberSecond, err := PrepareBPSHistory(doc, t.Name(), &bpsbridge.PrepareOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(doc.String(), `"content":"COVERED_OUTPUT"`) || !strings.Contains(doc.String(), "UNCOVERED_OUTPUT") || !strings.Contains(doc.String(), "KEEP_USER") {
		t.Fatal("first checkpoint removed uncovered output or retained covered output")
	}
	rememberSecond("second", "second-state")
	next := makeDoc(followup + `,{"type":"compaction","id":"second","encrypted_content":"second-state"},{"role":"developer","content":"KEEP_DEVELOPER"}`)
	if _, err := PrepareBPSHistory(next, t.Name(), &bpsbridge.PrepareOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"COVERED_OUTPUT", "first-state"} {
		if strings.Contains(next.String(), removed) {
			t.Fatalf("second checkpoint retained covered history: %s", removed)
		}
	}
	for _, kept := range []string{"KEEP_USER", "KEEP_DEVELOPER", "second-state"} {
		if !strings.Contains(next.String(), kept) {
			t.Fatalf("second checkpoint removed required history: %s", kept)
		}
	}
}

func TestBPSHistoryDiagnostics(t *testing.T) {
	const old = `{"role":"assistant","content":"private-old"}`
	const marker = `{"type":"compaction","id":"private-checkpoint","encrypted_content":"private-state"}`
	makeDoc := func(input string) *ojson.Doc { return ojson.NewDocString(`{"input":[` + input + `]}`) }
	initial := NewBPSObservation(0)
	remember, err := PrepareBPSHistory(makeDoc(old), t.Name(), &bpsbridge.PrepareOptions{}, initial)
	if err != nil {
		t.Fatal(err)
	}
	remember("private-checkpoint", "private-state")
	if initial.fields()["compaction_confirmation"] != "confirmed" {
		t.Fatal("missing checkpoint confirmation")
	}
	for _, tc := range []struct{ name, scope, input, status string }{
		{"trim", t.Name(), old + "," + marker, "trimmed"},
		{"unknown", t.Name() + "/other", old + "," + marker, "checkpoint_not_found"},
		{"prefix", t.Name(), strings.ReplaceAll(old, "private-old", "edited") + "," + marker, "prefix_mismatch"},
		{"invalid", t.Name(), old + "," + strings.Replace(marker, "{", `{"status":"in_progress",`, 1), "invalid_checkpoint"},
		{"latest unknown", t.Name(), old + "," + marker + "," + strings.ReplaceAll(marker, "private-checkpoint", "later"), "checkpoint_not_found"},
		{"absent", t.Name(), old, "no_checkpoint"},
		{"leading checkpoint", t.Name(), marker, "checkpoint_without_prefix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := NewBPSObservation(0)
			doc := makeDoc(tc.input)
			before := doc.String()
			if _, err := PrepareBPSHistory(doc, tc.scope, &bpsbridge.PrepareOptions{}, o); err != nil {
				t.Fatal(err)
			}
			fields := o.fields()
			if fields["history_status"] != tc.status {
				t.Fatalf("history status = %v, want %s", fields["history_status"], tc.status)
			}
			if tc.status == "trimmed" {
				if fields["history_items_before"] != 2 || fields["history_items_after"] != 1 || fields["history_covered_items"] != 1 {
					t.Fatalf("incorrect trim counts: %v", fields)
				}
			} else if doc.String() != before {
				t.Fatal("diagnostics changed unverified history")
			}
			for _, value := range fields {
				if text, ok := value.(string); ok && strings.Contains(text, "private-") {
					t.Fatal("history diagnostic leaked checkpoint or prompt")
				}
			}
		})
	}
}
