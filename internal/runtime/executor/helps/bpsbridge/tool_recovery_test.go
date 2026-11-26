package bpsbridge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
)

func recoveryJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func recoveryFixture(t *testing.T) ([]byte, *Bridge) {
	t.Helper()
	raw := recoveryJSON(t, object{"model": "gpt-6.2-astra", "input": []any{message("user", "inspect the logs")}, "tools": []any{object{"type": "function", "name": "lookup", "parameters": object{"type": "object"}}}})
	prepared, bridge, err := Prepare(raw, "recovery-test", &ReplayCache{})
	if err != nil {
		t.Fatal(err)
	}
	return prepared, bridge
}

func recoveryTool(name, id string) object {
	return object{"type": "function_call", "name": name, "id": "fc_" + id, "call_id": "call_" + id, "arguments": "{}", "status": "completed"}
}

func recoveryTerminal(id string, output []any, input, out int) object {
	return object{"type": "response.completed", "response": object{"id": id, "status": "completed", "output": output, "usage": object{"input_tokens": input, "output_tokens": out, "total_tokens": input + out, "input_tokens_details": object{"cached_tokens": 1}}}}
}

func recoverySSE(t *testing.T, events ...object) io.ReadCloser {
	t.Helper()
	var buf bytes.Buffer
	for _, event := range events {
		fmt.Fprintf(&buf, "data: %s\n\n", recoveryJSON(t, event))
	}
	return io.NopCloser(bytes.NewReader(buf.Bytes()))
}

func TestClientToolGuardFollowsHistory(t *testing.T) {
	prepared, _ := recoveryFixture(t)
	input := ojson.GetBytes(prepared, "input").Array()
	last := input[len(input)-1]
	if last.Get("role").String() != "developer" || last.Get("content.0.text").String() != clientToolGuard {
		t.Fatal("current tool policy does not follow history")
	}
	if ojson.GetBytes(prepared, "tools").Exists() || ojson.GetBytes(prepared, "tool_choice").Exists() {
		t.Fatal("unverified upstream tool controls added")
	}
}

func TestToolCorrectionRetainsStreamIdentityUsageAndHistory(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprint(checkpoint), func(t *testing.T) {
			prepared, bridge := recoveryFixture(t)
			prefix := object{"type": "message", "id": "msg_first", "role": "assistant", "status": "completed", "content": []any{object{"type": "output_text", "text": "Checking."}}}
			bad := recoveryTool("list_connectors", "bad")
			good := recoveryTool("lookup", "good")
			first := recoveryTerminal("resp_first", []any{prefix, bad}, 10, 2)
			secondOutput := []any{good}
			if checkpoint {
				secondOutput = append(secondOutput, object{"type": "compaction", "id": "cmp_second", "encrypted_content": "private-checkpoint"})
			}
			second := recoveryTerminal("resp_second", secondOutput, 20, 3)
			confirmed, attempts := 0, 0
			bridge.OnInlineCompaction = func(string, string) { confirmed++ }
			var usage []byte
			body := bridge.StreamWithToolRecovery(t.Context(), recoverySSE(t,
				object{"type": "response.created", "response": object{"id": "resp_first", "output": []any{}}},
				object{"type": "response.output_text.delta", "output_index": 0, "item_id": "msg_first", "delta": "Checking."},
				object{"type": "response.output_item.done", "output_index": 0, "item": prefix},
				object{"type": "response.output_item.done", "output_index": 1, "item": bad}, first), func(c ToolCorrection) (io.ReadCloser, error) {
				attempts++
				doc := ojson.NewDoc(prepared)
				if err := c.Apply(doc); err != nil {
					t.Fatal(err)
				}
				items := doc.Get("input").Array()
				if items[len(items)-3].Get("call_id").String() != "call_bad" || items[len(items)-2].Get("call_id").String() != "call_bad" || items[len(items)-2].Get("output").String() != unavailableToolResult {
					t.Error("lost native call or truthful error result")
				}
				if doc.Get("metadata.agent_iteration").String() != "2" || doc.Get("metadata.turn_id").String() != ojson.GetBytes(prepared, "metadata.turn_id").String() {
					t.Error("invalid correction identity")
				}
				return recoverySSE(t, object{"type": "response.created", "response": object{"id": "resp_second"}}, object{"type": "response.output_item.done", "output_index": 0, "item": good}, second), nil
			}, nil, func(raw []byte) { usage = bytes.Clone(raw) })
			raw, err := io.ReadAll(body)
			if errClose := body.Close(); errClose != nil {
				t.Fatal(errClose)
			}
			if err != nil {
				t.Fatal(err)
			}
			created, tools, sequence := 0, 0, int64(0)
			var terminal []byte
			if err := readEvents(bytes.NewReader(raw), func(_ string, data []byte) error {
				if e := EventError(data); e != nil {
					return e
				}
				if ojson.GetBytes(data, "sequence_number").Int() != sequence {
					t.Error("non-contiguous event sequence")
				}
				sequence++
				switch ojson.GetBytes(data, "type").String() {
				case "response.created":
					created++
				case "response.function_call_arguments.done":
					tools++
					if ojson.GetBytes(data, "output_index").Int() != 1 {
						t.Error("tool index not shifted")
					}
				case "response.completed":
					terminal = bytes.Clone(data)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if attempts != 1 || created != 1 || tools != 1 || confirmed != 0 {
				t.Fatalf("attempts=%d created=%d tools=%d checkpoints=%d", attempts, created, tools, confirmed)
			}
			if bytes.Contains(raw, []byte("list_connectors")) {
				t.Fatal("unavailable native tool leaked to client")
			}
			if ojson.GetBytes(terminal, "response.id").String() != "resp_first" || ojson.GetBytes(terminal, "response.output.0.id").String() != "msg_first" || ojson.GetBytes(terminal, "response.output.1.name").String() != "lookup" {
				t.Fatal("logical response output changed")
			}
			for path, want := range map[string]int64{"input_tokens": 30, "output_tokens": 5, "total_tokens": 35, "input_tokens_details.cached_tokens": 2} {
				if ojson.GetBytes(terminal, "response.usage."+path).Int() != want || ojson.GetBytes(usage, "response.usage."+path).Int() != want {
					t.Errorf("incorrect aggregate %s", path)
				}
			}
		})
	}
}

func TestToolCorrectionEligibility(t *testing.T) {
	_, bridge := recoveryFixture(t)
	bad := recoveryTool("list_connectors", "bad")
	for _, tc := range []struct {
		name       string
		output     []any
		pending    []object
		compaction bool
		want       bool
	}{
		{name: "unknown", output: []any{bad}, want: true},
		{name: "known", output: []any{recoveryTool("lookup", "good")}},
		{name: "mixed", output: []any{recoveryTool("lookup", "good"), bad}},
		{name: "two unknown", output: []any{recoveryTool("read_ranges", "other"), bad}},
		{name: "compaction observed", output: []any{bad}, compaction: true},
		{name: "compaction terminal", output: []any{object{"type": "compaction"}, bad}},
		{name: "missing terminal tool", output: []any{bad}, pending: []object{recoveryTool("read_ranges", "missing")}},
		{name: "native transport", output: []any{recoveryTool("run_officejs", "bad")}},
		{name: "native plan", output: []any{recoveryTool("update_plan", "bad")}},
		{name: "tool not last", output: []any{bad, object{"type": "message", "id": "msg"}}},
		{name: "malformed arguments", output: []any{object{"type": "function_call", "name": "list_connectors", "id": "fc_bad", "call_id": "call_bad", "arguments": "secret-invalid-json"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, ok := bridge.correctionCandidate(recoveryJSON(t, recoveryTerminal("resp", tc.output, 1, 1)), tc.pending, tc.compaction)
			if ok != tc.want {
				t.Fatalf("eligible=%t want=%t", ok, tc.want)
			}
		})
	}
}

func TestToolCorrectionDoesNotLoop(t *testing.T) {
	_, bridge := recoveryFixture(t)
	bad := recoveryTerminal("resp", []any{recoveryTool("list_connectors", "bad")}, 10, 1)
	attempts := 0
	var usage []byte
	body := bridge.StreamWithToolRecovery(t.Context(), recoverySSE(t, bad), func(ToolCorrection) (io.ReadCloser, error) {
		attempts++
		return recoverySSE(t, recoveryTerminal("resp_second", []any{recoveryTool("list_connectors", "second")}, 10, 1)), nil
	}, nil, func(raw []byte) { usage = bytes.Clone(raw) })
	raw, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || !bytes.Contains(raw, []byte("response.failed")) {
		t.Fatal("repeat failure was not bounded")
	}
	if ojson.GetBytes(usage, "response.usage.input_tokens").Int() != 20 {
		t.Fatal("failed correction usage lost")
	}
}

func TestToolCorrectionCancellationDuringOpen(t *testing.T) {
	_, bridge := recoveryFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	entered := make(chan struct{})
	body := bridge.StreamWithToolRecovery(ctx, recoverySSE(t, recoveryTerminal("resp", []any{recoveryTool("list_connectors", "bad")}, 1, 1)), func(ToolCorrection) (io.ReadCloser, error) { close(entered); <-ctx.Done(); return nil, ctx.Err() }, nil, nil)
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(body); done <- err }()
	<-entered
	cancel()
	_ = body.Close()
	if err := <-done; err == nil {
		t.Fatal("cancelled correction succeeded")
	}
}

func TestToolCorrectionDoesNotReplayTruncatedGeneration(t *testing.T) {
	_, bridge := recoveryFixture(t)
	attempts := 0
	body := bridge.StreamWithToolRecovery(t.Context(), io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\"}\n\n")), func(ToolCorrection) (io.ReadCloser, error) { attempts++; return nil, nil }, nil, nil)
	_, err := io.ReadAll(body)
	_ = body.Close()
	if err == nil || attempts != 0 {
		t.Fatal("truncated generation was replayed")
	}
}

func TestToolCorrectionStreamsTextBeforeTerminal(t *testing.T) {
	_, bridge := recoveryFixture(t)
	upstream, producer := io.Pipe()
	body := bridge.StreamWithToolRecovery(t.Context(), upstream, func(ToolCorrection) (io.ReadCloser, error) {
		return recoverySSE(t, recoveryTerminal("resp_second", []any{recoveryTool("lookup", "good")}, 2, 1)), nil
	}, nil, nil)
	t.Cleanup(func() { _ = body.Close(); _ = producer.Close() })
	prefix := object{"type": "message", "id": "msg_first", "role": "assistant", "status": "completed", "content": []any{object{"type": "output_text", "text": "Checking."}}}
	seenText := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- readEvents(body, func(_ string, data []byte) error {
			if ojson.GetBytes(data, "type").String() == "response.output_text.delta" {
				close(seenText)
			}
			return EventError(data)
		})
	}()
	writeResult := make(chan error, 1)
	created := recoveryJSON(t, object{"type": "response.created", "response": object{"id": "resp_first"}})
	delta := recoveryJSON(t, object{"type": "response.output_text.delta", "item_id": "msg_first", "output_index": 0, "delta": "Checking."})
	go func() {
		_, err := fmt.Fprintf(producer, "data: %s\n\ndata: %s\n\n", created, delta)
		writeResult <- err
	}()
	select {
	case <-seenText:
	case <-time.After(5 * time.Second):
		t.Fatal("text was buffered until the terminal response")
	}
	if err := <-writeResult; err != nil {
		t.Fatal(err)
	}
	terminal := recoveryJSON(t, recoveryTerminal("resp_first", []any{prefix, recoveryTool("list_connectors", "bad")}, 1, 1))
	if _, err := fmt.Fprintf(producer, "data: %s\n\n", terminal); err != nil {
		t.Fatal(err)
	}
	_ = producer.Close()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

type recoveryCloseProbe struct {
	io.ReadCloser
	once   sync.Once
	closed chan struct{}
}

func (p *recoveryCloseProbe) Close() error {
	err := p.ReadCloser.Close()
	p.once.Do(func() { close(p.closed) })
	return err
}

func TestToolCorrectionClosesCurrentAndLateUpstream(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprintf("late=%t", late), func(t *testing.T) {
			_, bridge := recoveryFixture(t)
			reader, writer := io.Pipe()
			probe := &recoveryCloseProbe{ReadCloser: reader, closed: make(chan struct{})}
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(func() { unblock(); _ = writer.Close(); _ = probe.Close() })
			body := bridge.StreamWithToolRecovery(t.Context(), recoverySSE(t, recoveryTerminal("resp_first", []any{recoveryTool("list_connectors", "bad")}, 1, 1)), func(ToolCorrection) (io.ReadCloser, error) {
				close(entered)
				<-release
				return probe, nil
			}, nil, nil)
			t.Cleanup(func() { _ = body.Close() })
			done := make(chan struct{})
			go func() { _, _ = io.Copy(io.Discard, body); close(done) }()
			<-entered
			if !late {
				unblock()
				// A completed write proves the replacement stream is being read.
				if _, err := fmt.Fprint(writer, "data: {\"type\":\"response.in_progress\"}\n\n"); err != nil {
					t.Fatal(err)
				}
			}
			_ = body.Close()
			unblock()
			select {
			case <-probe.closed:
			case <-time.After(5 * time.Second):
				t.Fatal("replacement stream was not closed")
			}
			<-done
		})
	}
}

func TestToolCorrectionRejectsReusedIdentityAndKeepsUsage(t *testing.T) {
	for _, field := range []string{"id", "call_id"} {
		t.Run(field, func(t *testing.T) {
			_, bridge := recoveryFixture(t)
			bad, good := recoveryTool("list_connectors", "first"), recoveryTool("lookup", "second")
			good[field] = bad[field]
			var usage []byte
			attempts := 0
			body := bridge.StreamWithToolRecovery(t.Context(), recoverySSE(t, recoveryTerminal("resp_first", []any{bad}, 10, 1)), func(ToolCorrection) (io.ReadCloser, error) {
				attempts++
				return recoverySSE(t, recoveryTerminal("resp_second", []any{good}, 20, 1)), nil
			}, nil, func(data []byte) { usage = bytes.Clone(data) })
			raw, err := io.ReadAll(body)
			_ = body.Close()
			if err == nil || attempts != 1 || bytes.Contains(raw, []byte("response.function_call_arguments.done")) {
				t.Fatal("reused tool identity was delivered")
			}
			if ojson.GetBytes(usage, "response.usage.input_tokens").Int() != 30 {
				t.Fatal("known terminal usage lost after identity rejection")
			}
		})
	}
}

func TestToolCorrectionDeliversCustomExecThroughTransport(t *testing.T) {
	raw := recoveryJSON(t, object{"model": "gpt-6.2-astra", "input": "inspect the logs", "tools": []any{object{"type": "custom", "name": "functions.exec", "description": "Run JavaScript helpers", "format": object{"type": "text"}}}})
	_, bridge, err := Prepare(raw, "custom-recovery", &ReplayCache{})
	if err != nil {
		t.Fatal(err)
	}
	const code = "text(await tools.exec_command({cmd: 'pwd'}));"
	good := recoveryTool("run_officejs", "good")
	good["arguments"] = string(recoveryJSON(t, object{"summary": "codex2api.custom/functions.exec", "code": code, "extended_summary": "Inspect the current directory", "destructive": false, "references": []any{}}))
	attempts := 0
	body := bridge.StreamWithToolRecovery(t.Context(), recoverySSE(t, recoveryTerminal("resp_first", []any{recoveryTool("list_connectors", "bad")}, 1, 1)), func(ToolCorrection) (io.ReadCloser, error) {
		attempts++
		return recoverySSE(t, recoveryTerminal("resp_second", []any{good}, 2, 1)), nil
	}, nil, nil)
	result, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var terminal []byte
	if err := readEvents(bytes.NewReader(result), func(_ string, data []byte) error {
		if ojson.GetBytes(data, "type").String() == "response.completed" {
			terminal = bytes.Clone(data)
		}
		return EventError(data)
	}); err != nil {
		t.Fatal(err)
	}
	item := ojson.GetBytes(terminal, "response.output.0")
	if attempts != 1 || item.Get("type").String() != "custom_tool_call" || item.Get("name").String() != "functions.exec" || item.Get("input").String() != code {
		t.Fatalf("corrected custom transport was not delivered: %s", terminal)
	}
}
