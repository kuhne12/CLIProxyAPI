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
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	log "github.com/sirupsen/logrus"
)

const maxHandshakeAttempts = 3

// dialWithRetry retries only known transient failures before any generation is sent.
// Caller cancellation controls the budget; no deadline is added to an established socket.
func dialWithRetry(ctx context.Context, address string, opts *websocket.DialOptions, wait func(context.Context, time.Duration) error) (*websocket.Conn, error) {
	observer := observerFromContext(ctx)
	for attempt := 1; ; attempt++ {
		if observer.DialStart != nil {
			observer.DialStart()
		}
		conn, resp, errDial := websocket.Dial(ctx, address, opts)
		if observer.DialDone != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			observer.DialDone(status)
		}
		if errDial == nil {
			return conn, nil
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if resp != nil {
			if resp.StatusCode >= 300 && resp.StatusCode <= 599 {
				// A bare HTTP 429 does not distinguish quota from capacity limits.
				// Preserve CPA's existing HTTP rejection classification.
				return nil, &pluginabi.Error{Code: "bps_upstream_rejected", HTTPStatus: resp.StatusCode,
					RetryAfterSeconds: retryAfterSeconds(resp.Header.Get("Retry-After"), time.Now()),
					Message:           fmt.Sprintf("BPS WebSocket handshake rejected (HTTP %d)", resp.StatusCode)}
			}
			return nil, handshakeStopped("invalid_upgrade", attempt)
		}
		reason, transient := handshakeFailureKind(errDial)
		log.WithFields(log.Fields{"reason": reason, "attempt": attempt, "request_not_sent": true}).Debug("BPS WebSocket handshake failed")
		if !transient || attempt >= maxHandshakeAttempts {
			return nil, handshakeStopped(reason, attempt)
		}
		if errWait := wait(ctx, time.Duration(attempt)*250*time.Millisecond); errWait != nil {
			return nil, errWait
		}
	}
}

func handshakeStopped(reason string, attempts int) error {
	// Request-scoped failures stop CPA's outer credential rotation and retry loop.
	// This avoids multiplying the bounded handshake attempts or penalizing credentials.
	return &pluginabi.Error{Code: pluginabi.ErrorCodeRequestScoped, HTTPStatus: http.StatusBadGateway,
		Message: fmt.Sprintf("BPS WebSocket connection failed before sending the request (%s; attempts=%d)", reason, attempts)}
}

func waitHandshakeRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// handshakeFailureKind follows the upstream pre-generation retry policy.
func handshakeFailureKind(err error) (string, bool) {
	var verify *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &verify) || errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid) {
		return "tls_certificate", false
	}
	if errors.Is(err, context.Canceled) {
		return "canceled", false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "network_eof", true
	}
	if runtime.GOOS == "windows" {
		if errors.Is(err, syscall.Errno(10054)) || errors.Is(err, syscall.Errno(10053)) {
			return "connection_interrupted", true
		}
		if errors.Is(err, syscall.Errno(10061)) {
			return "connection_refused", false
		}
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) {
		return "connection_interrupted", true
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection_refused", false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns", dns.IsTimeout || dns.IsTemporary
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return "network_timeout", true
	}
	return "unclassified", false
}

func retryAfterSeconds(value string, now time.Time) int {
	const maximum = 7 * 24 * 60 * 60
	value = strings.TrimSpace(value)
	if seconds, errParse := strconv.ParseInt(value, 10, 64); errParse == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > maximum {
			return maximum
		}
		return int(seconds)
	}
	date, errParse := http.ParseTime(value)
	if errParse != nil {
		return 0
	}
	remaining := date.Sub(now)
	if remaining <= 0 {
		return 0
	}
	if remaining >= maximum*time.Second {
		return maximum
	}
	return int((remaining + time.Second - 1) / time.Second)
}
