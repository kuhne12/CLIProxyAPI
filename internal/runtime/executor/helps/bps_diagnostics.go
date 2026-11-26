package helps

import (
	"context"
	"errors"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/bpsbridge"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	log "github.com/sirupsen/logrus"
)

// Diagnostics are collected per request and included in the existing timing
// entry. Callers supply only fixed reasons, bounded identifiers and counts.
func (o *BPSObservation) recordDiagnostic(fields log.Fields) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.diagnostics == nil {
		o.diagnostics = make(log.Fields)
	}
	for key, value := range fields {
		o.diagnostics[key] = value
	}
}

func (o *BPSObservation) SetRequestDiagnostic(scope string, catalog bpsbridge.CatalogDiagnostic, inputUnchanged bool) {
	o.recordDiagnostic(log.Fields{
		"scope_hash": scope, "tool_catalog_count": catalog.Count, "tool_catalog_hash": catalog.Hash,
		"tool_catalog_names": catalog.Names, "tool_catalog_names_truncated": catalog.Count > len(catalog.Names),
		"payload_input_unchanged": inputUnchanged,
	})
}

func (o *BPSObservation) ProtocolFailure(ctx context.Context, d bpsbridge.ProtocolDiagnostic) {
	o.recordDiagnostic(log.Fields{
		"validation_stage": d.Stage, "validation_reason": d.Reason, "validation_event": d.EventType,
		"validation_output_index": d.OutputIndex, "pending_tool_count": d.PendingTools,
		"missing_tools": d.MissingTools, "validation_compaction_seen": d.CompactionSeen,
		"native_tool_type": d.Tool.Type, "native_tool_name": d.Tool.Name,
		"client_tool_name": d.Tool.ClientName, "native_arguments_type": d.Tool.ArgumentsType,
		"native_call_hash": d.Tool.CallHash, "native_item_hash": d.Tool.ItemHash,
		"native_arguments_bytes": d.Tool.ArgumentsBytes, "native_transport_shape": d.Tool.TransportShape,
	})
	LogWithRequestID(ctx).WithField("provider", "bps").WithFields(o.fields()).Warn("BPS response validation failed")
}

func (o *BPSObservation) observeCompaction(item ojson.Result) {
	if item.Get("type").String() != "compaction" {
		return
	}
	o.recordDiagnostic(log.Fields{
		"compaction_seen":           true,
		"compaction_output_id_hash": bpsbridge.DiagnosticHash(item.Get("id").String()),
		"compaction_confirmation":   "pending_validation",
	})
}

func (o *BPSObservation) ConfirmCompaction(id string) {
	o.recordDiagnostic(log.Fields{
		"compaction_confirmation":      "confirmed",
		"compaction_confirmed_id_hash": bpsbridge.DiagnosticHash(id),
	})
}

func (o *BPSObservation) SkipCompactionConfirmation() {
	o.recordDiagnostic(log.Fields{"compaction_confirmation": "payload_input_changed"})
}

func (o *BPSObservation) RecordToolRecovery(status string) {
	o.recordDiagnostic(log.Fields{"tool_recovery_attempts": 1, "tool_recovery_outcome": status})
}

func (o *BPSObservation) RecordToolRecoveryBytes(size int) {
	o.recordDiagnostic(log.Fields{"tool_recovery_upstream_bytes": size})
}

// A corrective failure must not make the scheduler replay the accepted first
// generation, even if the correction itself was rejected before generation.
func (o *BPSObservation) ToolRecoveryError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	o.mu.Lock()
	attempted := o.diagnostics["tool_recovery_attempts"] == 1
	o.mu.Unlock()
	if !attempted {
		return err
	}
	var upstream *pluginabi.Error
	if errors.As(err, &upstream) {
		failure := *upstream
		failure.Code = pluginabi.ErrorCodeStreamTruncated
		failure.Retryable, failure.CredentialScoped = false, false
		return &failure
	}
	return &pluginabi.Error{Code: pluginabi.ErrorCodeStreamTruncated, HTTPStatus: 502,
		Message: "Upstream corrective generation failed; original request was not replayed"}
}

// Preserve completed usage if the corrective connection or response fails.
func (o *BPSObservation) ObserveFailureUsage(data []byte) {
	if !ojson.GetBytes(data, "response.usage").Exists() {
		return
	}
	if detail, ok := ParseCodexUsage(data); ok {
		o.mu.Lock()
		o.failureUsage, o.hasFailureUsage = detail, true
		o.mu.Unlock()
	}
}

func (o *BPSObservation) PublishFailureUsage(ctx context.Context, reporter *UsageReporter, err error) {
	o.mu.Lock()
	detail, found := o.failureUsage, o.hasFailureUsage
	o.mu.Unlock()
	if found {
		reporter.PublishFailureWithDetail(ctx, detail, err)
	} else {
		reporter.PublishFailure(ctx, err)
	}
}
