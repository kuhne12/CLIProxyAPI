package helps

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/bpsbridge"
)

func TestBPSHistoryRequiresMatchingInlineCheckpointAndPrefix(t *testing.T) {
	old := `{"role":"assistant","content":"old"}`
	marker := `{"type":"compaction","id":"checkpoint","encrypted_content":"opaque"}`
	doc := func(items string) *ojson.Doc { return ojson.NewDocString(`{"input":[` + items + `]}`) }
	remember, err := PrepareBPSHistory(t.Context(), doc(old), t.Name(), &bpsbridge.PrepareOptions{}, nil)
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
			if _, err := PrepareBPSHistory(t.Context(), d, tc.scope, &bpsbridge.PrepareOptions{}, nil); err != nil {
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
	rememberFirst, err := PrepareBPSHistory(t.Context(), makeDoc(prefix), t.Name(), &bpsbridge.PrepareOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rememberFirst("first", "first-state")
	// The generation's own output is outside its checkpoint's verified input.
	followup := prefix + "," + uncovered + "," + checkpoint
	doc := makeDoc(followup)
	rememberSecond, err := PrepareBPSHistory(t.Context(), doc, t.Name(), &bpsbridge.PrepareOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(doc.String(), `"content":"COVERED_OUTPUT"`) || !strings.Contains(doc.String(), "UNCOVERED_OUTPUT") || !strings.Contains(doc.String(), "KEEP_USER") {
		t.Fatal("first checkpoint removed uncovered output or retained covered output")
	}
	rememberSecond("second", "second-state")
	next := makeDoc(followup + `,{"type":"compaction","id":"second","encrypted_content":"second-state"},{"role":"developer","content":"KEEP_DEVELOPER"}`)
	if _, err := PrepareBPSHistory(t.Context(), next, t.Name(), &bpsbridge.PrepareOptions{}, nil); err != nil {
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

func TestBPSCheckpointPersistenceRequiresOriginalVerifiedPrefix(t *testing.T) {
	if err := kvstore.ConfigureLocal(true, filepath.Join(t.TempDir(), "kv.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := kvstore.ShutdownLocal(); err != nil {
			t.Error(err)
		}
	})
	const marker = `{"type":"compaction","id":"checkpoint","encrypted_content":"opaque"}`
	const old = `{"role":"assistant","content":"old"}`
	const edited = `{"role":"assistant","content":"edited"}`
	prepare := func(input string) (*ojson.Doc, func(string, string)) {
		t.Helper()
		doc := ojson.NewDocString(`{"input":[` + input + `]}`)
		remember, err := PrepareBPSHistory(t.Context(), doc, t.Name(), &bpsbridge.PrepareOptions{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return doc, remember
	}
	key := bpsCheckpointKey(t.Name(), "checkpoint", "opaque")
	_, echoed := prepare(old + "," + marker)
	echoed("checkpoint", "opaque")
	if _, source := lookupBPSCheckpoint(t.Context(), key); source != "kv_miss" {
		t.Fatalf("echoed checkpoint lookup = %s, want kv_miss", source)
	}
	_, first := prepare(old)
	first("checkpoint", "opaque")
	_, duplicate := prepare(edited)
	duplicate("checkpoint", "opaque")
	if doc, _ := prepare(edited + "," + marker); len(doc.Get("input").Array()) != 2 {
		t.Fatal("a second confirmation replaced the original prefix")
	}
	if doc, _ := prepare(old + "," + marker); len(doc.Get("input").Array()) != 1 {
		t.Fatal("original verified prefix was not recovered")
	}
	if _, err := kvstore.KVSetBytesRequired(t.Context(), key, []byte("truncated"), bpsCheckpointTTL); err != nil {
		t.Fatal(err)
	}
	if doc, _ := prepare(old + "," + marker); len(doc.Get("input").Array()) != 2 {
		t.Fatal("corrupt checkpoint record removed history")
	}
}

func TestBPSHistoryDiagnostics(t *testing.T) {
	const old = `{"role":"assistant","content":"private-old"}`
	const marker = `{"type":"compaction","id":"private-checkpoint","encrypted_content":"private-state"}`
	makeDoc := func(input string) *ojson.Doc { return ojson.NewDocString(`{"input":[` + input + `]}`) }
	initial := NewBPSObservation(0)
	remember, err := PrepareBPSHistory(t.Context(), makeDoc(old), t.Name(), &bpsbridge.PrepareOptions{}, initial)
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
			if _, err := PrepareBPSHistory(t.Context(), doc, tc.scope, &bpsbridge.PrepareOptions{}, o); err != nil {
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
