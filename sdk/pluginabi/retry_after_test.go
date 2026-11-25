package pluginabi

import (
	"testing"
	"time"
)

func TestNativeErrorRetryAfter(t *testing.T) {
	for _, e := range []*Error{nil, {}, {RetryAfterSeconds: -1}} {
		if e.RetryAfter() != nil {
			t.Fatal("absent or invalid hint must remain absent")
		}
		if len(e.SafeResponseHeaders()) != 0 {
			t.Fatal("absent retry hint must not produce a response header")
		}
	}
	e := &Error{RetryAfterSeconds: 120}
	if got := e.RetryAfter(); got == nil || *got != 120*time.Second {
		t.Fatalf("hint=%v", got)
	}
	if headers := e.SafeResponseHeaders(); len(headers) != 1 || headers.Get("Retry-After") != "120" {
		t.Fatalf("incorrect retry response headers: %v", headers)
	}
	maxInt := int(^uint(0) >> 1)
	e.RetryAfterSeconds = maxInt
	seconds := int64(maxInt)
	if limit := int64((1<<63 - 1) / time.Second); seconds > limit {
		seconds = limit
	}
	if got := e.RetryAfter(); got == nil || *got != time.Duration(seconds)*time.Second {
		t.Fatalf("overflowed hint=%v", got)
	}
}
