package bpsbridge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

type handshakeTransport func(*http.Request) (*http.Response, error)

func (f handshakeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBPSRetryAfterSeconds(t *testing.T) {
	now := time.Date(2026, 11, 25, 0, 0, 0, 250000000, time.UTC)
	for _, tc := range []struct {
		name, value string
		want        int
	}{
		{"missing", "", 0}, {"seconds", " 120 ", 120}, {"zero", "0", 0},
		{"negative", "-3", 0}, {"invalid", "fixture-secret", 0},
		{"cap", "604801", 604800}, {"large", "9223372036854775807", 604800},
		{"date", now.Add(120 * time.Second).Format(http.TimeFormat), 120},
		{"round_up", now.Add(time.Second).Format(http.TimeFormat), 1},
		{"past", now.Add(-time.Second).Format(http.TimeFormat), 0},
		{"far_date", now.Add(30 * 24 * time.Hour).Format(http.TimeFormat), 604800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryAfterSeconds(tc.value, now); got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestBPSHandshakeFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		reason string
		retry  bool
	}{
		{"eof", io.EOF, "network_eof", true},
		{"unexpected_eof", io.ErrUnexpectedEOF, "network_eof", true},
		{"reset", syscall.ECONNRESET, "connection_interrupted", true},
		{"abort", syscall.ECONNABORTED, "connection_interrupted", true},
		{"pipe", syscall.EPIPE, "connection_interrupted", true},
		{"refused", syscall.ECONNREFUSED, "connection_refused", false},
		{"temporary_dns", &net.DNSError{IsTemporary: true}, "dns", true},
		{"dns_timeout", &net.DNSError{IsTimeout: true}, "dns", true},
		{"missing_dns", &net.DNSError{IsNotFound: true}, "dns", false},
		{"authority", x509.UnknownAuthorityError{}, "tls_certificate", false},
		{"verification", &tls.CertificateVerificationError{Err: io.EOF}, "tls_certificate", false},
		{"hostname", x509.HostnameError{}, "tls_certificate", false},
		{"invalid_certificate", x509.CertificateInvalidError{}, "tls_certificate", false},
		{"canceled", context.Canceled, "canceled", false},
		{"unknown", errors.New("fixture-secret"), "unclassified", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, retry := handshakeFailureKind(fmt.Errorf("wrapped: %w", tc.err))
			if reason != tc.reason || retry != tc.retry {
				t.Fatalf("got (%s,%t), want (%s,%t)", reason, retry, tc.reason, tc.retry)
			}
		})
	}
}

func TestBPSHandshakeRetryThenSingleGeneration(t *testing.T) {
	var attempts, generations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errAccept := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if errAccept != nil {
			t.Error(errAccept)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, body, errRead := conn.Read(r.Context())
		if errRead != nil {
			t.Error(errRead)
			return
		}
		generations.Add(1)
		if string(body) != "one-generation" {
			t.Error("unexpected payload")
		}
		if errWrite := conn.Write(r.Context(), websocket.MessageText, []byte("done")); errWrite != nil {
			t.Error(errWrite)
			return
		}
		_, _, _ = conn.Read(r.Context())
	}))
	defer server.Close()
	client := server.Client()
	base := client.Transport
	client.Transport = handshakeTransport(func(r *http.Request) (*http.Response, error) {
		if attempts.Add(1) < 3 {
			return nil, io.ErrUnexpectedEOF
		}
		return base.RoundTrip(r)
	})
	var waits []time.Duration
	conn, errDial := dialWithRetry(t.Context(), server.URL, &websocket.DialOptions{HTTPClient: client}, func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return nil })
	if errDial != nil {
		t.Fatal(errDial)
	}
	defer func() { _ = conn.CloseNow() }()
	if errWrite := conn.Write(t.Context(), websocket.MessageText, []byte("one-generation")); errWrite != nil {
		t.Fatal(errWrite)
	}
	if _, _, errRead := conn.Read(t.Context()); errRead != nil {
		t.Fatal(errRead)
	}
	if attempts.Load() != 3 || generations.Load() != 1 || len(waits) != 2 || waits[0] != 250*time.Millisecond || waits[1] != 500*time.Millisecond {
		t.Fatalf("attempts=%d generations=%d waits=%v", attempts.Load(), generations.Load(), waits)
	}
}

func TestBPSHandshakeStopsRetriesAndRedactsErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		calls int
	}{
		{"exhausted", io.EOF, 3},
		{"certificate", x509.UnknownAuthorityError{}, 1},
		{"refused", syscall.ECONNREFUSED, 1},
		{"unknown", errors.New("fixture-secret"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, waits := 0, 0
			client := &http.Client{Transport: handshakeTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, fmt.Errorf("openai-bearer.fixture-secret: %w", tc.err)
			})}
			_, errDial := dialWithRetry(t.Context(), "https://example.invalid", &websocket.DialOptions{HTTPClient: client}, func(context.Context, time.Duration) error { waits++; return nil })
			var failure *pluginabi.Error
			if !errors.As(errDial, &failure) || !failure.IsRequestScoped() || failure.HTTPStatus != 502 {
				t.Fatalf("unexpected error: %v", errDial)
			}
			if calls != tc.calls || waits != tc.calls-1 {
				t.Fatalf("calls=%d waits=%d", calls, waits)
			}
			if strings.Contains(errDial.Error(), "fixture-secret") || strings.Contains(errDial.Error(), "example.invalid") {
				t.Fatal("sensitive transport error escaped")
			}
		})
	}
}

func TestBPSHandshakeHTTPRejectionsDoNotRetry(t *testing.T) {
	for _, status := range []int{200, 302, 401, 403, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "120")
				if status == 302 {
					w.Header().Set("Location", "https://example.invalid")
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte("fixture-secret"))
			}))
			defer server.Close()
			_, errOpen := Open(t.Context(), server.Client(), server.URL, "fixture-secret", []byte("{}"))
			var failure *pluginabi.Error
			if !errors.As(errOpen, &failure) {
				t.Fatalf("unexpected error: %v", errOpen)
			}
			if calls.Load() != 1 || strings.Contains(errOpen.Error(), "fixture-secret") {
				t.Fatalf("calls=%d error=%v", calls.Load(), errOpen)
			}
			if status == 200 {
				if !failure.IsRequestScoped() || failure.HTTPStatus != 502 {
					t.Fatalf("invalid upgrade: %+v", failure)
				}
			} else if failure.HTTPStatus != status || failure.RetryAfterSeconds != 120 {
				t.Fatalf("lost rejection metadata: %+v", failure)
			}
			if status >= 300 && failure.Code != "bps_upstream_rejected" {
				t.Fatal("changed HTTP rejection classification without upstream evidence")
			}
		})
	}
}

func TestBPSHandshakeCancellationStopsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	client := &http.Client{Transport: handshakeTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, io.EOF })}
	_, errDial := dialWithRetry(ctx, "https://example.invalid", &websocket.DialOptions{HTTPClient: client}, func(ctx context.Context, d time.Duration) error { cancel(); return waitHandshakeRetry(ctx, d) })
	if !errors.Is(errDial, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, errDial)
	}
}
