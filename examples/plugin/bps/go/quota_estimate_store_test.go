package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	after := quotaTestAnnotate(e, .7, now.Add(time.Second))
	if len(after.Summary) != 2 || math.Abs(after.Summary[1].Value-73) > 1e-8 || math.Abs(after.Summary[0].Value-51.1) > 1e-8 {
		t.Fatalf("estimate did not survive restart: %+v", after.Summary)
	}
	key, _ := quotaEstimateKey("test-index", "")
	if !e.accounts[key].Windows["0:7d"].Started.Equal(now.Add(time.Second)) {
		t.Fatal("sampling baseline crossed restart")
	}
	e.observe(quotaTestRecord(), now)
	after = quotaTestAnnotate(e, .69, now.Add(time.Second))
	if len(after.Summary) != 2 || math.Abs(after.Summary[1].Value-146) > 1e-8 {
		t.Fatal("fresh calibration included offline usage")
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
	for _, kind := range []string{"plan", "reset", "refill", "other account", "corrupt", "oversize"} {
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
			if kind == "oversize" {
				_ = os.WriteFile(path, []byte(strings.Repeat(" ", (1<<20)+2)), 0600)
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
			if len(q.Summary) != 2 || math.Abs(q.Summary[1].Value-73) > 1e-8 {
				t.Fatal("lost completed calibration during observation gap")
			}
			e.observe(quotaTestRecord(), now)
			q = quotaTestAnnotate(e, .74, now)
			if len(q.Summary) != 2 || math.Abs(q.Summary[1].Value-146) > 1e-8 {
				t.Fatal("partial sample polluted fresh calibration")
			}
		})
	}
}
