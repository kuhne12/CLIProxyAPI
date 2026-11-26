package helps

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/bpsbridge"
	log "github.com/sirupsen/logrus"
)

// Standalone compaction windows may contain retained items. Only an inline
// checkpoint emitted by BPS can cover the input of the request that produced it.
const bpsCheckpointTTL = 30 * 24 * time.Hour

type bpsCoveredInput struct {
	count  int
	digest [sha256.Size]byte
}

var bpsCheckpoints = cache.NewBoundedLRU[string, bpsCoveredInput](1024, nil)

func bpsCheckpointKey(scope, id, encrypted string) string {
	// The executor supplies the hash of the credential, caller and session.
	return fmt.Sprintf("cpa:bps:checkpoint:v1:%s:%x:%x", scope, sha256.Sum256([]byte(id)), sha256.Sum256([]byte(encrypted)))
}

func storeBPSCheckpoint(ctx context.Context, key string, prefix bpsCoveredInput) string {
	// Keep the first verified binding even when multiple instances see the same ID.
	value := binary.BigEndian.AppendUint64(make([]byte, 0, 8+sha256.Size), uint64(prefix.count))
	value = append(value, prefix.digest[:]...)
	active, inserted, err := kvstore.KVSetNXRequired(ctx, key, value, bpsCheckpointTTL)
	if err != nil {
		return "kv_error"
	}
	if active {
		if !inserted {
			return "already_stored"
		}
		return "kv"
	}
	bpsCheckpoints.GetOrAdd(key, func() bpsCoveredInput { return prefix })
	return "memory"
}

func lookupBPSCheckpoint(ctx context.Context, key string) (bpsCoveredInput, string) {
	client, active, err := kvstore.CurrentKVClient()
	if !active {
		if prefix, ok := bpsCheckpoints.Get(key); ok {
			return prefix, "memory"
		}
		return bpsCoveredInput{}, "memory_miss"
	}
	if err != nil {
		return bpsCoveredInput{}, "kv_error"
	}
	raw, found, errGet := client.KVGet(ctx, key)
	if errGet != nil {
		return bpsCoveredInput{}, "kv_error"
	}
	if !found {
		return bpsCoveredInput{}, "kv_miss"
	}
	if len(raw) != 8+sha256.Size {
		return bpsCoveredInput{}, "kv_invalid"
	}
	count := binary.BigEndian.Uint64(raw[:8])
	if count == 0 || count > math.MaxInt {
		return bpsCoveredInput{}, "kv_invalid"
	}
	return bpsCoveredInput{count: int(count), digest: [sha256.Size]byte(raw[8:])}, "kv"
}

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
func PrepareBPSHistory(ctx context.Context, doc *ojson.Doc, scope string, options *bpsbridge.PrepareOptions, observation *BPSObservation) (func(string, string), error) {
	input := doc.Get("input")
	items := input.Array()
	status, checkpoint := "no_checkpoint", ""
	covered := 0
	cacheEntries := bpsCheckpoints.Len()
	defer func() {
		after := doc.Get("input")
		observation.recordDiagnostic(log.Fields{
			"history_status": status, "history_checkpoint_id_hash": checkpoint, "history_cache_entries": cacheEntries,
			"history_items_before": len(items), "history_items_after": len(after.Array()),
			"history_bytes_before": len(input.Raw), "history_bytes_after": len(after.Raw),
			"history_covered_items": covered, "history_agent_iteration_override": options.AgentIteration,
		})
	}()
	if !input.IsArray() {
		status = "input_not_array"
		return nil, nil
	}
	remember := func(id, encrypted string) {
		// An echo of an input checkpoint proves no new coverage.
		for _, item := range items {
			if item.Get("type").String() == "compaction" && item.Get("id").String() == id && item.Get("encrypted_content").String() == encrypted {
				observation.recordDiagnostic(log.Fields{"compaction_confirmation": "replayed_checkpoint"})
				return
			}
		}
		if len(items) == 0 {
			return
		}
		store := storeBPSCheckpoint(ctx, bpsCheckpointKey(scope, id, encrypted), bpsInputPrefix(items))
		observation.recordDiagnostic(log.Fields{"compaction_store": store})
		observation.ConfirmCompaction(id)
	}
	if !strings.Contains(input.Raw, "compaction") {
		return remember, nil
	}
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if item.Get("type").String() != "compaction" {
			continue
		}
		id, state := item.Get("id"), item.Get("encrypted_content")
		checkpoint = bpsbridge.DiagnosticHash(id.String())
		if i == 0 {
			status = "checkpoint_without_prefix"
			break
		}
		status = "invalid_checkpoint"
		itemStatus := item.Get("status")
		if id.Type != ojson.String || id.Str == "" || state.Type != ojson.String || state.Str == "" || (itemStatus.Exists() && itemStatus.String() != "completed") {
			break
		}
		prefix, source := lookupBPSCheckpoint(ctx, bpsCheckpointKey(scope, id.Str, state.Str))
		observation.recordDiagnostic(log.Fields{"history_checkpoint_source": source})
		status = "checkpoint_not_found"
		if prefix.count > 0 {
			status = "prefix_mismatch"
			covered = prefix.count
		}
		if prefix.count > 0 && prefix.count <= i && bpsInputPrefix(items[:prefix.count]) == prefix {
			errTrim := trimBPSCoveredInput(doc, items, prefix.count, options)
			status = "trimmed"
			if errTrim != nil {
				status = "trim_failed"
			} else if doc.Get("input").Raw == input.Raw {
				status = "nothing_to_trim"
			}
			return remember, errTrim
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
