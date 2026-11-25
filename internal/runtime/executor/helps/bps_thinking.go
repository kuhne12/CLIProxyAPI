package helps

import (
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// ApplyBPSRequestThinkingDoc uses the selected account's catalog, including when
// its native model is only published under an alias or credential prefix.
func ApplyBPSRequestThinkingDoc(doc *ojson.Doc, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, from string, model *registry.ModelInfo, updatesChanged bool) error {
	to := sdktranslator.FormatCodex.String()
	if model == nil {
		return ApplyRequestThinkingDoc(doc, req, opts, from, to, "bps", updatesChanged)
	}
	original := opts.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	source := req.Payload
	if len(source) == 0 {
		source = original
	}
	summary := translatedRequestSummaryConfigDoc(sdktranslator.Default(), doc, req.Payload, original, req.Model, from, to)
	return thinking.ApplyThinkingWithModelInfoAndSummaryDoc(doc, source, req.Model, from, to, "bps", model, summary, updatesChanged)
}
