package helps

import (
	"context"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/bpsbridge"
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
