package helps

import (
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// BPSCacheCreationAsInput snapshots the opt-in downstream accounting policy.
// Executor configuration is immutable for the lifetime of an in-flight request.
func BPSCacheCreationAsInput(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	plugin, exists := cfg.Plugins.Configs["bps"]
	if !exists || plugin.Raw.Kind == 0 {
		return false
	}
	var settings struct {
		CacheCreationAsInput bool `yaml:"cache-creation-as-input"`
	}
	return plugin.Raw.Decode(&settings) == nil && settings.CacheCreationAsInput
}

// BPSDownstreamUsage reclassifies cache creation without inflating total input.
// Call after publishing the original upstream usage, before protocol translation.
// Cache reads, totals, output, and unrelated metadata remain unchanged.
func BPSDownstreamUsage(raw []byte) []byte {
	if !ojson.ValidBytes(raw) {
		return raw
	}
	doc := ojson.NewDoc(raw)
	changed := false
	for _, root := range []string{"usage", "response.usage"} {
		usage := doc.Get(root)
		if !usage.IsObject() || (usage.Get("input_tokens").Type != ojson.Number && usage.Get("prompt_tokens").Type != ojson.Number) {
			continue
		}
		for _, field := range []string{
			"input_tokens_details.cache_write_tokens", "input_tokens_details.cache_creation_tokens", "input_tokens_details.cached_creation_tokens",
			"prompt_tokens_details.cache_write_tokens", "prompt_tokens_details.cache_creation_tokens", "prompt_tokens_details.cached_creation_tokens",
			"cache_write_tokens", "cache_creation_input_tokens", "cache_write_input_tokens", "cache_creation_tokens", "cached_creation_tokens",
			"cache_creation_5m_tokens", "cache_creation_30m_tokens", "cache_creation_1h_tokens", "cache_creation_12h_tokens",
			"cache_creation.ephemeral_5m_input_tokens", "cache_creation.ephemeral_30m_input_tokens",
			"cache_creation.ephemeral_1h_input_tokens", "cache_creation.ephemeral_12h_input_tokens",
		} {
			if value := usage.Get(field); !value.Exists() || value.Raw == "0" {
				continue
			}
			if errSet := doc.Set(root+"."+field, 0); errSet != nil {
				return raw
			}
			changed = true
		}
		for _, field := range []string{
			"input_tokens_details.cache_write_tokens_details", "prompt_tokens_details.cache_write_tokens_details",
			"input_tokens_details.cache_creation_tokens_details", "prompt_tokens_details.cache_creation_tokens_details",
		} {
			if !usage.Get(field).Exists() {
				continue
			}
			if errDelete := doc.Delete(root + "." + field); errDelete != nil {
				return raw
			}
			changed = true
		}
	}
	if !changed {
		return raw
	}
	return doc.Bytes()
}
