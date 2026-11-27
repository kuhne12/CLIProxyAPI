package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
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
	cfg        *config.Config
	endpoint   string
	imageRelay *helps.BPSImageRelay
}

var bpsSlots bpsbridge.SlotPool
var bpsReplay bpsbridge.ReplayCache

func NewBPSExecutor(cfg *config.Config) *BPSExecutor {
	return &BPSExecutor{cfg: cfg, endpoint: bpsbridge.ResponsesURL}
}

// WithImageRelay binds the service-owned image store and download handler.
func (e *BPSExecutor) WithImageRelay(relay *helps.BPSImageRelay) *BPSExecutor {
	e.imageRelay = relay
	return e
}
func (*BPSExecutor) Identifier() string { return "bps" }

func (e *BPSExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (resp coreexecutor.Response, err error) {
	observation := helps.NewBPSObservation(len(req.Payload))
	defer func() { observation.Finish(ctx, err) }()
	cacheCreationAsInput := helps.BPSCacheCreationAsInput(e.cfg)
	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, auth)
	defer reporter.TrackFailure(ctx, &err)
	defer func() {
		if err != nil {
			observation.PublishFailureUsage(ctx, reporter, err)
		}
	}()
	body, prepared, errOpen := e.open(ctx, auth, req, opts, reporter, observation)
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
		helps.ObserveResponsesTokenEvent(reporter, data)
		observation.ObserveFailureUsage(data)
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
		return resp, observation.ToolRecoveryError(errRead)
	}
	if len(terminal) == 0 {
		return resp, observation.ToolRecoveryError(helps.BPSMissingTerminal())
	}
	reporter.ObserveResponseModel(terminal)
	if detail, ok := helps.ParseCodexUsage(terminal); ok {
		reporter.Publish(ctx, detail)
	} else {
		reporter.EnsurePublished(ctx)
	}
	if cacheCreationAsInput {
		terminal = helps.BPSDownstreamUsage(terminal)
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
	observation.Mark("response_ready")
	return coreexecutor.Response{Payload: payload, Headers: observation.Headers("application/json")}, nil
}

func (e *BPSExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (result *coreexecutor.StreamResult, err error) {
	observation := helps.NewBPSObservation(len(req.Payload))
	defer func() {
		if err != nil {
			observation.Finish(ctx, err)
		}
	}()
	cacheCreationAsInput := helps.BPSCacheCreationAsInput(e.cfg)
	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, auth)
	defer reporter.TrackFailure(ctx, &err)
	body, prepared, errOpen := e.open(ctx, auth, req, opts, reporter, observation)
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
		var errStream error
		defer func() { observation.Finish(ctx, errStream) }()
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
			helps.ObserveResponsesTokenEvent(reporter, data)
			observation.ObserveFailureUsage(data)
			if errEvent := bpsbridge.EventError(data); errEvent != nil {
				return errEvent
			}
			kind := ojson.GetBytes(data, "type").String()
			if kind == "response.completed" || kind == "response.incomplete" {
				terminal = true
				reporter.ObserveResponseModel(data)
				if detail, ok := helps.ParseCodexUsage(data); ok {
					reporter.Publish(ctx, detail)
				}
			}
			if cacheCreationAsInput {
				data = helps.BPSDownstreamUsage(data)
			}
			line := append([]byte("data: "), data...)
			frames := sdktranslator.TranslateStream(ctx, sdktranslator.FormatCodex, target, req.Model, original, prepared, line, &param)
			for _, frame := range frames {
				if errSend := send(coreexecutor.StreamChunk{Payload: frame}); errSend != nil {
					return errSend
				}
			}
			if len(frames) > 0 {
				observation.Handoff(data)
			}
			return nil
		})
		if errRead == nil && !terminal {
			errRead = helps.BPSMissingTerminal()
		}
		if errRead != nil {
			errRead = observation.ToolRecoveryError(errRead)
			errStream = errRead
			observation.PublishFailureUsage(ctx, reporter, errRead)
			_ = send(coreexecutor.StreamChunk{Err: errRead})
		} else {
			reporter.EnsurePublished(ctx)
		}
	}()
	return &coreexecutor.StreamResult{Headers: observation.Headers("text/event-stream"), Chunks: out}, nil
}

func (e *BPSExecutor) open(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options, reporter *helps.UsageReporter, observation *helps.BPSObservation) (io.ReadCloser, []byte, error) {
	if opts.Alt == "responses/compact" {
		return nil, nil, &pluginabi.Error{Code: pluginabi.ErrorCodeNotImplemented, HTTPStatus: 501, Message: "Upstream requires complete history and has no standalone compaction endpoint"}
	}
	if auth == nil || auth.Provider != "bps" {
		return nil, nil, &pluginabi.Error{Code: "bps_auth_required", HTTPStatus: 401, Message: "An upstream authorization file is required"}
	}
	token, _ := auth.Metadata["access_token"].(string)
	account, _ := auth.Metadata["account_id"].(string)
	if token == "" || account == "" {
		return nil, nil, &pluginabi.Error{Code: "bps_auth_required", HTTPStatus: 401, Message: "Upstream credential is missing its access token or account ID"}
	}
	ctx = bpsbridge.WithObserver(ctx, observation.ObserverWithUsageErrorLogging(ctx, reporter, token))
	subject, _ := auth.Metadata["sub"].(string)
	queueDone := observation.Stage("queue")
	options, release, errSlot := bpsSlots.Acquire(ctx, account+"\x00"+subject)
	queueDone()
	if errSlot != nil {
		return nil, nil, errSlot
	}
	transferred := false
	defer func() {
		if !transferred {
			release()
		}
	}()
	prepareDone := observation.Stage("prepare")
	defer prepareDone()
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	nativeModel := bpsbridge.NativeModel(baseModel)
	modelInfo, resolved := coreauth.ResolvedModelInfo(req)
	for _, model := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
		if !resolved && model != nil && (model.ID == baseModel || model.MetadataModelID == nativeModel) {
			modelInfo = model
			break
		}
	}
	if modelInfo != nil && modelInfo.Thinking != nil {
		options.EffortLevels = modelInfo.Thinking.Levels
	}
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
	// The shared Codex translator strips this field, but BPS supports it.
	// Restore it before BPS conversion and the final user payload rules.
	if management := ojson.GetBytes(req.Payload, "context_management"); management.IsArray() {
		_ = doc.SetRaw("context_management", management.Raw)
	}
	// Default rules must recognize the caller's restored fields as explicit.
	original := doc.Bytes()
	_ = doc.Set("model", nativeModel)
	if errThinking := helps.ApplyBPSRequestThinkingDoc(doc, req, opts, from.String(), modelInfo, translated.ConfigurationUpdatesChanged); errThinking != nil {
		return nil, nil, errThinking
	}
	caller, _ := opts.Metadata[coreexecutor.CallerScopeMetadataKey].(string)
	session, _ := opts.Metadata[coreexecutor.CanonicalSessionIDMetadataKey].(string)
	scopeHash := sha256.Sum256([]byte(auth.ID + "\x00" + caller + "\x00" + session))
	scope := hex.EncodeToString(scopeHash[:])
	rememberCompaction, errHistory := helps.PrepareBPSHistory(ctx, doc, scope, &options, observation)
	if errHistory != nil {
		return nil, nil, fmt.Errorf("prepare compacted history: %w", errHistory)
	}
	// Fingerprint the client's history before process-local delegation proofs
	// can change its representation. Compaction retains all agent messages.
	helps.NormalizeBPSDelegationInputDoc(doc, opts)
	if errImage := e.imageRelay.RewriteDoc(doc, scope, helps.BPSImageRelayOrigin(e.cfg)); errImage != nil {
		status := http.StatusBadRequest
		if errors.Is(errImage, bpsbridge.ErrImageRelayFull) || errors.Is(errImage, bpsbridge.ErrImageRelayStorage) {
			status = http.StatusServiceUnavailable
		}
		return nil, nil, &pluginabi.Error{Code: pluginabi.ErrorCodeRequestScoped, HTTPStatus: status, Message: errImage.Error()}
	}
	upstream, bridge, errPrepare := bpsbridge.PrepareWithOptions(doc.Bytes(), scope, &bpsReplay, options)
	if errPrepare != nil {
		return nil, nil, &pluginabi.Error{Code: pluginabi.ErrorCodeRequestScoped, HTTPStatus: 400, Message: errPrepare.Error()}
	}
	final := ojson.NewDoc(upstream)
	bpsbridge.Frame(final)
	// Apply rules once, after every BPS conversion, injection, and cleanup.
	finalize := helps.NewPayloadFinalizer(e.cfg, e.Identifier(), baseModel, sdktranslator.FormatCodex.String(), "", original, req, opts, auth)
	finalize.FinalizeDoc(final)
	payload := final.Bytes()
	inputUnchanged := ojson.GetBytesNoCopy(upstream, "input").Raw == ojson.GetBytesNoCopy(payload, "input").Raw
	observation.SetRequestDiagnostic(scope, bridge.CatalogDiagnostic(), inputUnchanged)
	bridge.OnProtocolError = func(d bpsbridge.ProtocolDiagnostic) { observation.ProtocolFailure(ctx, d) }
	bridge.OnToolHandoff = func(data []byte) {
		if ctx.Err() == nil {
			helps.RememberBPSDelegationOutput(data, opts)
		}
	}
	reporter.SetUpstreamModel(final.Get("model").String())
	reporter.SetTranslatedReasoningEffort(payload, sdktranslator.FormatOpenAI.String())
	if opts.ProxyURL != "" {
		ctx = coreexecutor.WithRequestProxyURL(ctx, opts.ProxyURL)
	}
	client := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	observation.SetWireBytes(len(payload))
	prepareDone()
	// Match other executors: TTFT starts at upstream I/O, excluding local queue
	// and preparation. The observation log measures the full executor lifetime.
	reporter.StartResponseTTFT()
	openDone := observation.Stage("upstream_open")
	socket, errOpen := bpsbridge.Open(ctx, client, e.endpoint, token, payload)
	openDone()
	if errOpen != nil {
		return nil, nil, errOpen
	}
	// A checkpoint can only cover the input actually sent after payload rules.
	if rememberCompaction != nil {
		bridge.OnInlineCompaction = func(id, state string) {
			if inputUnchanged {
				rememberCompaction(id, state)
			} else {
				observation.SkipCompactionConfirmation()
			}
		}
	}
	var reopen bpsbridge.CorrectionOpener
	if inputUnchanged {
		reopen = func(correction bpsbridge.ToolCorrection) (io.ReadCloser, error) {
			// Rebuild from the unconfigured business payload. Rules run once for
			// this continuation, after all internal history and framing changes.
			next := ojson.NewDoc(upstream)
			if errCorrection := correction.Apply(next); errCorrection != nil {
				return nil, errCorrection
			}
			bpsbridge.Frame(next)
			correctionInput := next.Get("input").Raw
			finalize.FinalizeDoc(next)
			// Validation is read-only: never restore history removed by user rules.
			if next.Get("input").Raw != correctionInput {
				return nil, fmt.Errorf("Upstream corrective history was changed by final payload rules")
			}
			nextPayload := next.Bytes()
			observation.RecordToolRecoveryBytes(len(nextPayload))
			return bpsbridge.Open(ctx, client, e.endpoint, token, nextPayload)
		}
	}
	transformed := bridge.StreamWithToolRecovery(ctx, socket, reopen, observation.RecordToolRecovery, observation.ObserveFailureUsage)
	stop := context.AfterFunc(ctx, func() { _ = transformed.Close() })
	transferred = true
	return &helps.BPSBody{ReadCloser: transformed, Release: func() { stop(); release() }}, payload, nil
}

// These endpoints have no BPS equivalent. The plugin owns OAuth refresh.
func (*BPSExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &pluginabi.Error{Code: pluginabi.ErrorCodeNotImplemented, HTTPStatus: 501, Message: "Upstream has no token counting endpoint"}
}
func (*BPSExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &pluginabi.Error{Code: pluginabi.ErrorCodeNotImplemented, HTTPStatus: 501, Message: "Upstream supports generation through its WebSocket executor only"}
}
func (*BPSExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, fmt.Errorf("Credential refresh requires the enabled authorization plugin")
}
