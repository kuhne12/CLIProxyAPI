package bpsbridge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
)

// Only locally authored messages may be marked safe for diagnostic logs.
// Unknown errors deliberately retain no message in the diagnostic callback.
type validationError struct {
	error
	reason string
}

func (e validationError) Unwrap() error { return e.error }

func invalidResponse(message string) error {
	return validationError{error: errors.New(message), reason: message}
}

type toolResponseError struct {
	error
	tool  ToolDiagnostic
	index int
}

func (e toolResponseError) Unwrap() error { return e.error }

// ProtocolDiagnostic never contains raw events, argument values or error text
// from external parsers. Tool names are bounded identifiers; IDs are hashed.
type ProtocolDiagnostic struct {
	Stage          string
	Reason         string
	EventType      string
	PendingTools   int
	MissingTools   []string
	CompactionSeen bool
	OutputIndex    int
	Tool           ToolDiagnostic
}

type ToolDiagnostic struct {
	Type           string
	Name           string
	ClientName     string
	CallHash       string
	ItemHash       string
	ArgumentsBytes int
	ArgumentsType  string
	TransportShape string
}

type CatalogDiagnostic struct {
	Count int
	Hash  string
	Names []string
}

func DiagnosticHash(value string) string {
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func diagnosticLabel(value string) string {
	if value == "" {
		return ""
	}
	if len(value) <= 128 {
		valid := true
		for _, c := range value {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-') {
				valid = false
				break
			}
		}
		if valid {
			return value
		}
	}
	return "sha256:" + DiagnosticHash(value)
}

func describeTool(item object) ToolDiagnostic {
	d := ToolDiagnostic{Type: diagnosticLabel(text(item["type"])), Name: diagnosticLabel(text(item["name"])), ArgumentsBytes: -1,
		CallHash: DiagnosticHash(text(item["call_id"])), ItemHash: DiagnosticHash(text(item["id"]))}
	value := item["arguments"]
	if text(item["type"]) == "custom_tool_call" {
		value = item["input"]
	}
	if raw, ok := value.(string); ok {
		d.ArgumentsBytes = len(raw)
		d.ArgumentsType = "string"
	} else if _, ok := value.(object); ok {
		d.ArgumentsType = "object"
	} else if value == nil {
		d.ArgumentsType = "missing_or_null"
	} else {
		d.ArgumentsType = "other"
	}
	if name := text(item["name"]); name == "run_officejs" || name == "functions.run_officejs" {
		args, ok := value.(object)
		if !ok {
			_ = decode([]byte(text(value)), &args)
		}
		d.TransportShape = transportShape(args["code"])
		envelope, marked, err := customTransportEnvelope(args)
		if !marked && err == nil {
			envelope, err = decodeTransportEnvelope(args["code"])
		}
		if err == nil {
			name, _ := envelopeName(envelope)
			d.ClientName = diagnosticLabel(name)
		}
	}
	return d
}

// CatalogDiagnostic fingerprints the complete merged declarations, including
// descriptions that can advertise exec helpers. Only names and hashes are logged.
func (b *Bridge) CatalogDiagnostic() CatalogDiagnostic {
	keys := make([]string, 0, len(b.tools))
	for key := range b.tools {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var signature strings.Builder
	signature.WriteString("bps-tool-catalog-v1\x00")
	d := CatalogDiagnostic{Count: len(keys)}
	for _, key := range keys {
		entry := b.tools[key]
		signature.WriteString(key + "\x00" + entry.Kind + "\x00" + entry.Definition + "\x00" + entry.DeclarationHash + "\x00")
		if len(d.Names) < 64 {
			d.Names = append(d.Names, diagnosticLabel(key))
		}
	}
	d.Hash = DiagnosticHash(signature.String())
	return d
}

func (b *Bridge) reportProtocolError(err error, d ProtocolDiagnostic, pending map[string]string) {
	if b.OnProtocolError == nil {
		return
	}
	d.Reason = "unclassified protocol validation failure"
	var safe validationError
	if errors.As(err, &safe) {
		d.Reason = safe.reason
	}
	var toolErr toolResponseError
	if errors.As(err, &toolErr) {
		d.Tool, d.OutputIndex = toolErr.tool, toolErr.index
	}
	d.PendingTools = len(pending)
	for key, name := range pending {
		d.MissingTools = append(d.MissingTools, name+":"+DiagnosticHash(key))
	}
	sort.Strings(d.MissingTools)
	if len(d.MissingTools) > 8 {
		d.MissingTools = d.MissingTools[:8]
	}
	b.OnProtocolError(d)
}

func responseOutput(response object) []any {
	output, _ := response["output"].([]any)
	return output
}
