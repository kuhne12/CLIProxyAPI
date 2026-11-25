package helps

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestBPSObservationSeparatesTransportAndToolDelivery(t *testing.T) {
	now := time.Unix(100, 0)
	o := NewBPSObservation(1200)
	o.started, o.now = now, func() time.Time { return now }
	advance := func(ms int) { now = now.Add(time.Duration(ms) * time.Millisecond) }
	queueDone := o.Stage("queue")
	advance(7)
	queueDone()
	queueDone()
	prepareDone := o.Stage("prepare")
	advance(3)
	prepareDone()
	o.SetWireBytes(2400)
	observer := o.Observer()
	openDone := o.Stage("upstream_open")
	observer.DialStart()
	advance(20)
	observer.DialDone(0)
	advance(250)
	observer.DialStart()
	advance(30)
	observer.DialDone(101)
	observer.WriteStart()
	advance(12)
	observer.WriteDone()
	openDone()
	advance(10)
	observer.Event([]byte(`{"type":"basispoints.response.metadata","secret":"private-token"}`))
	observer.Event([]byte(`{"type":"basispoints.response.upstream_sent"}`))
	advance(40)
	observer.Event([]byte(`{"type":"response.function_call_arguments.done","arguments":"private-code"}`))
	advance(20)
	observer.Event([]byte(`{"type":"response.output_item.done","item":{"type":"function_call"}}`))
	advance(30)
	observer.Event([]byte(`{"type":"response.completed"}`))
	advance(5)
	o.Handoff([]byte(`{"type":"response.output_item.done","item":{"type":"function_call","arguments":"private-code"}}`))
	advance(10)
	o.Handoff([]byte(`{"type":"response.output_item.done","item":{"type":"function_call"}}`))
	fields := o.fields()
	for key, want := range map[string]float64{
		"queue_ms": 7, "prepare_ms": 3, "ws_dial_ms": 50, "ws_write_ms": 12,
		"upstream_open_ms": 312, "first_wire_event_ms": 332,
		"upstream_tool_arguments_done_ms": 372, "upstream_terminal_ms": 422,
		"tool_handoff_ms": 427, "tool_delivery_wait_ms": 55,
	} {
		if got := fields[key]; got != want {
			t.Errorf("%s=%v, want %v", key, got, want)
		}
	}
	if fields["ws_handshake_attempts"] != 2 || fields["ws_handshake_status"] != 101 {
		t.Fatal("handshake attempts were not observed")
	}
	if _, ok := fields["first_upstream_text_ms"]; ok {
		t.Fatal("tool-only response invented a text observation")
	}
	headers := o.Headers("text/event-stream")
	if headers.Get("X-BPS-Ws-Dial-Ms") != "50.000" || headers.Get("X-BPS-Queue-Ms") != "7.000" || headers.Get("X-BPS-Upstream-Bytes") != "2400" {
		t.Fatalf("incorrect timing headers: %v", headers)
	}
	raw, errMarshal := json.Marshal(fields)
	if errMarshal != nil || strings.Contains(string(raw), "private-") {
		t.Fatal("observation retained private content")
	}
}

func TestBPSObservationFailureOmitsUnobservedStages(t *testing.T) {
	hook := logtest.NewGlobal()
	previous := log.GetLevel()
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() { log.SetLevel(previous) })
	o := NewBPSObservation(17)
	observer := o.Observer()
	observer.DialStart()
	observer.DialDone(429)
	o.Finish(context.Background(), errors.New("private-token"))
	o.Finish(context.Background(), nil)
	entries := hook.AllEntries()
	if len(entries) != 1 || entries[0].Data["outcome"] != "failed" || entries[0].Data["ws_handshake_status"] != 429 {
		t.Fatalf("missing or duplicate failure observation: %v", entries)
	}
	for _, key := range []string{"ws_write_ms", "upstream_terminal_ms", "tool_handoff_ms", "first_wire_event_ms"} {
		if _, ok := entries[0].Data[key]; ok {
			t.Errorf("invented observation %s", key)
		}
	}
	raw, _ := json.Marshal(entries[0].Data)
	if strings.Contains(string(raw), "private-token") {
		t.Fatal("raw error leaked into timing log")
	}
	formatted, errFormat := (&logging.LogFormatter{}).Format(entries[0])
	if errFormat != nil {
		t.Fatal(errFormat)
	}
	for key, value := range entries[0].Data {
		if !strings.Contains(string(formatted), fmt.Sprintf("%s=%v", key, value)) {
			t.Errorf("timing field %s was dropped by the application log formatter", key)
		}
	}
	if strings.Contains(string(formatted), "private-token") {
		t.Fatal("raw error leaked into formatted timing log")
	}
}

func TestBPSObservationConcurrentStreamAndHeaders(t *testing.T) {
	o := NewBPSObservation(1)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			for j := 0; j < 100; j++ {
				o.observeUpstream([]byte(`{"type":"response.output_text.delta","delta":"hello"}`))
				o.Handoff([]byte(`{"type":"response.output_text.delta","delta":"hello"}`))
				_ = o.Headers("text/event-stream")
			}
		})
	}
	wg.Wait()
	fields := o.fields()
	if fields["first_upstream_text_ms"] == nil || fields["first_text_handoff_ms"] == nil {
		t.Fatal("missing concurrent observation")
	}
}
