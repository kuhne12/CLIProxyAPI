package main

import (
	"crypto/sha256"
	"fmt"
	"math"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
)

func TestQuotaUltraFastRequiresExplicitMultiplier(t *testing.T) {
	e := quotaTestEstimator(t)
	r := quotaTestRecord()
	r.EffectiveServiceTier = "ultrafast"
	if _, ok := quotaUsageCost(r, e.config); ok {
		t.Fatal("unpriced ultrafast tier was guessed")
	}
	cfg := quotaTestPrices + "    ultrafast-multiplier: 3\n"
	if err := e.configure(quotaConfigRequest(t, cfg), false); err != nil {
		t.Fatal(err)
	}
	cost, ok := quotaUsageCost(r, e.config)
	if !ok || math.Abs(cost-4.38) > 1e-8 {
		t.Fatalf("cost=%v valid=%v", cost, ok)
	}
	r.ResponseServiceTier = "default"
	if cost, ok = quotaUsageCost(r, e.config); !ok || math.Abs(cost-1.46) > 1e-8 {
		t.Fatal("reported upstream tier did not take precedence")
	}
	for _, invalid := range []string{"-1", ".nan", ".inf", "1000000001"} {
		if err := e.configure(quotaConfigRequest(t, quotaTestPrices+"    ultrafast-multiplier: "+invalid+"\n"), false); err == nil {
			t.Fatalf("invalid multiplier accepted: %s", invalid)
		}
	}
}

func TestQuotaUltraFastPreservesLegacyPriceFingerprintWhenUnset(t *testing.T) {
	e := quotaTestEstimator(t)
	raw, err := json.Marshal(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if ojson.GetBytes(raw, "Prices.gpt-6-astra.UltraFast").Exists() {
		t.Fatal("new absent field invalidates existing quota samples")
	}
	legacy := []byte(`{"CacheCreationAsInput":false,"Prices":{"gpt-6-astra":{"Input":10,"Output":50,"CacheRead":1,"CacheWrite":12.5,"Priority":2,"Flex":null,"LongContext":272000,"LongInput":2,"LongOutput":1.5}}}`)
	want := fmt.Sprintf("%x", sha256.Sum256(legacy))
	if got := e.priceFingerprint(); got != want {
		t.Fatalf("legacy price fingerprint changed: %s != %s", got, want)
	}
	if err := e.configure(quotaConfigRequest(t, quotaTestPrices+"force-ultrafast: true\nforce-ultrafast-auth-ids: [bps-one.json]\n"), false); err != nil {
		t.Fatal(err)
	}
	if got := e.priceFingerprint(); got != want {
		t.Fatal("tier selection alone invalidated quota samples")
	}
}
