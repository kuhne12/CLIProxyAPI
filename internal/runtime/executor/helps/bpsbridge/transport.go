package bpsbridge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func NativeModel(model string) string { return strings.TrimPrefix(model, "bps-") }

// Frame adds only WebSocket transport fields. Call before the final payload barrier.
func Frame(doc *ojson.Doc) {
	_ = doc.Set("type", "response.create")
	_ = doc.Set("basispoints_request_id", uuid.NewString())
	_ = doc.Set("basispoints_resume_token", "resume_"+strings.ReplaceAll(uuid.NewString(), "-", ""))
}

// Open uses the public BPS browser WebSocket protocol. It never retries a sent request.
func Open(ctx context.Context, client *http.Client, endpoint, token string, body []byte) (io.ReadCloser, error) {
	u, errURL := url.Parse(endpoint)
	if errURL != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid BPS endpoint")
	}
	platform := "PC"
	if runtime.GOOS == "darwin" {
		platform = "Mac"
	}
	profile, _ := json.Marshal(map[string]string{
		"X-Openai-Internal-Basispoints-Client-Product":        "basispoints-excel-plugin",
		"X-Openai-Internal-Basispoints-Client-Platform":       "excel",
		"X-Openai-Internal-Basispoints-Client-Agent-Profile":  "excel",
		"X-Openai-Internal-Basispoints-Client-Editor":         "excel",
		"X-Openai-Internal-Basispoints-Client-Host":           "office",
		"X-Openai-Internal-Basispoints-Client-Runtime":        "desktop",
		"X-Openai-Internal-Basispoints-Client-Platform-Class": platform,
		"X-Openai-Internal-Basispoints-Office-Host":           "Excel",
		"X-Openai-Internal-Basispoints-Office-Platform":       platform,
		"X-Openai-Internal-Basispoints-Browser-Name":          "unknown",
	})
	q := u.Query()
	q.Set("bps_client_info", string(profile))
	q.Set("bps_auth_mode", "chatgpt")
	q.Set("bps_ws_affinity", ojson.GetBytes(body, "basispoints_request_id").String())
	q.Set("bps_control_frames", "upstream_sent,heartbeat")
	u.RawQuery = q.Encode()
	scoped := *client
	scoped.Timeout = 0
	scoped.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	conn, resp, errDial := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient:   &scoped,
		HTTPHeader:   http.Header{"Origin": {"https://bps.openai.com"}, "User-Agent": {"Mozilla/5.0"}, "Accept": {"*/*"}},
		Subprotocols: []string{"responses", "openai-bearer." + token},
	})
	if errDial != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			return nil, &pluginabi.Error{Code: "bps_upstream_rejected", HTTPStatus: resp.StatusCode, Message: fmt.Sprintf("BPS WebSocket authorization rejected (HTTP %d)", resp.StatusCode)}
		}
		// Do not expose transport errors that can contain bearer subprotocols.
		return nil, &pluginabi.Error{Code: pluginabi.ErrorCodeTransientTransport, HTTPStatus: 502, Message: "BPS WebSocket connection failed before sending the request"}
	}
	conn.SetReadLimit((16 << 20) - 1024)
	if errWrite := conn.Write(ctx, websocket.MessageText, body); errWrite != nil {
		_ = conn.CloseNow()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &pluginabi.Error{Code: pluginabi.ErrorCodeStreamTruncated, HTTPStatus: 502, Message: "BPS request transmission failed; generation was not replayed"}
	}
	return &socketBody{ctx: ctx, conn: conn, pending: bytes.NewReader(nil)}, nil
}

type socketBody struct {
	ctx      context.Context
	conn     *websocket.Conn
	pending  *bytes.Reader
	terminal bool
	once     sync.Once
}

func (b *socketBody) Close() error {
	b.once.Do(func() { _ = b.conn.CloseNow() })
	return nil
}

func (b *socketBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for b.pending.Len() == 0 {
		if b.terminal {
			return 0, io.EOF
		}
		kind, data, errRead := b.conn.Read(b.ctx)
		if errRead != nil {
			if b.ctx.Err() != nil {
				return 0, b.ctx.Err()
			}
			return 0, &pluginabi.Error{Code: pluginabi.ErrorCodeStreamTruncated, HTTPStatus: 502, Message: "BPS disconnected before a terminal response; generation was not replayed"}
		}
		if kind != websocket.MessageText || !ojson.ValidBytes(data) {
			return 0, protocolFailure()
		}
		doc := ojson.NewDoc(data)
		event := doc.Get("type").String()
		switch event {
		case "basispoints.response.resume_token", "basispoints.response.metadata", "basispoints.response.upstream_sent", "basispoints.response.heartbeat":
			continue
		case "basispoints.response.server_draining":
			return 0, &pluginabi.Error{Code: pluginabi.ErrorCodeStreamTruncated, HTTPStatus: 503, Message: "BPS server is draining; generation was not replayed"}
		}
		if event != "error" && !strings.HasPrefix(event, "response.") {
			return 0, protocolFailure()
		}
		for _, c := range event {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_') {
				return 0, protocolFailure()
			}
		}
		if len(event) > 128 {
			return 0, protocolFailure()
		}
		_ = doc.Delete("basispoints_replay_cursor")
		b.terminal = event == "response.completed" || event == "response.incomplete" || event == "response.failed" || event == "error"
		// WebSocket JSON may be pretty-printed. Each SSE data field must stay on
		// one physical line, while escaped newlines inside strings remain intact.
		var compact bytes.Buffer
		if errCompact := json.Compact(&compact, doc.Bytes()); errCompact != nil {
			return 0, protocolFailure()
		}
		b.pending.Reset([]byte("event: " + event + "\ndata: " + compact.String() + "\n\n"))
	}
	return b.pending.Read(p)
}

func protocolFailure() error {
	return &pluginabi.Error{Code: pluginabi.ErrorCodeStreamTruncated, HTTPStatus: 502, Message: "BPS returned an invalid protocol event"}
}

// EventError keeps provider failures visible to CPA, without echoing upstream secrets.
func EventError(data []byte) error {
	kind := ojson.GetBytes(data, "type").String()
	if kind != "error" && kind != "response.failed" {
		return nil
	}
	detail := ojson.GetBytes(data, "error")
	if !detail.IsObject() {
		detail = ojson.GetBytes(data, "response.error")
	}
	code := detail.Get("code").String()
	failure := &pluginabi.Error{Code: pluginabi.ErrorCodeRequestScoped, HTTPStatus: 400, Message: "BPS rejected the generation request"}
	switch code {
	case "rate_limit_exceeded", "too_many_requests", "concurrent_request_limit":
		failure.Code = pluginabi.ErrorCodeTransientRateLimit
		failure.HTTPStatus = 429
		failure.RetryAfterSeconds = 1
		failure.Message = "BPS is temporarily rate limited; retry later"
	case "insufficient_quota", "usage_limit_reached", "quota_exceeded":
		failure.Code = pluginabi.ErrorCodeCredentialQuota
		failure.HTTPStatus = 429
		failure.CredentialScoped = true
		failure.Message = "BPS account quota is exhausted"
	case "invalid_api_key", "invalid_token", "token_expired", "unauthorized":
		failure.Code = "bps_auth_unavailable"
		failure.HTTPStatus = 401
		failure.Message = "BPS authorization is no longer valid; refresh or sign in again"
	case "model_not_found", "model_not_available", "unsupported_model":
		failure.Code = pluginabi.ErrorCodeModelNotFound
		failure.HTTPStatus = 404
		failure.Message = "This model is unavailable for the BPS credential"
	case "server_error", "internal_error":
		failure.Code = pluginabi.ErrorCodeStreamTruncated
		failure.HTTPStatus = 502
		failure.Message = "BPS failed after accepting the request; generation was not replayed"
	case "basispoints_protocol_error":
		failure.Code = pluginabi.ErrorCodeStreamTruncated
		failure.HTTPStatus = 502
		failure.Message = "BPS returned an unsupported response or tool call"
	}
	return failure
}
