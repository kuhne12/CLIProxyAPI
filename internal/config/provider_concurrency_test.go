package config

import "testing"

func TestProviderConcurrencyV8(t *testing.T) {
	data := []byte("config-version: 8\nrouting:\n  provider-concurrency: {bps: 7, codex: 13}\n")
	if err := ValidateV8Config(data); err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfigBytes(data)
	if err != nil || cfg.Routing.ProviderConcurrency != (ProviderConcurrencyLimits{BPS: 7, Codex: 13}) {
		t.Fatalf("provider limits were not read: %v", err)
	}
	for _, v := range []string{"-1", "18446744073709551616", "1.5", "invalid"} {
		if _, err := ParseConfigBytes([]byte("config-version: 8\nrouting:\n  provider-concurrency: {codex: " + v + "}\n")); err == nil {
			t.Errorf("accepted invalid limit %s", v)
		}
	}
}
