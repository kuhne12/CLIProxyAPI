package helps

import (
	"bytes"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

func TestBPSCacheCreationAsInputConfig(t *testing.T) {
	if BPSCacheCreationAsInput(nil) || BPSCacheCreationAsInput(&config.Config{}) {
		t.Fatal("normalization must be opt-in")
	}
	for _, tc := range []struct {
		raw     string
		enabled bool
	}{
		{"enabled: true\n", false},
		{"enabled: true\ncache-creation-as-input: true\n", true},
		{"enabled: true\ncache-creation-as-input: false\n", false},
		{"enabled: true\ncache-creation-as-input: invalid\n", false},
	} {
		var item config.PluginInstanceConfig
		if err := yaml.Unmarshal([]byte(tc.raw), &item); err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{Plugins: config.PluginsConfig{Configs: map[string]config.PluginInstanceConfig{"bps": item}}}
		if got := BPSCacheCreationAsInput(cfg); got != tc.enabled {
			t.Fatalf("got %t for %q", got, tc.raw)
		}
	}
}

func TestBPSDownstreamUsage(t *testing.T) {
	for _, root := range []string{"usage", "response.usage"} {
		for _, details := range []string{"input_tokens_details", "prompt_tokens_details"} {
			t.Run(root+"/"+details, func(t *testing.T) {
				doc := ojson.NewDocString(`{"extension":9007199254740993,"text":"cache_write_tokens"}`)
				_ = doc.Set(root+".input_tokens", 1000)
				_ = doc.Set(root+".output_tokens", 50)
				_ = doc.Set(root+".total_tokens", 1050)
				_ = doc.Set(root+".output_tokens_details.reasoning_tokens", 20)
				_ = doc.Set(root+"."+details+".cached_tokens", 100)
				_ = doc.Set(root+"."+details+".cache_write_tokens_details.ttl_30m_tokens", 50)
				_ = doc.Set(root+"."+details+".cache_write_tokens_details.ttl_12h_tokens", 150)
				fields := []string{details + ".cache_write_tokens", details + ".cache_creation_tokens", details + ".cached_creation_tokens", "cache_write_tokens", "cache_creation_input_tokens", "cache_write_input_tokens", "cache_creation_tokens", "cached_creation_tokens", "cache_creation_5m_tokens", "cache_creation_30m_tokens", "cache_creation_1h_tokens", "cache_creation_12h_tokens", "cache_creation.ephemeral_5m_input_tokens", "cache_creation.ephemeral_30m_input_tokens", "cache_creation.ephemeral_1h_input_tokens", "cache_creation.ephemeral_12h_input_tokens"}
				for _, field := range fields {
					_ = doc.Set(root+"."+field, 200)
				}
				raw := doc.Bytes()
				before := bytes.Clone(raw)
				got := BPSDownstreamUsage(raw)
				if !bytes.Equal(raw, before) {
					t.Fatal("original upstream bytes mutated")
				}
				for _, field := range fields {
					if value := ojson.GetBytes(got, root+"."+field); value.Type != ojson.Number || value.Int() != 0 {
						t.Fatalf("write alias not cleared: %s", field)
					}
				}
				for _, field := range []string{"extension", "text", root + ".input_tokens", root + ".output_tokens", root + ".total_tokens", root + ".output_tokens_details.reasoning_tokens", root + "." + details + ".cached_tokens"} {
					if ojson.GetBytes(got, field).Raw != ojson.GetBytes(before, field).Raw {
						t.Fatalf("unrelated field changed: %s", field)
					}
				}
				if ojson.GetBytes(got, root+"."+details+".cache_write_tokens_details").Exists() {
					t.Fatal("stale cache-write TTL details")
				}
				if !bytes.Equal(got, BPSDownstreamUsage(got)) {
					t.Fatal("normalization is not idempotent")
				}
			})
		}
	}
}

func TestBPSDownstreamUsageNoop(t *testing.T) {
	for _, raw := range []string{`not-json`, `{"type":"response.output_text.delta","delta":"cache_write_tokens"}`, `{"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":5}}}`, `{"usage":{"cache_write_tokens":10}}`} {
		if got := BPSDownstreamUsage([]byte(raw)); string(got) != raw {
			t.Fatalf("unexpected mutation: %s", got)
		}
	}
}
