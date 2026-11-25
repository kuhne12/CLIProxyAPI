package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/bpsbridge"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// BPSExecutor reuses the public bps-local Responses adapter and CPA's native
// request lifecycle. OAuth acquisition/refresh and model discovery belong to
// the BPS plugin; this executor never treats its grant as a Codex credential.
type BPSExecutor struct {
	cfg      *config.Config
	endpoint string
}

var bpsSlots bpsbridge.SlotPool
var bpsReplay bpsbridge.ReplayCache

func NewBPSExecutor(cfg *config.Config) *BPSExecutor {
	return &BPSExecutor{cfg: cfg, endpoint: bpsbridge.ResponsesURL}
}
func (*BPSExecutor) Identifier() string { return "bps" }

func (e *BPSExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (resp coreexecutor.Response, err error) {
	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, auth)
	defer reporter.TrackFailure(ctx, &err)
	body, prepared, errOpen := e.open(ctx, auth, req, opts, reporter)
	if errOpen != nil {
		return resp, errOpen
	}
	defer func() {
		if errClose := body.Close(); errClose != nil {
			log.Debug("BPS response body close failed")
		}
	}()
	var terminal []byte
	errRead := helps.ConsumeBPSEvents(body, func(data []byte) error {
		if errEvent := bpsbridge.EventError(data); errEvent != nil {
			return errEvent
		}
		kind := ojson.GetBytes(data, "type").String()
		if kind == "response.completed" || kind == "response.incomplete" {
			terminal = bytes.Clone(data)
		}
		return nil
	})
	if errRead != nil {
		return resp, errRead
	}
	if len(terminal) == 0 {
		return resp, helps.BPSMissingTerminal()
	}
	reporter.ObserveResponseModel(terminal)
	if detail, ok := helps.ParseCodexUsage(terminal); ok {
		reporter.Publish(ctx, detail)
	} else {
		reporter.EnsurePublished(ctx)
	}
	target := coreexecutor.ResponseFormatOrSource(opts)
	if target == "" {
		target = req.Format
	}
	original := opts.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	payload := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatCodex, target, req.Model, original, prepared, terminal, nil)
	if target == sdktranslator.FormatCodex {
		payload = []byte(ojson.GetBytes(terminal, "response").Raw)
	}
	return coreexecutor.Response{Payload: payload, Headers: http.Header{"Content-Type": {"application/json"}}}, nil
}

func (e *BPSExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (result *coreexecutor.StreamResult, err error) {
	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, auth)
	defer reporter.TrackFailure(ctx, &err)
	body, prepared, errOpen := e.open(ctx, auth, req, opts, reporter)
	if errOpen != nil {
		return nil, errOpen
	}
	out := make(chan coreexecutor.StreamChunk)
	target := coreexecutor.ResponseFormatOrSource(opts)
	if target == "" {
		target = req.Format
	}
	original := opts.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	go func() {
		defer close(out)
		defer func() {
			if errClose := body.Close(); errClose != nil {
				log.Debug("BPS response body close failed")
			}
		}()
		var param any
		terminal := false
		send := func(chunk coreexecutor.StreamChunk) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case out <- chunk:
				return nil
			}
		}
		errRead := helps.ConsumeBPSEvents(body, func(data []byte) error {
			if errEvent := bpsbridge.EventError(data); errEvent != nil {
				return errEvent
			}
			reporter.RecordFirstPacket()
			kind := ojson.GetBytes(data, "type").String()
			if kind == "response.output_text.delta" {
				reporter.ObserveTokenEvent(true)
			}
			if kind == "response.completed" || kind == "response.incomplete" {
				terminal = true
				reporter.ObserveResponseModel(data)
				if detail, ok := helps.ParseCodexUsage(data); ok {
					reporter.Publish(ctx, detail)
				}
			}
			line := append([]byte("data: "), data...)
			frames := sdktranslator.TranslateStream(ctx, sdktranslator.FormatCodex, target, req.Model, original, prepared, line, &param)
			for _, frame := range frames {
				if errSend := send(coreexecutor.StreamChunk{Payload: frame}); errSend != nil {
					return errSend
				}
			}
			return nil
		})
		if errRead == nil && !terminal {
			errRead = helps.BPSMissingTerminal()
		}
		if errRead != nil {
			reporter.PublishFailure(ctx, errRead)
			_ = send(coreexecutor.StreamChunk{Err: errRead})
		} else {
			reporter.EnsurePublished(ctx)
		}
	}()
	return &coreexecutor.StreamResult{Headers: http.Header{"Content-Type": {"text/event-stream"}}, Chunks: out}, nil
}

func (e *BPSExecutor) open(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options, reporter *helps.UsageReporter) (io.ReadCloser, []byte, error) {
	if opts.Alt == "responses/compact" {
		return nil, nil, &pluginabi.Error{Code: pluginabi.ErrorCodeNotImplemented, HTTPStatus: 501, Message: "BPS requires complete history and has no standalone compaction endpoint"}
	}
	if auth == nil || auth.Provider != "bps" {
		return nil, nil, &pluginabi.Error{Code: "bps_auth_required", HTTPStatus: 401, Message: "A BPS authorization file is required"}
	}
	token, _ := auth.Metadata["access_token"].(string)
	account, _ := auth.Metadata["account_id"].(string)
	if token == "" || account == "" {
		return nil, nil, &pluginabi.Error{Code: "bps_auth_required", HTTPStatus: 401, Message: "BPS credential is missing its access token or account ID"}
	}
	subject, _ := auth.Metadata["sub"].(string)
	options, release, errSlot := bpsSlots.Acquire(ctx, account+"\x00"+subject)
	if errSlot != nil {
		return nil, nil, errSlot
	}
	transferred := false
	defer func() {
		if !transferred {
			release()
		}
	}()
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	nativeModel := bpsbridge.NativeModel(baseModel)
	from := opts.SourceFormat
	if from == "" {
		from = req.Format
	}
	if from == "" {
		from = sdktranslator.FormatOpenAIResponse
	}
	translated := sdktranslator.TranslateRequestEnvelope(ctx, from, sdktranslator.FormatCodex, sdktranslator.RequestEnvelope{Format: from, Model: nativeModel, Stream: true, Body: req.Payload})
	if translated.Err != nil {
		return nil, nil, translated.Err
	}
	doc := ojson.NewDoc(translated.Body)
	_ = doc.Set("model", nativeModel)
	if errThinking := helps.ApplyRequestThinkingDoc(doc, req, opts, from.String(), sdktranslator.FormatCodex.String(), e.Identifier(), translated.ConfigurationUpdatesChanged); errThinking != nil {
		return nil, nil, errThinking
	}
	caller, _ := opts.Metadata[coreexecutor.CallerScopeMetadataKey].(string)
	session, _ := opts.Metadata[coreexecutor.CanonicalSessionIDMetadataKey].(string)
	scopeHash := sha256.Sum256([]byte(auth.ID + "\x00" + caller + "\x00" + session))
	upstream, bridge, errPrepare := bpsbridge.PrepareWithOptions(doc.Bytes(), hex.EncodeToString(scopeHash[:]), &bpsReplay, options)
	if errPrepare != nil {
		return nil, nil, &pluginabi.Error{Code: pluginabi.ErrorCodeRequestScoped, HTTPStatus: 400, Message: errPrepare.Error()}
	}
	final := ojson.NewDoc(upstream)
	bpsbridge.Frame(final)
	// Apply rules once, after every BPS conversion, injection, and cleanup.
	finalize := helps.NewPayloadFinalizer(e.cfg, e.Identifier(), baseModel, sdktranslator.FormatCodex.String(), "", translated.Body, req, opts, auth)
	finalize.FinalizeDoc(final)
	payload := final.Bytes()
	reporter.SetUpstreamModel(final.Get("model").String())
	reporter.SetTranslatedReasoningEffort(payload, sdktranslator.FormatOpenAI.String())
	if opts.ProxyURL != "" {
		ctx = coreexecutor.WithRequestProxyURL(ctx, opts.ProxyURL)
	}
	client := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	socket, errOpen := bpsbridge.Open(ctx, client, e.endpoint, token, payload)
	if errOpen != nil {
		return nil, nil, errOpen
	}
	transformed := bridge.Stream(socket)
	stop := context.AfterFunc(ctx, func() { _ = transformed.Close() })
	transferred = true
	return &helps.BPSBody{ReadCloser: transformed, Release: func() { stop(); release() }}, payload, nil
}

// These endpoints have no BPS equivalent. The plugin owns OAuth refresh.
func (*BPSExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &pluginabi.Error{Code: pluginabi.ErrorCodeNotImplemented, HTTPStatus: 501, Message: "BPS has no token counting endpoint"}
}
func (*BPSExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &pluginabi.Error{Code: pluginabi.ErrorCodeNotImplemented, HTTPStatus: 501, Message: "BPS supports generation through its WebSocket executor only"}
}
func (*BPSExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, fmt.Errorf("BPS refresh requires the enabled BPS authorization plugin")
}
