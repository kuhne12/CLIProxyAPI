package helps

import (
	"crypto/sha256"
	"io"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/bpsbridge"
)

// Standalone compaction windows may contain retained items. Only an inline
// checkpoint emitted by BPS can cover the input of the request that produced it.
type bpsCheckpointKey struct {
	scope string
	id    [sha256.Size]byte
	state [sha256.Size]byte
}

type bpsCoveredInput struct {
	count  int
	digest [sha256.Size]byte
}

var bpsCheckpoints = cache.NewBoundedLRU[bpsCheckpointKey, bpsCoveredInput](1024, nil)

func bpsInputPrefix(items []ojson.Result) bpsCoveredInput {
	h := sha256.New()
	for _, item := range items {
		_, _ = io.WriteString(h, item.Raw)
		_, _ = h.Write([]byte{0}) // A raw JSON value cannot contain a literal NUL.
	}
	prefix := bpsCoveredInput{count: len(items)}
	h.Sum(prefix.digest[:0])
	return prefix
}

// PrepareBPSHistory retains explicit user instructions and adapter metadata.
// The callback records only fingerprints, after a successful inline compaction.
func PrepareBPSHistory(doc *ojson.Doc, scope string, options *bpsbridge.PrepareOptions) (func(string, string), error) {
	input := doc.Get("input")
	if !input.IsArray() {
		return nil, nil
	}
	remember := func(id, encrypted string) {
		key := bpsCheckpointKey{scope, sha256.Sum256([]byte(id)), sha256.Sum256([]byte(encrypted))}
		prefix := bpsInputPrefix(input.Array())
		bpsCheckpoints.GetOrAdd(key, func() bpsCoveredInput { return prefix })
	}
	if bpsCheckpoints.Len() == 0 || !strings.Contains(input.Raw, "compaction") {
		return remember, nil
	}
	items := input.Array()
	for i := len(items) - 1; i > 0; i-- {
		item := items[i]
		if item.Get("type").String() != "compaction" {
			continue
		}
		id, state := item.Get("id"), item.Get("encrypted_content")
		status := item.Get("status")
		if id.Type != ojson.String || id.Str == "" || state.Type != ojson.String || state.Str == "" || (status.Exists() && status.String() != "completed") {
			break
		}
		key := bpsCheckpointKey{scope, sha256.Sum256([]byte(id.Str)), sha256.Sum256([]byte(state.Str))}
		prefix, known := bpsCheckpoints.Get(key)
		if known && prefix.count > 0 && prefix.count <= i && bpsInputPrefix(items[:prefix.count]) == prefix {
			return remember, trimBPSCoveredInput(doc, items, prefix.count, options)
		}
		break
	}
	return remember, nil
}

func trimBPSCoveredInput(doc *ojson.Doc, items []ojson.Result, covered int, options *bpsbridge.PrepareOptions) error {
	// A result following the covered prefix can still need its original call,
	// including when the tool replay cache has already evicted the native item.
	resultIDs := make(map[string]bool)
	for _, item := range items[covered:] {
		switch item.Get("type").String() {
		case "function_call_output", "custom_tool_call_output":
			resultIDs[item.Get("call_id").String()] = true
		}
	}
	kept := make([]string, 0, len(items))
	for i, item := range items {
		if i < covered {
			switch item.Get("type").String() {
			case "compaction", "compaction_trigger", "reasoning", "web_search_call", "function_call_output", "custom_tool_call_output":
				continue
			case "function_call", "custom_tool_call":
				if !resultIDs[item.Get("call_id").String()] {
					continue
				}
			case "", "message":
				if item.Get("role").String() == "assistant" {
					continue
				}
			}
		}
		kept = append(kept, item.Raw)
	}
	if len(kept) == len(items) {
		return nil
	}
	if err := doc.SetRawBytes("input", JoinRawJSONStrings(kept)); err != nil {
		return err
	}
	// The upstream iteration counts all tool results since the last user turn.
	iteration := 1
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Get("role").String() == "user" {
			break
		}
		if strings.HasSuffix(items[i].Get("type").String(), "_call_output") {
			iteration++
		}
	}
	options.AgentIteration = iteration
	return nil
}
