package bpsbridge

import (
	"github.com/MoeForge/moego/json"
	"testing"
)

func TestDiscoveredEffortsArePreservedOnWire(t *testing.T) {
	for _, level := range []string{"none", "low", "max", "ultra"} {
		raw, _ := json.Marshal(object{"model": "future-bps-model", "input": "ping", "reasoning": object{"effort": level}})
		prepared, bridge, err := PrepareWithOptions(raw, "test", nil, PrepareOptions{EffortLevels: []string{"none", "low", "max", "ultra"}})
		var payload object
		if err != nil || decode(prepared, &payload) != nil || payload["reasoning_effort"] != level || bridge.Effort != level {
			t.Fatalf("catalog effort %s was remapped: %v", level, err)
		}
	}
	raw, _ := json.Marshal(object{"model": "gpt-6.2-sol", "input": "ping", "reasoning": object{"effort": "max"}})
	if _, _, err := PrepareWithOptions(raw, "test", nil, PrepareOptions{EffortLevels: []string{"low", "medium"}}); err == nil {
		t.Fatal("unadvertised effort accepted")
	}
}
