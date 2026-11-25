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
	remember, err := PrepareBPSHistory(doc(old), t.Name(), &bpsbridge.PrepareOptions{})
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
			if _, err := PrepareBPSHistory(d, tc.scope, &bpsbridge.PrepareOptions{}); err != nil {
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
	rememberFirst, err := PrepareBPSHistory(makeDoc(prefix), t.Name(), &bpsbridge.PrepareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rememberFirst("first", "first-state")
	// The generation's own output is outside its checkpoint's verified input.
	followup := prefix + "," + uncovered + "," + checkpoint
	doc := makeDoc(followup)
	rememberSecond, err := PrepareBPSHistory(doc, t.Name(), &bpsbridge.PrepareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(doc.String(), `"content":"COVERED_OUTPUT"`) || !strings.Contains(doc.String(), "UNCOVERED_OUTPUT") || !strings.Contains(doc.String(), "KEEP_USER") {
		t.Fatal("first checkpoint removed uncovered output or retained covered output")
	}
	rememberSecond("second", "second-state")
	next := makeDoc(followup + `,{"type":"compaction","id":"second","encrypted_content":"second-state"},{"role":"developer","content":"KEEP_DEVELOPER"}`)
	if _, err := PrepareBPSHistory(next, t.Name(), &bpsbridge.PrepareOptions{}); err != nil {
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
