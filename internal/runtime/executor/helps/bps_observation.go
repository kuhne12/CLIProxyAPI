package helps

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/bpsbridge"
	log "github.com/sirupsen/logrus"
)

// BPSObservation retains bounded diagnostics and local monotonic timings. Milestone
// fields are offsets from executor entry, while stage fields are durations.
// Missing observations are omitted, rather than reported as measured zeros.
type BPSObservation struct {
	mu          sync.Mutex
	now         func() time.Time
	started     time.Time
	stages      map[string]time.Duration
	milestones  map[string]time.Duration
	inputBytes  int
	wireBytes   int
	attempts    int
	status      int
	terminal    string
	finishOnce  sync.Once
	diagnostics log.Fields
}

func NewBPSObservation(inputBytes int) *BPSObservation {
	return &BPSObservation{now: time.Now, started: time.Now(), inputBytes: inputBytes,
		stages: make(map[string]time.Duration), milestones: make(map[string]time.Duration)}
}

// Stage records each completed interval once, accumulating repeated dial attempts.
func (o *BPSObservation) Stage(name string) func() {
	start := o.now()
	var once sync.Once
	return func() {
		once.Do(func() {
			o.mu.Lock()
			o.stages[name] += o.now().Sub(start)
			o.mu.Unlock()
		})
	}
}

func (o *BPSObservation) Mark(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, exists := o.milestones[name]; !exists {
		o.milestones[name] = o.now().Sub(o.started)
	}
}

func (o *BPSObservation) SetWireBytes(size int) {
	o.mu.Lock()
	o.wireBytes = size
	o.mu.Unlock()
}

func (o *BPSObservation) Observer() bpsbridge.Observer {
	// Dial and write callbacks are sequential within a single open operation.
	var dialDone, writeDone func()
	return bpsbridge.Observer{
		DialStart: func() {
			o.mu.Lock()
			o.attempts++
			o.mu.Unlock()
			dialDone = o.Stage("ws_dial")
		},
		DialDone: func(status int) {
			dialDone()
			o.mu.Lock()
			o.status = status
			o.mu.Unlock()
		},
		WriteStart: func() { writeDone = o.Stage("ws_write") },
		WriteDone:  func() { writeDone() },
		Event:      o.observeUpstream,
	}
}

func (o *BPSObservation) observeUpstream(data []byte) {
	o.Mark("first_wire_event")
	kind := ojson.GetBytes(data, "type").String()
	if strings.HasPrefix(kind, "response.") || kind == "error" {
		o.Mark("first_response_event")
	}
	switch kind {
	case "basispoints.response.upstream_sent":
		o.Mark("upstream_sent")
	case "response.output_text.delta":
		if ojson.GetBytes(data, "delta").String() != "" {
			o.Mark("first_upstream_text")
		}
	case "response.function_call_arguments.done", "response.custom_tool_call_input.done":
		o.Mark("upstream_tool_arguments_done")
	case "response.output_item.done":
		o.observeCompaction(ojson.GetBytes(data, "item"))
		if isBPSClientTool(ojson.GetBytes(data, "item.type").String()) {
			o.Mark("upstream_tool_item_done")
		}
	case "response.completed", "response.incomplete", "response.failed", "error":
		for _, item := range ojson.GetBytes(data, "response.output").Array() {
			o.observeCompaction(item)
		}
		o.Mark("upstream_terminal")
		o.mu.Lock()
		o.terminal = kind
		o.mu.Unlock()
	}
}

func isBPSClientTool(kind string) bool {
	return kind == "function_call" || kind == "custom_tool_call"
}

// Handoff observes successful executor-channel delivery, not socket receipt by
// the remote client. It runs after translating and sending the event's frames.
func (o *BPSObservation) Handoff(data []byte) {
	o.Mark("first_handoff")
	kind := ojson.GetBytes(data, "type").String()
	switch kind {
	case "response.output_text.delta":
		if ojson.GetBytes(data, "delta").String() != "" {
			o.Mark("first_text_handoff")
		}
	case "response.output_item.done":
		if isBPSClientTool(ojson.GetBytes(data, "item.type").String()) {
			o.Mark("tool_handoff")
		}
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		// The bridge emits one complete, validated argument/input delta. Some
		// downstream translators consume output_item.done without emitting a frame.
		if ojson.GetBytes(data, "delta").String() != "" {
			o.Mark("tool_handoff")
		}
	case "response.completed", "response.incomplete", "response.failed", "error":
		o.Mark("terminal_handoff")
	}
}

func bpsMilliseconds(duration time.Duration) float64 {
	return float64(duration.Microseconds()) / 1000
}

func (o *BPSObservation) fields() log.Fields {
	o.mu.Lock()
	defer o.mu.Unlock()
	fields := log.Fields{"input_bytes": o.inputBytes, "ws_handshake_attempts": o.attempts}
	for key, value := range o.diagnostics {
		fields[key] = value
	}
	if o.wireBytes != 0 {
		fields["upstream_bytes"] = o.wireBytes
	}
	if o.status != 0 {
		fields["ws_handshake_status"] = o.status
	}
	if o.terminal != "" {
		fields["upstream_terminal_type"] = o.terminal
	}
	for name, duration := range o.stages {
		fields[name+"_ms"] = bpsMilliseconds(duration)
	}
	for name, duration := range o.milestones {
		fields[name+"_ms"] = bpsMilliseconds(duration)
	}
	if done, ok := o.milestones["upstream_tool_arguments_done"]; ok {
		if handoff, okHandoff := o.milestones["tool_handoff"]; okHandoff {
			fields["tool_delivery_wait_ms"] = bpsMilliseconds(handoff - done)
		}
	}
	return fields
}

// Headers contains only stages available when the executor opens the response.
// Later stream milestones are published in the final structured log record.
func (o *BPSObservation) Headers(contentType string) http.Header {
	h := http.Header{"Content-Type": {contentType}}
	fields := o.fields()
	for field, header := range map[string]string{
		"queue_ms": "X-BPS-Queue-Ms", "prepare_ms": "X-BPS-Prepare-Ms",
		"ws_dial_ms": "X-BPS-Ws-Dial-Ms", "ws_write_ms": "X-BPS-Ws-Write-Ms",
		"upstream_open_ms": "X-BPS-Upstream-Open-Ms",
	} {
		if value, ok := fields[field].(float64); ok {
			h.Set(header, strconv.FormatFloat(value, 'f', 3, 64))
		}
	}
	for field, header := range map[string]string{
		"input_bytes": "X-BPS-Input-Bytes", "upstream_bytes": "X-BPS-Upstream-Bytes",
		"ws_handshake_attempts": "X-BPS-Ws-Handshake-Attempts", "ws_handshake_status": "X-BPS-Ws-Handshake-Status",
	} {
		if value, ok := fields[field].(int); ok {
			h.Set(header, strconv.Itoa(value))
		}
	}
	return h
}

func (o *BPSObservation) Finish(ctx context.Context, err error) {
	o.finishOnce.Do(func() {
		o.Mark("executor_done")
		fields := o.fields()
		if fields["compaction_confirmation"] == "pending_validation" {
			fields["compaction_confirmation"] = "not_confirmed"
		}
		fields["provider"] = "bps"
		fields["outcome"] = "completed"
		if err != nil {
			fields["outcome"] = "failed"
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				fields["outcome"] = "cancelled"
			}
		}
		LogWithRequestID(ctx).WithFields(fields).Info("BPS request timing")
	})
}
