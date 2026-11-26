package main

import (
	"encoding/hex"
	"errors"
	"math"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestQuotaWindowMigratesVersionOneRoundedSampleAndBucketOrder(t *testing.T) {
	e := quotaTestEstimator(t)
	e.statePath = filepath.Join(t.TempDir(), "quota.json")
	now := time.Unix(1800000000, 0).UTC()
	q := quotaTestResponse(.52, now)
	reset, err := time.Parse(time.RFC3339, q.Groups[0].Buckets[0].ResetTime)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := quotaEstimateKey("test-index", "")
	state := quotaCalibrationFile{Version: 1, ConfigHash: e.priceFingerprint(), Accounts: []quotaCalibrationAccount{{
		Key: hex.EncodeToString(key[:]), Plan: q.Subscription.Plan, Touched: now,
		Windows: map[string]quotaCalibration{"0:7d": {Reset: reset, Started: now.Add(-time.Hour), LastUsed: 48, TotalUSD: 234.3600000000006, SampleCost: 2.3436, SamplePoints: .9999999999999973}},
	}}}
	if err := writeQuotaCalibrations(e.statePath, state); err != nil {
		t.Fatal(err)
	}
	restored := newPersistentBPSQuotaEstimator(e.statePath)
	if err := restored.configure(quotaConfigRequest(t, quotaTestPrices), true); err != nil {
		t.Fatal(err)
	}
	weekly := q.Groups[0].Buckets[0]
	short := weekly
	short.Window = "5h"
	q.Groups[0].Buckets = append(q.Groups[0].Buckets, short)
	q.Groups[0].Buckets[0], q.Groups[0].Buckets[1] = short, weekly
	restored.annotate(quotaFetchRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{AuthIndex: "test-index"}}, &q, now)
	if len(q.Summary) != 2 || q.Summary[1].Label != "Estimated total (7d)" || math.Abs(q.Summary[1].Value-234.36) > 1e-8 {
		t.Fatalf("version 1 migration or bucket reorder lost the existing estimate: %+v", q.Summary)
	}
}

func TestQuotaWindowRestoresRoundedOnePointSample(t *testing.T) {
	e := quotaTestEstimator(t)
	e.statePath = filepath.Join(t.TempDir(), "quota.json")
	now := time.Unix(1800000000, 0).UTC()
	quotaTestAnnotate(e, .53, now)
	e.observe(quotaTestRecord(), now)
	before := quotaTestAnnotate(e, .52, now)
	restored := newPersistentBPSQuotaEstimator(e.statePath)
	if err := restored.configure(quotaConfigRequest(t, quotaTestPrices), true); err != nil {
		t.Fatal(err)
	}
	after := quotaTestAnnotate(restored, .52, now)
	if len(after.Summary) != 2 || math.Abs(after.Summary[1].Value-before.Summary[1].Value) > 1e-8 {
		t.Fatalf("lost rounded 1-point calibration: before=%+v after=%+v", before.Summary, after.Summary)
	}
}

func TestQuotaWindowWeightsDisjointSamplesWithoutDoubleCounting(t *testing.T) {
	e := quotaTestEstimator(t)
	now := time.Unix(1800000000, 0).UTC()
	quotaTestAnnotate(e, .8, now)
	for range 4 {
		e.observe(quotaTestRecord(), now)
	}
	quotaTestAnnotate(e, .76, now)
	failed := quotaTestRecord()
	failed.Failed = true
	e.observe(failed, now)
	quotaTestAnnotate(e, .73, now)
	small := quotaTestRecord()
	small.Detail.InputTokens /= 10
	small.Detail.OutputTokens /= 10
	small.Detail.CacheReadTokens /= 10
	small.Detail.CacheCreationTokens /= 10
	e.observe(small, now)
	want := (4*1.46 + .146) * 100 / 5
	for range 5 {
		q := quotaTestAnnotate(e, .72, now)
		if len(q.Summary) != 2 || math.Abs(q.Summary[1].Value-want) > 1e-8 {
			t.Fatalf("short sample replaced history or repeated reads counted twice: %+v want %v", q.Summary, want)
		}
	}
}

func TestQuotaWindowPersistsTrafficBeforeFirstClick(t *testing.T) {
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
	key, _ := quotaEstimateKey(r.AuthIndex, r.AuthID)
	if a := restored.accounts[key]; a == nil || math.Abs(a.Cost-1.46) > 1e-8 {
		t.Fatalf("traffic was lost or counted twice on restart: %+v", a)
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
	restored.invalidateSnapshot(req.AuthIndex, req.AuthID)
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
