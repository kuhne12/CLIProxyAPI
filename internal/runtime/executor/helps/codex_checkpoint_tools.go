package helps

import (
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// RestoreCodexCheckpointTools re-registers client tools in the active context.
// A checkpoint produced by BPS can contain its relay protocol instead of native
// Codex tool declarations. Keep the historical declarations in place for calls
// before the checkpoint, and let subsequent client updates take precedence.
func RestoreCodexCheckpointTools(doc *ojson.Doc) {
	if doc == nil || doc.Get("previous_response_id").String() != "" || doc.Get("type").String() == "response.append" || doc.Get("tool_choice").String() == "none" {
		return
	}
	input := doc.Get("input")
	if !input.IsArray() || !strings.Contains(input.Raw, `"compaction"`) || !strings.Contains(input.Raw, `"additional_tools"`) {
		return
	}
	items := input.Array()
	checkpoint := -1
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if item.Get("type").String() != "compaction" {
			continue
		}
		state, status := item.Get("encrypted_content"), item.Get("status")
		if state.Type == ojson.String && state.Str != "" && (!status.Exists() || status.String() == "completed") {
			checkpoint = i
			break
		}
	}
	if checkpoint < 1 {
		return
	}

	var batches []string
	for _, item := range items[:checkpoint] {
		if batch := checkpointToolBatch(item); batch != "" {
			batches = append(batches, batch)
		}
	}
	// The client may already have restored a prefix of these updates.
	insertAt := checkpoint + 1
	for len(batches) > 0 && insertAt < len(items) {
		if checkpointToolBatch(items[insertAt]) != batches[0] {
			break
		}
		insertAt++
		batches = batches[1:]
	}
	if len(batches) == 0 {
		return
	}
	restored := make([]string, 0, len(items)+len(batches))
	for i, item := range items {
		restored = append(restored, item.Raw)
		if i+1 == insertAt {
			for _, batch := range batches {
				// Item IDs belong to the historical update and cannot be reused.
				update := ojson.NewDocString(`{"type":"additional_tools","role":"developer"}`)
				_ = update.SetRaw("tools", batch)
				restored = append(restored, update.String())
			}
		}
	}
	_ = doc.SetRawBytes("input", JoinRawJSONStrings(restored))
}

// additional_tools is a typed client control item. Some Responses clients omit
// its implicit developer role. Never promote an explicitly different role.
func checkpointToolBatch(item ojson.Result) string {
	if item.Get("type").String() != "additional_tools" {
		return ""
	}
	role, tools := item.Get("role"), item.Get("tools")
	if (role.Exists() && role.String() != "developer") || !tools.IsArray() {
		return ""
	}
	return tools.Raw
}
