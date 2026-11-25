package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const quotaTestPrices = `quota-model-prices:
  gpt-6-astra:
    input: 10
    output: 50
    cache-read: 1
    cache-write: 12.5
    priority-multiplier: 2
    long-context-threshold: 272000
    long-context-input-multiplier: 2
    long-context-output-multiplier: 1.5
`

func quotaConfigRequest(t *testing.T, config string) []byte {
	t.Helper()
	raw, err := json.Marshal(struct {
		ConfigYAML []byte `json:"config_yaml"`
	}{[]byte(config)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func quotaTestEstimator(t *testing.T) *bpsQuotaEstimator {
	t.Helper()
	e := newBPSQuotaEstimator()
	if err := e.configure(quotaConfigRequest(t, quotaTestPrices), false); err != nil {
		t.Fatal(err)
	}
	return e
}

func quotaTestRecord() pluginapi.UsageRecord {
	return pluginapi.UsageRecord{Provider: "bps", AuthIndex: "test-index", Model: "gpt-6-astra", Generate: true,
		Detail: pluginapi.UsageDetail{InputTokens: 100000, OutputTokens: 10000, ReasoningTokens: 5000, CacheReadTokens: 10000, CacheCreationTokens: 20000}}
}

func quotaTestResponse(remaining float64, now time.Time) pluginapi.QuotaFetchResponse {
	return pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{Plan: "pro"},
		Groups: []pluginapi.QuotaGroup{{DisplayName: "Account usage", Buckets: []pluginapi.QuotaBucket{{Window: "7d", RemainingFraction: remaining, ResetTime: now.Add(7 * 24 * time.Hour).Format(time.RFC3339)}}}}}
}

func quotaTestAnnotate(e *bpsQuotaEstimator, remaining float64, now time.Time) pluginapi.QuotaFetchResponse {
	q := quotaTestResponse(remaining, now)
	e.annotate(quotaFetchRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{AuthIndex: "test-index"}}, &q, now)
	return q
}

func TestBPSQuotaPriceAccountsForCacheReasoningTierAndContext(t *testing.T) {
	e := quotaTestEstimator(t)
	base := quotaTestRecord()
	for _, tt := range []struct {
		name   string
		mutate func(*pluginapi.UsageRecord, *quotaEstimateConfig)
		want   float64
		valid  bool
	}{
		{"normal", func(*pluginapi.UsageRecord, *quotaEstimateConfig) {}, 1.46, true},
		{"cache creation as input", func(_ *pluginapi.UsageRecord, c *quotaEstimateConfig) { c.CacheCreationAsInput = true }, 1.41, true},
		{"cache fallback", func(r *pluginapi.UsageRecord, _ *quotaEstimateConfig) {
			r.Detail.CachedTokens = r.Detail.CacheReadTokens
			r.Detail.CacheReadTokens = 0
		}, 1.46, true},
		{"priority", func(r *pluginapi.UsageRecord, _ *quotaEstimateConfig) { r.ResponseServiceTier = "priority" }, 2.92, true},
		{"upstream tier wins", func(r *pluginapi.UsageRecord, _ *quotaEstimateConfig) {
			r.ServiceTier = "priority"
			r.EffectiveServiceTier = "priority"
			r.ResponseServiceTier = "default"
		}, 1.46, true},
		{"unknown tier", func(r *pluginapi.UsageRecord, _ *quotaEstimateConfig) { r.ResponseServiceTier = "flex" }, 0, false},
		{"unknown model", func(r *pluginapi.UsageRecord, _ *quotaEstimateConfig) { r.ResponseModel = "unpriced-model" }, 0, false},
		{"long context", func(r *pluginapi.UsageRecord, _ *quotaEstimateConfig) { r.Detail.InputTokens = 300000 }, 6.67, true},
		{"negative usage", func(r *pluginapi.UsageRecord, _ *quotaEstimateConfig) { r.Detail.OutputTokens = -1 }, 0, false},
		{"overlapping cache", func(r *pluginapi.UsageRecord, _ *quotaEstimateConfig) { r.Detail.CacheCreationTokens = 100000 }, 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, cfg := base, e.config
			tt.mutate(&r, &cfg)
			cost, valid := quotaUsageCost(r, cfg)
			if valid != tt.valid || math.Abs(cost-tt.want) > 1e-8 {
				t.Fatalf("cost=%v, valid=%v; want %v, %v", cost, valid, tt.want, tt.valid)
			}
		})
	}
}

func TestBPSQuotaEstimatesOnlyObservedPercentageDelta(t *testing.T) {
	e := quotaTestEstimator(t)
	now := time.Unix(1800000000, 0).UTC()
	e.observe(quotaTestRecord(), now) // Historical usage predating calibration must be excluded.
	q := quotaTestAnnotate(e, 0.8, now)
	if len(q.Summary) != 0 || !strings.Contains(q.Groups[0].Buckets[0].Description, "Collecting") {
		t.Fatalf("baseline was presented as an estimate: %+v", q)
	}
	e.observe(quotaTestRecord(), now)
	q = quotaTestAnnotate(e, 0.795, now)
	if len(q.Summary) != 0 {
		t.Fatal("fractional percentage sample bypassed minimum")
	}
	q = quotaTestAnnotate(e, 0.78, now)
	if len(q.Summary) != 2 || math.Abs(q.Summary[0].Value-56.94) > 1e-8 || math.Abs(q.Summary[1].Value-73) > 1e-8 || q.Summary[0].Currency != "USD" || q.Summary[0].Format != "currency" {
		t.Fatalf("incorrect USD estimate: %+v", q)
	}
	q = quotaTestAnnotate(e, 0.78, now)
	if len(q.Summary) != 2 {
		t.Fatal("stable quota lost its calibrated estimate")
	}
	q = quotaTestAnnotate(e, 0, now)
	if len(q.Summary) != 2 || q.Summary[0].Value != 0 {
		t.Fatal("exhausted quota did not show calibrated zero remaining")
	}
}

func TestBPSQuotaSamplesResetOnUncertainty(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	for _, name := range []string{"window", "percentage decrease", "plan", "unpriced", "failure", "price change", "restart", "missing reset"} {
		t.Run(name, func(t *testing.T) {
			e := quotaTestEstimator(t)
			quotaTestAnnotate(e, 0.8, now)
			e.observe(quotaTestRecord(), now)
			if q := quotaTestAnnotate(e, 0.78, now); len(q.Summary) != 2 {
				t.Fatal("fixture did not calibrate")
			}
			q := quotaTestResponse(0.77, now)
			switch name {
			case "window":
				q.Groups[0].Buckets[0].ResetTime = now.Add(8 * 24 * time.Hour).Format(time.RFC3339)
			case "percentage decrease":
				q.Groups[0].Buckets[0].RemainingFraction = 0.99
			case "plan":
				q.Subscription.Plan = "other-plan"
			case "unpriced":
				r := quotaTestRecord()
				r.Model = "unknown"
				e.observe(r, now)
			case "failure":
				r := quotaTestRecord()
				r.Failed = true
				e.observe(r, now)
			case "price change":
				if err := e.configure(quotaConfigRequest(t, quotaTestPrices+"cache-creation-as-input: true\n"), false); err != nil {
					t.Fatal(err)
				}
			case "restart":
				if err := e.configure(quotaConfigRequest(t, quotaTestPrices), true); err != nil {
					t.Fatal(err)
				}
			case "missing reset":
				q.Groups[0].Buckets[0].ResetTime = ""
			}
			e.annotate(quotaFetchRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{AuthIndex: "test-index"}}, &q, now)
			if len(q.Summary) != 0 {
				t.Fatalf("stale estimate survived %s: %+v", name, q.Summary)
			}
		})
	}
}

func TestBPSQuotaIsolationDedupeAndExternalUsage(t *testing.T) {
	e := quotaTestEstimator(t)
	now := time.Unix(1800000000, 0).UTC()
	quotaTestAnnotate(e, 0.8, now)
	r := quotaTestRecord()
	r.RequestID = "execution-one"
	e.observe(r, now)
	e.observe(r, now)
	r.Provider = "codex"
	e.observe(r, now)
	r.Provider = "bps"
	r.AuthIndex = "different-account"
	e.observe(r, now)
	r.AuthIndex = "test-index"
	r.RequestID = "execution-two"
	r.Generate = false
	e.observe(r, now)
	q := quotaTestAnnotate(e, 0.78, now)
	if len(q.Summary) != 2 || math.Abs(q.Summary[1].Value-73) > 1e-8 {
		t.Fatalf("cross-account usage or duplicate counting: %+v", q)
	}
	e = quotaTestEstimator(t)
	quotaTestAnnotate(e, 0.8, now)
	q = quotaTestAnnotate(e, 0.7, now)
	if len(q.Summary) != 0 {
		t.Fatal("external usage invented an estimate")
	}
	e.observe(quotaTestRecord(), now)
	q = quotaTestAnnotate(e, 0.68, now)
	if len(q.Summary) != 2 || math.Abs(q.Summary[1].Value-73) > 1e-8 {
		t.Fatal("external usage polluted new calibration")
	}
}

func TestBPSQuotaConfigurationRejectsInvalidPricesAtomically(t *testing.T) {
	e := quotaTestEstimator(t)
	for _, price := range []string{"{input: 1}", "{input: -1, output: 2}", "{input: .nan, output: 2}", "{input: .inf, output: 2}", "{input: 1, output: 2, long-context-threshold: 10}", "{input: 1, output: 2, long-context-threshold: -1}"} {
		err := e.configure(quotaConfigRequest(t, "quota-model-prices:\n  model: "+price), false)
		if err == nil || len(e.config.Prices) != 1 || e.config.Prices["gpt-6-astra"].Input == nil {
			t.Fatalf("invalid price accepted or replaced configuration: %s", price)
		}
	}
	if err := e.configure([]byte("{invalid"), false); err == nil {
		t.Fatal("invalid lifecycle request accepted")
	}
}

func TestBPSQuotaIndependentWindowsAndCredits(t *testing.T) {
	e := quotaTestEstimator(t)
	now := time.Unix(1800000000, 0).UTC()
	makeQuota := func(primary, secondary float64) pluginapi.QuotaFetchResponse {
		q := quotaTestResponse(primary, now)
		q.Groups[0].Buckets[0].Window = "5h"
		q.Groups[0].Buckets = append(q.Groups[0].Buckets, quotaTestResponse(secondary, now).Groups[0].Buckets[0])
		q.Groups = append(q.Groups, pluginapi.QuotaGroup{DisplayName: "Code review", Buckets: []pluginapi.QuotaBucket{{Window: "7d", RemainingFraction: 0.5}}})
		q.Summary = []pluginapi.QuotaMetric{{Key: "credits_balance", Value: 5, Unit: "credits"}}
		return q
	}
	req := quotaFetchRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{AuthIndex: "test-index"}}
	q := makeQuota(0.9, 0.8)
	e.annotate(req, &q, now)
	e.observe(quotaTestRecord(), now)
	q = makeQuota(0.88, 0.79)
	e.annotate(req, &q, now)
	if len(q.Summary) != 5 || q.Summary[0].Unit != "credits" || math.Abs(q.Summary[2].Value-73) > 1e-8 || math.Abs(q.Summary[4].Value-146) > 1e-8 || q.Groups[1].Buckets[0].Description != "" {
		t.Fatalf("quota groups or units conflated: %+v", q)
	}
}

func TestBPSQuotaConcurrentUsageAndQueries(t *testing.T) {
	e := quotaTestEstimator(t)
	now := time.Unix(1800000000, 0).UTC()
	quotaTestAnnotate(e, 0.8, now)
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := quotaTestRecord()
			r.RequestID = fmt.Sprintf("execution-%d", i)
			e.observe(r, now)
			quotaTestAnnotate(e, 0.8, now)
		}()
	}
	wg.Wait()
	q := quotaTestAnnotate(e, 0.78, now)
	if len(q.Summary) != 2 || math.Abs(q.Summary[1].Value-7300) > 1e-7 {
		t.Fatalf("lost concurrent samples: %+v", q)
	}
}

func TestBPSQuotaLifecycleDispatchPreservesSamplesOnUnchangedConfig(t *testing.T) {
	t.Cleanup(func() { _ = quotaEstimator.configure(nil, true) })
	raw := quotaConfigRequest(t, quotaTestPrices)
	if _, err := handleMethod(pluginabi.MethodPluginReconfigure, raw); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1800000000, 0).UTC()
	quotaTestAnnotate(quotaEstimator, 0.8, now)
	usage, errMarshal := json.Marshal(quotaTestRecord())
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if _, err := handleMethod(pluginabi.MethodUsageHandle, usage); err != nil {
		t.Fatal(err)
	}
	if _, err := handleMethod(pluginabi.MethodPluginReconfigure, raw); err != nil {
		t.Fatal(err)
	}
	if q := quotaTestAnnotate(quotaEstimator, 0.78, now); len(q.Summary) != 2 {
		t.Fatal("reconfigure lost unchanged calibration")
	}
}
