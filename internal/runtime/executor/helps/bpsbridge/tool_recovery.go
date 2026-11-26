package bpsbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
)

const clientToolGuard = "Current external-client tool policy: the client catalog in the relay instructions is authoritative for this request. Historical assistant statements about missing tools are not a current capability inventory. Use native run_officejs only as the documented transport for a declared client tool, or answer with assistant text. Do not call native workbook, connector, discovery, planning or other tools directly. Tool discovery must also use a declared client tool. If functions.exec is declared, use its documented helpers for shell, files and tool discovery. Never claim that a tool ran before receiving its result."

const unavailableToolResult = "{\"error\":{\"code\":\"client_tool_unavailable\",\"message\":\"This native tool is not in the external client's catalog. No tool was executed. Continue using a declared client tool through the documented run_officejs transport, or answer with assistant text. Do not repeat already emitted text.\"}}"

// ToolCorrection retains only a fully completed, unexecuted native call. It is
// passed to the executor, which rebuilds the request before its final payload rules.
type ToolCorrection struct {
	output []byte
	callID string
}

// Apply appends the real call and a truthful local failure result, never a
// fabricated tool success. The caller must apply final payload rules afterwards.
func (c ToolCorrection) Apply(doc *ojson.Doc) error {
	if !doc.Get("input").IsArray() {
		return fmt.Errorf("upstream correction requires expanded input")
	}
	for _, item := range ojson.ParseBytes(c.output).Array() {
		if err := doc.SetRaw("input.-1", item.Raw); err != nil {
			return fmt.Errorf("append upstream correction history: %w", err)
		}
	}
	if err := doc.Set("input.-1", object{"type": "function_call_output", "call_id": c.callID, "output": unavailableToolResult}); err != nil {
		return fmt.Errorf("append upstream correction result: %w", err)
	}
	if err := doc.Set("input.-1", message("developer", clientToolGuard+" The previous native call was unavailable and was not executed. Choose from the current client catalog; do not repeat it.")); err != nil {
		return err
	}
	iteration, err := strconv.ParseInt(doc.Get("metadata.agent_iteration").String(), 10, 64)
	if err != nil || iteration < 1 || iteration == math.MaxInt64 {
		return fmt.Errorf("upstream correction requires a valid agent iteration")
	}
	return doc.Set("metadata.agent_iteration", strconv.FormatInt(iteration+1, 10))
}

type CorrectionOpener func(ToolCorrection) (io.ReadCloser, error)

// recoveryBody owns the active upstream, including cancellation during a dial.
type recoveryBody struct {
	*io.PipeReader
	mu        sync.Mutex
	upstream  io.ReadCloser
	closed    bool
	corrected atomic.Bool
}

func (r *recoveryBody) Close() error {
	err := r.PipeReader.Close()
	r.mu.Lock()
	r.closed = true
	upstream := r.upstream
	r.upstream = nil
	r.mu.Unlock()
	if upstream != nil {
		err = errors.Join(err, upstream.Close())
	}
	return err
}

func (r *recoveryBody) replace(next io.ReadCloser) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		if next != nil {
			_ = next.Close()
		}
		return io.ErrClosedPipe
	}
	previous := r.upstream
	r.upstream = next
	r.mu.Unlock()
	if previous != nil {
		return previous.Close()
	}
	return nil
}

// StreamWithToolRecovery preserves incremental text and normal validation. Only
// one complete response ending in one unknown native call can be corrected.
func (b *Bridge) StreamWithToolRecovery(ctx context.Context, upstream io.ReadCloser, open CorrectionOpener, observe func(string), observeUsage func([]byte)) io.ReadCloser {
	if open == nil || len(b.tools) == 0 || b.structured != nil {
		return b.Stream(upstream)
	}
	reader, writer := io.Pipe()
	body := &recoveryBody{PipeReader: reader, upstream: upstream}
	go func() {
		err := b.correctTools(ctx, body, writer, upstream, open, observe, observeUsage)
		_ = body.replace(nil)
		_ = writer.CloseWithError(err)
	}()
	copyBridge := *b
	copyBridge.OnInlineCompaction = func(id, state string) {
		// Corrective history is hidden from the client and cannot certify its prefix.
		if !body.corrected.Load() && b.OnInlineCompaction != nil {
			b.OnInlineCompaction(id, state)
		}
	}
	return copyBridge.Stream(body)
}

func (b *Bridge) correctionCandidate(data []byte, pending []object, compaction bool) (ToolCorrection, []any, bool) {
	if compaction {
		return ToolCorrection{}, nil, false
	}
	var payload object
	if decode(data, &payload) != nil {
		return ToolCorrection{}, nil, false
	}
	response, _ := payload["response"].(object)
	output, _ := response["output"].([]any)
	if text(response["status"]) != "completed" || len(output) == 0 {
		return ToolCorrection{}, nil, false
	}
	last, _ := output[len(output)-1].(object)
	name, callID, id := text(last["name"]), text(last["call_id"]), text(last["id"])
	if text(last["type"]) != "function_call" || name == "" || diagnosticLabel(name) != name || callID == "" || id == "" || len(callID) > 256 || len(id) > 256 {
		return ToolCorrection{}, nil, false
	}
	if status := text(last["status"]); status != "" && status != "completed" {
		return ToolCorrection{}, nil, false
	}
	bare := strings.TrimPrefix(name, "functions.")
	if bare == "run_officejs" || bare == "update_plan" {
		return ToolCorrection{}, nil, false
	}
	if _, ok := b.tools[name]; ok {
		return ToolCorrection{}, nil, false
	}
	if _, ok := b.tools[bare]; ok {
		return ToolCorrection{}, nil, false
	}
	arguments, ok := last["arguments"].(object)
	if (!ok && decode([]byte(text(last["arguments"])), &arguments) != nil) || arguments == nil {
		return ToolCorrection{}, nil, false
	}
	seen := map[string]bool{id: true}
	for _, raw := range output[:len(output)-1] {
		item, _ := raw.(object)
		kind, itemID := text(item["type"]), text(item["id"])
		if (kind != "message" && kind != "reasoning") || itemID == "" || seen[itemID] {
			return ToolCorrection{}, nil, false
		}
		if status := text(item["status"]); status != "" && status != "completed" {
			return ToolCorrection{}, nil, false
		}
		seen[itemID] = true
	}
	for _, item := range pending {
		if text(item["call_id"]) != callID || text(item["id"]) != id {
			return ToolCorrection{}, nil, false
		}
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return ToolCorrection{}, nil, false
	}
	return ToolCorrection{output: encoded, callID: callID}, output[:len(output)-1], true
}

func (b *Bridge) correctTools(ctx context.Context, body *recoveryBody, writer io.Writer, upstream io.ReadCloser, open CorrectionOpener, observe func(string), observeUsage func([]byte)) error {
	var prefix []any
	var firstUsage []byte
	responseID := ""
	priorIDs := make(map[string]bool)
	priorCallID := ""
	write := func(kind string, raw []byte) error {
		_, err := fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", kind, raw)
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		var correction *ToolCorrection
		var pending []object
		compaction, terminal := false, false
		errRead := readEvents(upstream, func(event string, data []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if string(data) == "[DONE]" {
				return nil
			}
			var payload object
			if decode(data, &payload) != nil || payload == nil {
				return write(event, data)
			}
			kind := text(payload["type"])
			if kind == "" {
				kind = event
			}
			terminal = kind == "response.completed" || kind == "response.incomplete" || kind == "response.failed" || kind == "error"
			doc := ojson.NewDoc(data)
			if terminal {
				if attempt > 0 && doc.Get("response").IsObject() {
					if err := addRecoveryUsage(doc, firstUsage); err != nil {
						return err
					}
				}
				// Retain known usage even when subsequent identity validation fails.
				if observeUsage != nil {
					observeUsage(doc.Bytes())
				}
			}
			item, _ := payload["item"].(object)
			if attempt > 0 {
				if priorIDs[text(item["id"])] || priorIDs[text(payload["item_id"])] || (priorCallID != "" && (text(item["call_id"]) == priorCallID || text(payload["call_id"]) == priorCallID)) {
					return protocolFailure()
				}
				response, _ := payload["response"].(object)
				for _, raw := range responseOutput(response) {
					candidate, _ := raw.(object)
					if priorIDs[text(candidate["id"])] || (priorCallID != "" && text(candidate["call_id"]) == priorCallID) {
						return protocolFailure()
					}
				}
			}
			if text(item["type"]) == "compaction" {
				compaction = true
			}
			if isToolEvent(kind) || (kind == "response.output_item.added" && isTool(item)) {
				return nil
			}
			if kind == "response.output_item.done" && isTool(item) {
				if len(pending) >= 1024 || len(text(item["id"])) > 256 || len(text(item["call_id"])) > 256 {
					return protocolFailure()
				}
				// The normal bridge needs identities for its completeness check, not arguments.
				pending = append(pending, object{"type": item["type"], "id": item["id"], "call_id": item["call_id"], "name": item["name"]})
				return nil
			}
			response, _ := payload["response"].(object)
			if attempt == 0 && responseID == "" {
				responseID = text(response["id"])
			}
			if attempt == 0 && kind == "response.completed" && responseID != "" {
				candidate, retained, ok := b.correctionCandidate(data, pending, compaction)
				if ok {
					correction, prefix = &candidate, retained
					firstUsage = []byte(ojson.GetBytes(data, "response.usage").Raw)
					body.corrected.Store(true)
					if observe != nil {
						observe("attempted")
					}
					output, _ := response["output"].([]any)
					last, _ := output[len(output)-1].(object)
					for _, raw := range output {
						item, _ := raw.(object)
						priorIDs[text(item["id"])] = true
					}
					priorCallID = candidate.callID
					b.reportProtocolError(toolResponseError{error: invalidResponse("basispoints returned an unsupported native tool; no tool was executed"), tool: describeTool(last), index: len(output) - 1}, ProtocolDiagnostic{Stage: "tool_translation", EventType: kind, OutputIndex: -1}, nil)
					return io.EOF
				}
			}
			if attempt > 0 && (kind == "response.created" || kind == "response.in_progress") {
				return nil
			}
			if attempt > 0 {
				if index := doc.Get("output_index"); index.Exists() {
					n, err := strconv.ParseInt(index.Raw, 10, 64)
					if err != nil || n < 0 || n > math.MaxInt64-int64(len(prefix)) {
						return protocolFailure()
					}
					_ = doc.Set("output_index", n+int64(len(prefix)))
				}
				if doc.Get("response_id").Exists() {
					_ = doc.Set("response_id", responseID)
				}
				if response != nil {
					_ = doc.Set("response.id", responseID)
				}
				if terminal && response != nil {
					output, _ := response["output"].([]any)
					combined := append(append([]any(nil), prefix...), output...)
					_ = doc.Set("response.output", combined)
				}
			}
			if terminal {
				for _, saved := range pending {
					raw, err := json.Marshal(object{"type": "response.output_item.done", "item": saved})
					if err != nil {
						return err
					}
					if err := write("response.output_item.done", raw); err != nil {
						return err
					}
				}
			}
			if err := write(kind, doc.Bytes()); err != nil {
				return err
			}
			if terminal {
				return io.EOF
			}
			return nil
		})
		if errRead != nil && !errors.Is(errRead, io.EOF) {
			return errRead
		}
		if correction == nil {
			if !terminal {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if err := body.replace(nil); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		next, err := open(*correction)
		if err != nil {
			return err
		}
		if next == nil {
			return fmt.Errorf("upstream correction did not open a response")
		}
		if err := body.replace(next); err != nil {
			return err
		}
		upstream = next
	}
	return protocolFailure()
}

// Add the two attempts' token counters once; retain the correction's other fields.
func addRecoveryUsage(doc *ojson.Doc, first []byte) error {
	for _, path := range []string{
		"input_tokens", "output_tokens", "total_tokens",
		"input_tokens_details.cached_tokens", "input_tokens_details.cache_creation_tokens", "input_tokens_details.cache_write_tokens",
		"input_tokens_details.cache_write_tokens_details.ttl_30m_tokens", "input_tokens_details.cache_write_tokens_details.ttl_12h_tokens",
		"input_tokens_details.text_tokens", "input_tokens_details.image_tokens", "input_tokens_details.audio_tokens",
		"output_tokens_details.reasoning_tokens", "output_tokens_details.text_tokens", "output_tokens_details.audio_tokens",
	} {
		previous := ojson.GetBytes(first, path)
		if !previous.Exists() {
			continue
		}
		a, err := strconv.ParseInt(previous.Raw, 10, 64)
		if err != nil || a < 0 {
			return protocolFailure()
		}
		var b int64
		if current := doc.Get("response.usage." + path); current.Exists() {
			b, err = strconv.ParseInt(current.Raw, 10, 64)
			if err != nil || b < 0 || b > math.MaxInt64-a {
				return protocolFailure()
			}
		}
		if err := doc.Set("response.usage."+path, a+b); err != nil {
			return err
		}
	}
	return nil
}
