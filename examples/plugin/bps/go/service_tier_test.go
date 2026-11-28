package main

import (
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func bpsServiceTierConfigRequest(t *testing.T, value string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"config_yaml": []byte(value)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestServiceTierPluginConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		yaml  string
		valid bool
	}{
		{"empty", "", true},
		{"null", "null", true},
		{"off", "force-ultrafast: false\nforce-ultrafast-auth-ids: []", true},
		{"global", "force-ultrafast: true", true},
		{"selected", "force-ultrafast-auth-ids: [credential-id, credential.json]", true},
		{"null_values", "force-ultrafast: null\nforce-ultrafast-auth-ids: null", true},
		{"invalid_yaml", "force-ultrafast: [", false},
		{"invalid_boolean", "force-ultrafast: wrong", false},
		{"quoted_boolean", "force-ultrafast: 'true'", false},
		{"numeric_boolean", "force-ultrafast: 1", false},
		{"non_object_config", "[true]", false},
		{"invalid_ids_scalar", "force-ultrafast-auth-ids: credential-id", false},
		{"invalid_ids_map", "force-ultrafast-auth-ids: {id: true}", false},
		{"invalid_ids_item", "force-ultrafast-auth-ids: [{id: true}]", false},
		{"empty_id", "force-ultrafast-auth-ids: ['']", false},
		{"null_id", "force-ultrafast-auth-ids: [null]", false},
		{"numeric_id", "force-ultrafast-auth-ids: [123]", false},
		{"boolean_id", "force-ultrafast-auth-ids: [true]", false},
		{"quoted_numeric_id", "force-ultrafast-auth-ids: ['123']", true},
		{"duplicate_ids", "force-ultrafast-auth-ids: [id, id]", false},
		{"duplicate_global_key", "force-ultrafast: false\nforce-ultrafast: true", false},
		{"duplicate_selection_key", "force-ultrafast-auth-ids: [a]\nforce-ultrafast-auth-ids: [b]", false},
		{"whitespace_id", "force-ultrafast-auth-ids: [' id']", false},
		{"embedded_newline", `force-ultrafast-auth-ids: ["id\nname"]`, false},
		{"oversized_id", "force-ultrafast-auth-ids: [" + strings.Repeat("x", 1025) + "]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := bpsServiceTierConfigRequest(t, tc.yaml)
			if err := validateBPSSettings(raw); (err == nil) != tc.valid {
				t.Fatalf("valid=%t, error=%v", tc.valid, err)
			}
			if !tc.valid {
				for _, method := range []string{pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure} {
					if _, err := handleMethod(method, raw); err == nil {
						t.Fatalf("%s accepted invalid service tier configuration", method)
					}
				}
			}
		})
	}
	for _, raw := range [][]byte{[]byte("not-json"), []byte(`{"config_yaml":{}}`), []byte(`{"config_yaml":"not base64!"}`)} {
		if err := validateBPSSettings(raw); err == nil {
			t.Fatalf("accepted invalid request: %s", raw)
		}
	}
}

func TestServiceTierPluginRegistrationDefaults(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodPluginRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var registration struct {
		Metadata pluginapi.Metadata `json:"metadata"`
	}
	if err := json.Unmarshal(envelope.Result, &registration); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || registration.Metadata.Version != "0.5.10" {
		t.Fatalf("unexpected plugin registration: %s", raw)
	}
	fields := make(map[string]pluginapi.ConfigField)
	for _, field := range registration.Metadata.ConfigFields {
		fields[field.Name] = field
	}
	global, ok := fields["force-ultrafast"]
	if !ok || global.Type != pluginapi.ConfigFieldTypeBoolean || global.Default != false {
		t.Fatalf("global switch must default to false: %s", raw)
	}
	selected, ok := fields["force-ultrafast-auth-ids"]
	ids, arrayOK := selected.Default.([]any)
	if !ok || selected.Type != pluginapi.ConfigFieldTypeArray || !arrayOK || len(ids) != 0 {
		t.Fatalf("credential selection must default to an empty array: %s", raw)
	}
}
