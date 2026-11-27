package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestBPSQuotaPersistenceRestartsSamplingWithoutLosingEstimate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.json")
	newEstimator := func() *bpsQuotaEstimator {
		e := newPersistentBPSQuotaEstimator(path)
		if err := e.configure(quotaConfigRequest(t, quotaTestPrices), true); err != nil {
			t.Fatal(err)
		}
		return e
	}
	now := time.Unix(1800000000, 0).UTC()
	e := newEstimator()
	quotaTestAnnotate(e, .8, now)
	e.observe(quotaTestRecord(), now)
	before := quotaTestAnnotate(e, .78, now)
	if len(before.Summary) != 2 {
		t.Fatal("missing calibration")
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if strings.Contains(string(raw), "test-index") || strings.Contains(string(raw), "access_token") {
		t.Fatal("saved sensitive identity")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("state permissions are not private")
	}
	e = newEstimator()
	// Usage while CPA was stopped must not be included in a new cost sample.
	after := quotaTestResponse(.7, now)
	req := quotaFetchRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{AuthIndex: "test-index"}}
	e.annotate(req, &after, now.Add(time.Second))
	if len(after.Summary) != 2 || math.Abs(after.Summary[1].Value-1.46/.3) > 1e-8 {
		t.Fatalf("reset-to-date cost did not survive restart: %+v", after.Summary)
	}
	if err := e.configure(quotaConfigRequest(t, quotaTestPrices+"cache-creation-as-input: true\n"), false); err != nil {
		t.Fatal(err)
	}
	if q := quotaTestAnnotate(newEstimator(), .69, now); len(q.Summary) != 0 {
		t.Fatal("price change resurrected previous estimates")
	}
}

func TestBPSQuotaPersistenceRejectsStaleAndCorruptState(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	for _, kind := range []string{"plan", "reset", "refill", "other account", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "quota.json")
			e := quotaTestEstimator(t)
			e.statePath = path
			quotaTestAnnotate(e, .8, now)
			e.observe(quotaTestRecord(), now)
			quotaTestAnnotate(e, .78, now)
			if kind == "corrupt" {
				_ = os.WriteFile(path, []byte("{broken"), 0600)
			}
			e = newPersistentBPSQuotaEstimator(path)
			if err := e.configure(quotaConfigRequest(t, quotaTestPrices), true); err != nil {
				t.Fatal(err)
			}
			q := quotaTestResponse(.77, now)
			req := quotaFetchRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{AuthIndex: "test-index"}}
			switch kind {
			case "plan":
				q.Subscription.Plan = "other-plan"
			case "reset":
				q.Groups[0].Buckets[0].ResetTime = now.Add(8 * 24 * time.Hour).Format(time.RFC3339)
			case "refill":
				q.Groups[0].Buckets[0].RemainingFraction = .9
			case "other account":
				req.AuthIndex = "other-index"
			}
			e.annotate(req, &q, now)
			if len(q.Summary) != 0 {
				t.Fatal("invalid calibration restored")
			}
		})
	}
}

func TestBPSQuotaRetainsCalibrationAcrossObservationGaps(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	for _, kind := range []string{"unpriced", "failure", "external"} {
		t.Run(kind, func(t *testing.T) {
			e := quotaTestEstimator(t)
			quotaTestAnnotate(e, .8, now)
			e.observe(quotaTestRecord(), now)
			quotaTestAnnotate(e, .78, now)
			r := quotaTestRecord()
			if kind == "unpriced" {
				r.Model = "unknown"
			}
			if kind == "failure" {
				r.Failed = true
			}
			if kind != "external" {
				e.observe(r, now)
			}
			q := quotaTestAnnotate(e, .75, now)
			if len(q.Summary) != 2 || math.Abs(q.Summary[1].Value-1.46/.25) > 1e-8 {
				t.Fatal("did not recompute from known priced usage")
			}
			e.observe(quotaTestRecord(), now)
			q = quotaTestAnnotate(e, .74, now)
			if len(q.Summary) != 2 || math.Abs(q.Summary[1].Value-2.92/.26) > 1e-8 {
				t.Fatal("lost cumulative cost for the current window")
			}
		})
	}
}

func TestBPSQuotaRecoversHistoryAndDeduplicatesLiveUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.json")
	e := quotaTestEstimator(t)
	e.statePath = path
	start := time.Unix(1800000000, 0).UTC()
	e.trackingSince = start.Add(time.Minute)
	record := quotaTestRecord()
	record.RequestID = "live"
	record.RequestedAt = start.Add(2 * time.Minute)
	e.observe(record, record.RequestedAt)
	key, _ := quotaEstimateKey(record.AuthIndex, record.AuthID)
	seed := quotaHistorySeed{
		Key: hex.EncodeToString(key[:]), ConfigHash: e.priceFingerprint(), Since: start, Through: record.RequestedAt,
		Entries: []quotaRecoveredCost{
			{quotaCostEntry{At: start, USD: 4.54}, fmt.Sprintf("%x", sha256.Sum256([]byte("historical")))},
			{quotaCostEntry{At: record.RequestedAt, USD: 1.46}, fmt.Sprintf("%x", sha256.Sum256([]byte("live")))},
		},
	}
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "bps-quota-history.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	req := quotaFetchRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{AuthIndex: record.AuthIndex}}
	for range 2 {
		q := quotaTestResponse(.75, start)
		e.annotate(req, &q, record.RequestedAt)
		if len(q.Summary) != 2 || math.Abs(q.Summary[1].Value-24) > 1e-8 {
			t.Fatalf("history was skipped or counted twice: %+v", q.Summary)
		}
	}
}
