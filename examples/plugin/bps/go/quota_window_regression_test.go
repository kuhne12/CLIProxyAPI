package main

import (
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestQuotaWindowMigratesLegacySnapshotsWithoutDeltaEstimates(t *testing.T) {
	e := quotaTestEstimator(t)
	e.statePath = filepath.Join(t.TempDir(), "quota.json")
	now := time.Unix(1800000000, 0).UTC()
	q := quotaTestResponse(.52, now)
	q.Groups[0].Buckets[0].Description = "Old sample explanation"
	q.Summary = []pluginapi.QuotaMetric{{Key: "bps_estimated_0_total_usd", Value: 234}, {Key: "credits_balance", Value: 2}}
	key, _ := quotaEstimateKey("test-index", "")
	legacy, err := json.Marshal(map[string]any{"version": 2, "config_hash": e.priceFingerprint(), "accounts": []map[string]any{{
		"key": hex.EncodeToString(key[:]), "plan": q.Subscription.Plan, "snapshot": q,
		"windows": map[string]any{"7d": map[string]any{"last_used": 48, "total_usd": 234}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.statePath, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.configure(quotaConfigRequest(t, quotaTestPrices), true); err != nil {
		t.Fatal(err)
	}
	if a := e.accounts[key]; a == nil || len(a.Windows) != 0 || len(a.Snapshot.Summary) != 1 || a.Snapshot.Summary[0].Key != "credits_balance" || a.Snapshot.Groups[0].Buckets[0].Description != "" {
		t.Fatalf("legacy migration retained an obsolete estimate or lost reported balance: %+v", a)
	}
}

func TestQuotaWindowKeepsCostWhenHistoryIsFolded(t *testing.T) {
	e := quotaTestEstimator(t)
	now := time.Unix(1800000000, 0).UTC()
	quotaTestAnnotate(e, 1, now)
	for range quotaEstimateMaxEntries + 1 {
		e.observe(quotaTestRecord(), now)
	}
	q := quotaTestAnnotate(e, .74, now)
	want := (quotaEstimateMaxEntries + 1) * 1.46 / .26
	if len(q.Summary) != 2 || math.Abs(q.Summary[1].Value-want) > 1e-7 {
		t.Fatalf("window lost cost when bounded history filled: %+v", q.Summary)
	}
}

func TestQuotaWindowPersistsAndDeduplicatesBeforeFirstRefresh(t *testing.T) {
	e := quotaTestEstimator(t)
	e.statePath = filepath.Join(t.TempDir(), "quota.json")
	now := time.Unix(1800000000, 0).UTC()
	r := quotaTestRecord()
	r.RequestID = "synthetic-request"
	e.observe(r, now)
	restored := newPersistentBPSQuotaEstimator(e.statePath)
	if err := restored.configure(quotaConfigRequest(t, quotaTestPrices), true); err != nil {
		t.Fatal(err)
	}
	restored.observe(r, now)
	q := quotaTestAnnotate(restored, .50, now)
	if len(q.Summary) != 2 || math.Abs(q.Summary[1].Value-2.92) > 1e-8 {
		t.Fatalf("traffic was lost or counted twice on restart: %+v", q.Summary)
	}
}

func TestQuotaSnapshotDeduplicatesRetainsAndInvalidates(t *testing.T) {
	e := quotaTestEstimator(t)
	e.statePath = filepath.Join(t.TempDir(), "quota.json")
	now := time.Unix(1800000000, 0).UTC()
	req := quotaFetchRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{AuthIndex: "test-index", Provider: "bps"}}
	var calls atomic.Int32
	fetch := func() (pluginapi.QuotaFetchResponse, error) { calls.Add(1); return quotaTestResponse(.8, now), nil }
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if _, err := e.fetchSnapshot(req, fetch, now); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d", calls.Load())
	}
	fail := func() (pluginapi.QuotaFetchResponse, error) {
		calls.Add(1)
		return pluginapi.QuotaFetchResponse{}, errors.New("offline")
	}
	q, err := e.fetchSnapshot(req, fail, now.Add(time.Minute))
	if err != nil || !q.Stale || len(q.Groups) != 1 || q.ObservedAt != now.Format(time.RFC3339Nano) {
		t.Fatalf("lost last good snapshot: %+v %v", q, err)
	}
	q.Groups[0].Buckets[0].RemainingFraction = 0
	q, err = e.fetchSnapshot(req, fail, now.Add(time.Minute+time.Second))
	if err != nil || q.Groups[0].Buckets[0].RemainingFraction != .8 || calls.Load() != 2 {
		t.Fatal("cache mutated or failure retry was not throttled")
	}
	restored := newPersistentBPSQuotaEstimator(e.statePath)
	if err := restored.configure(quotaConfigRequest(t, quotaTestPrices), true); err != nil {
		t.Fatal(err)
	}
	q, err = restored.fetchSnapshot(req, fail, now.Add(2*time.Minute))
	if err != nil || !q.Stale || len(q.Groups) != 1 {
		t.Fatal("snapshot did not survive restart")
	}
	restored.invalidateSnapshot(req.AuthIndex, req.AuthID, now.Add(2*time.Minute))
	if _, err := restored.fetchSnapshot(req, fail, now.Add(2*time.Minute)); err == nil {
		t.Fatal("reset reused stale quota or credits")
	}
}

func TestQuotaSnapshotDoesNotRepopulateAfterConfigurationChange(t *testing.T) {
	e := quotaTestEstimator(t)
	now := time.Unix(1800000000, 0).UTC()
	req := quotaFetchRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{AuthIndex: "test-index", Provider: "bps"}}
	_, err := e.fetchSnapshot(req, func() (pluginapi.QuotaFetchResponse, error) {
		if err := e.configure(nil, true); err != nil {
			t.Fatal(err)
		}
		return quotaTestResponse(.8, now), nil
	}, now)
	if err == nil || len(e.accounts) != 0 {
		t.Fatal("stale refresh repopulated a changed configuration")
	}
}
