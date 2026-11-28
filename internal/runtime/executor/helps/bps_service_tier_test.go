package helps

import (
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"gopkg.in/yaml.v3"
)

func bpsTierPolicyFixture(t *testing.T, text string) *config.Config {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(text), &node); err != nil {
		t.Fatal(err)
	}
	return &config.Config{Plugins: config.PluginsConfig{Configs: map[string]config.PluginInstanceConfig{"bps": {Raw: node}}}}
}

func TestApplyBPSServiceTierDoc(t *testing.T) {
	for _, tc := range []struct {
		name     string
		policy   string
		provider string
		want     string
	}{
		{"global", "force-ultrafast: true", "bps", "ultrafast"},
		{"off", "force-ultrafast: false", "bps", "priority"},
		{"selected_id", "force-ultrafast-auth-ids: [credential-id]", "bps", "ultrafast"},
		{"selected_filename", "force-ultrafast-auth-ids: [credential.json]", "bps", "ultrafast"},
		{"unselected", "force-ultrafast-auth-ids: [other.json]", "bps", "priority"},
		{"substring", "force-ultrafast-auth-ids: [credential]", "bps", "priority"},
		{"case_sensitive", "force-ultrafast-auth-ids: [CREDENTIAL-ID]", "bps", "priority"},
		{"no_account_claim_matching", "force-ultrafast-auth-ids: [account-id]", "bps", "priority"},
		{"empty_list", "force-ultrafast-auth-ids: []", "bps", "priority"},
		{"null_policy", "null", "bps", "priority"},
		{"null_values", "force-ultrafast: null\nforce-ultrafast-auth-ids: null", "bps", "priority"},
		{"invalid_bool", "force-ultrafast: [true]", "bps", "priority"},
		{"quoted_bool", "force-ultrafast: 'true'", "bps", "priority"},
		{"numeric_id", "force-ultrafast: true\nforce-ultrafast-auth-ids: [123]", "bps", "priority"},
		{"null_id", "force-ultrafast: true\nforce-ultrafast-auth-ids: [null]", "bps", "priority"},
		{"duplicate_key", "force-ultrafast: false\nforce-ultrafast: true", "bps", "priority"},
		{"invalid_ids_shape", "force-ultrafast: true\nforce-ultrafast-auth-ids: {id: true}", "bps", "priority"},
		{"duplicate_ids", "force-ultrafast: true\nforce-ultrafast-auth-ids: [id, id]", "bps", "priority"},
		{"empty_id", "force-ultrafast: true\nforce-ultrafast-auth-ids: ['']", "bps", "priority"},
		{"non_bps_provider", "force-ultrafast: true", "codex", "priority"},
		{"missing_provider", "force-ultrafast: true", "", "priority"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := ojson.NewDocString(`{"service_tier":"priority","reasoning":{"effort":"high"},"reasoning_effort":"low","metadata":{"keep":true}}`)
			auth := &coreauth.Auth{ID: "credential-id", FileName: "credential.json", Provider: tc.provider, Metadata: map[string]any{"account_id": "account-id"}}
			ApplyBPSServiceTierDoc(doc, bpsTierPolicyFixture(t, tc.policy), auth)
			if got := doc.Get("service_tier").String(); got != tc.want {
				t.Fatalf("service_tier=%q, want %q", got, tc.want)
			}
			if doc.Get("reasoning.effort").String() != "high" || doc.Get("reasoning_effort").String() != "low" || !doc.Get("metadata.keep").Bool() {
				t.Fatalf("service tier changed unrelated fields: %s", doc.Bytes())
			}
		})
	}
}

func TestApplyBPSServiceTierDocNilAndMissingConfiguration(t *testing.T) {
	auth := &coreauth.Auth{ID: "id", Provider: "bps"}
	global := bpsTierPolicyFixture(t, "force-ultrafast: true")
	ApplyBPSServiceTierDoc(nil, global, auth)
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		auth *coreauth.Auth
	}{
		{"nil_config", nil, auth},
		{"missing_plugin", &config.Config{}, auth},
		{"zero_raw_node", &config.Config{Plugins: config.PluginsConfig{Configs: map[string]config.PluginInstanceConfig{"bps": {}}}}, auth},
		{"nil_auth", global, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := ojson.NewDocString(`{"model":"gpt-6-astra"}`)
			ApplyBPSServiceTierDoc(doc, tc.cfg, tc.auth)
			if doc.Get("service_tier").Exists() {
				t.Fatal("inactive configuration inserted a service tier")
			}
		})
	}
}
