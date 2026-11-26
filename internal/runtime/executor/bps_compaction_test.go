package executor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestBPSInlineCompactionReplay(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			// Older native checkpoints and completed work should disappear from
			// the next create, while a tool result may still refer to an old call.
			prefix := `{"type":"additional_tools","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]},
				{"role":"developer","content":"KEEP_INSTRUCTION"},
				{"role":"user","content":"KEEP_USER"},
				{"type":"compaction","id":"old","encrypted_content":"OLD_CHECKPOINT"},
				{"type":"reasoning","encrypted_content":"OLD_REASONING"},
				{"type":"custom_tool_call","name":"edit","call_id":"old-call","input":"old input"},
				{"type":"custom_tool_call_output","call_id":"old-call","output":"` + strings.Repeat("OLD_RESULT", 1200) + `"},
				{"type":"function_call","name":"lookup","call_id":"cross-boundary","arguments":"{}"}`
			marker := `{"type":"compaction","id":"inline","encrypted_content":"OPAQUE"}`
			completed := ojson.NewDocString(bpsCompletedFixture)
			if err := completed.SetRaw("response.output", "["+marker+"]"); err != nil {
				t.Fatal(err)
			}
			wire := make(chan []byte, 1)
			e, _ := newBPSFixture(t, []string{completed.String()}, func(raw []byte) { wire <- raw })
			auth := bpsFixtureAuth()
			auth.ID = "bps-checkpoint-test"
			identity := map[string]any{coreexecutor.CallerScopeMetadataKey: "caller", coreexecutor.CanonicalSessionIDMetadataKey: "session"}
			send := func(input string) []byte {
				t.Helper()
				req, opts := bpsFixtureRequest(sdktranslator.FormatOpenAIResponse)
				opts.Metadata = identity
				doc := ojson.NewDoc(req.Payload)
				if err := doc.SetRaw("input", "["+input+"]"); err != nil {
					t.Fatal(err)
				}
				req.Payload = doc.Bytes()
				opts.OriginalRequest = req.Payload
				if stream {
					result, err := e.ExecuteStream(t.Context(), auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := e.Execute(t.Context(), auth, req, opts); err != nil {
					t.Fatal(err)
				}
				return <-wire
			}
			// Only the child sees the originating request. The parent starts with
			// cold process state and reopens exactly the same database.
			const seedEnv = "CPA_TEST_BPS_CHECKPOINT_SEED_DB"
			dbPath := os.Getenv(seedEnv)
			seed := dbPath != ""
			if !seed {
				dbPath = filepath.Join(t.TempDir(), "kv.db")
				cmd := exec.CommandContext(t.Context(), os.Args[0], fmt.Sprintf("-test.run=^TestBPSInlineCompactionReplay$/^stream=%t$", stream))
				cmd.Env = append(os.Environ(), seedEnv+"="+dbPath)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("seed process: %v\n%s", err, out)
				}
			}
			if err := kvstore.ConfigureLocal(true, dbPath); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := kvstore.ShutdownLocal(); err != nil {
					t.Error(err)
				}
			})
			if seed {
				if !strings.Contains(string(send(prefix)), "OLD_RESULT") {
					t.Fatal("originating request lost history")
				}
				return
			}
			followup := prefix + "," + marker + `,{"type":"function_call_output","call_id":"cross-boundary","output":"late result"},{"role":"developer","content":"next instruction"}`
			// Use a separate credential to measure the same follow-up on a miss.
			originalID := auth.ID
			auth.ID += "/cold"
			full := send(followup)
			auth.ID = originalID
			if !strings.Contains(string(full), "OLD_RESULT") {
				t.Fatal("unverified history was dropped")
			}
			for _, field := range []string{coreexecutor.CallerScopeMetadataKey, coreexecutor.CanonicalSessionIDMetadataKey} {
				original := identity[field]
				identity[field] = "other"
				if !strings.Contains(string(send(followup)), "OLD_RESULT") {
					t.Fatalf("used checkpoint from a different %s", field)
				}
				identity[field] = original
			}
			if !strings.Contains(string(send(strings.Replace(followup, "KEEP_USER", "EDITED_USER", 1))), "OLD_RESULT") {
				t.Fatal("edited prefix was trimmed after restart")
			}
			compact := send(followup)
			input := ojson.GetBytes(compact, "input").Raw
			for _, want := range []string{"KEEP_INSTRUCTION", "KEEP_USER", "Client tool catalog", "lookup", "cross-boundary", "late result", "OPAQUE", "next instruction"} {
				if !strings.Contains(input, want) {
					t.Errorf("compacted request lost %q", want)
				}
			}
			for _, removed := range []string{"OLD_RESULT", "old-call", "OLD_REASONING", "OLD_CHECKPOINT"} {
				if strings.Contains(input, removed) {
					t.Errorf("compacted request retained %q", removed)
				}
			}
			if ojson.GetBytes(compact, "metadata.agent_iteration").String() != "3" {
				t.Error("lost original tool iteration")
			}
			if len(compact) >= len(full)-10000 {
				t.Errorf("expected smaller WS create: %d -> %d bytes", len(full), len(compact))
			}
			t.Logf("post-restart follow-up WS response.create: %d -> %d bytes", len(full), len(compact))

			e.cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gpt-6-astra", Protocol: "codex"}}, Params: map[string]any{"input": []string{"policy"}}}}
			if got := ojson.GetBytes(send(followup), "input.0").String(); got != "policy" {
				t.Errorf("final input override = %q", got)
			}
			e.cfg.Payload.Override = nil
			e.cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "gpt-6-astra", Protocol: "codex"}}, Params: []string{"input"}}}
			if ojson.GetBytes(send(followup), "input").Exists() {
				t.Error("history compaction restored input removed by the final payload filter")
			}
		})
	}
}

func TestBPSHistoryRejectsUncoveredCheckpoint(t *testing.T) {
	const prefix = `{"role":"assistant","content":"retain history"}`
	const marker = `{"type":"compaction","id":"inline","encrypted_content":"opaque"}`
	for _, tc := range []struct {
		name     string
		terminal string
		override bool
		filter   bool
	}{
		{"incomplete response", "incomplete", false, false},
		{"payload rule replaced initial input", "completed", true, false},
		{"payload rule removed initial input", "completed", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{
				`{"type":"response.output_item.done","output_index":0,"item":` + marker + `}`,
				`{"type":"response.` + tc.terminal + `","response":{"id":"resp","status":"` + tc.terminal + `","output":[` + marker + `]}}`,
			}
			wire := make(chan []byte, 1)
			e, _ := newBPSFixture(t, events, func(raw []byte) { wire <- raw })
			auth := bpsFixtureAuth()
			auth.ID = t.TempDir()
			req, opts := bpsFixtureRequest(sdktranslator.FormatOpenAIResponse)
			for i, input := range []string{prefix, prefix + "," + marker} {
				e.cfg.Payload.Override = nil
				e.cfg.Payload.Filter = nil
				if tc.override && i == 0 {
					e.cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: req.Model, Protocol: "codex"}}, Params: map[string]any{"input": []string{"policy"}}}}
				}
				if tc.filter && i == 0 {
					e.cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: req.Model, Protocol: "codex"}}, Params: []string{"input"}}}
				}
				doc := ojson.NewDoc(req.Payload)
				if err := doc.SetRaw("input", "["+input+"]"); err != nil {
					t.Fatal(err)
				}
				req.Payload = doc.Bytes()
				opts.OriginalRequest = req.Payload
				if _, err := e.Execute(t.Context(), auth, req, opts); err != nil {
					t.Fatal(err)
				}
				raw := <-wire
				if i == 1 && !strings.Contains(ojson.GetBytes(raw, "input").Raw, "retain history") {
					t.Fatal("dropped input that was never covered by a completed checkpoint")
				}
			}
		})
	}
}
