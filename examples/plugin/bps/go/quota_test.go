package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestBPSQuotaNormalizesWindowsAndCredits(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	raw := []byte(`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":18000,"reset_after_seconds":60},"secondary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":1800001000}},"code_review_rate_limit":{"primary_window":{"used_percent":2,"limit_window_seconds":604800}},"additional_rate_limits":[{"limit_name":"GPT additional","rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000}}}],"credits":{"balance":"12.5","unlimited":false}}`)
	quota, err := normalizeBPSQuota(raw, http.Header{"Date": {now.Add(10 * time.Second).Format(http.TimeFormat)}}, now)
	if err != nil || quota.Subscription.Plan != "pro" || len(quota.Groups) != 3 || quota.ServerTimeOffsetMs != 10000 {
		t.Fatalf("invalid normalized quota: %+v, %v", quota, err)
	}
	first := quota.Groups[0].Buckets
	if len(first) != 2 || first[0].Window != "5h" || first[0].RemainingFraction != 0.75 || first[0].ResetTime != now.Add(70*time.Second).Format(time.RFC3339) || first[1].Window != "7d" || first[1].RemainingFraction != 0 || first[1].ResetTime != time.Unix(1800001000, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("invalid windows: %+v", first)
	}
	if len(quota.Summary) != 1 || quota.Summary[0].Value != 12.5 || quota.Summary[0].Currency != "" || quota.Summary[0].Unit != "credits" {
		t.Fatalf("invalid credit units: %+v", quota.Summary)
	}
}

func TestBPSQuotaUnknownAndZeroAreDistinct(t *testing.T) {
	for _, raw := range []string{`null`, `""`, `"NaN"`, `"Inf"`, `-1`, `{}`, `true`} {
		quota, err := normalizeBPSQuota([]byte(`{"plan_type":"business","rate_limit":{"primary_window":{},"secondary_window":{"used_percent":null}},"credits":{"balance":`+raw+`}}`), nil, time.Unix(1800000000, 0))
		if err != nil || len(quota.Summary) != 0 || len(quota.Groups) != 0 {
			t.Fatalf("unknown quota became zero or available: %s, %+v, %v", raw, quota, err)
		}
	}
	quota, err := normalizeBPSQuota([]byte(`{"rate_limit":{"primary_window":{"used_percent":0}},"credits":{"balance":0}}`), nil, time.Now())
	if err != nil || len(quota.Groups) != 1 || quota.Groups[0].Buckets[0].RemainingFraction != 1 || len(quota.Summary) != 1 || quota.Summary[0].Value != 0 {
		t.Fatalf("explicit zero lost: %+v, %v", quota, err)
	}
	quota, err = normalizeBPSQuota([]byte(`{"credits":{"unlimited":true,"balance":"0"}}`), nil, time.Now())
	if err != nil || len(quota.Groups) != 1 || quota.Groups[0].Buckets[0].Description != "Unlimited credits" || len(quota.Summary) != 0 {
		t.Fatalf("unlimited credits shown as zero: %+v, %v", quota, err)
	}
}

func TestBPSQuotaRejectsMalformedAndUnrecognizedResponses(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `[]`, `<html>error</html>`, `{"error":{"message":"secret"}}`, `{"rate_limit":{"primary_window":{"used_percent":101}}}`, `{"rate_limit":{"primary_window":{"used_percent":-1}}}`, `{"plan_type":"` + strings.Repeat("x", 2<<20) + `"}`} {
		if _, err := normalizeBPSQuota([]byte(raw), nil, time.Now()); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("malformed quota accepted or upstream error leaked")
		}
	}
}

func quotaFixtureRequest() quotaFetchRequest {
	return quotaFetchRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{Provider: "bps", Metadata: map[string]any{"access_token": "fixture-token", "account_id": "fixture-account"}}, HostCallbackID: "fixture-callback"}
}

func TestBPSQuotaUsesHostTransportAndAccount(t *testing.T) {
	calls := 0
	quota, err := fetchBPSQuota(quotaFixtureRequest(), func(method string, raw []byte) ([]byte, error) {
		calls++
		var request quotaHTTPRequest
		if json.Unmarshal(raw, &request) != nil || method != pluginabi.MethodHostHTTPDo || request.Method != "GET" || request.URL != bpsQuotaURL || request.HostCallbackID != "fixture-callback" || request.Headers.Get("Authorization") != "Bearer fixture-token" || request.Headers.Get("ChatGPT-Account-Id") != "fixture-account" {
			t.Fatal("quota request lost its host context or account scope")
		}
		return okEnvelope(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"rate_limit":{"primary_window":{"used_percent":20}},"credits":{"balance":null}}`)})
	})
	if err != nil || calls != 1 || len(quota.Groups) != 1 || quota.Groups[0].Buckets[0].RemainingFraction != 0.8 {
		t.Fatalf("quota fetch failed: %+v, %v, calls=%d", quota, err, calls)
	}
}

func TestBPSQuotaFailuresNeverExposeSecretsOrRetry(t *testing.T) {
	for _, status := range []int{301, 401, 403, 429, 500} {
		calls := 0
		_, err := fetchBPSQuota(quotaFixtureRequest(), func(string, []byte) ([]byte, error) {
			calls++
			return okEnvelope(pluginapi.HTTPResponse{StatusCode: status, Body: []byte(`{"error":"fixture-secret"}`)})
		})
		if err == nil || strings.Contains(err.Error(), "fixture-secret") || calls != 1 {
			t.Fatalf("HTTP %d failure leaked, retried, or accepted", status)
		}
	}
	for _, callback := range []func(string, []byte) ([]byte, error){
		func(string, []byte) ([]byte, error) { return nil, errors.New("fixture-secret") },
		func(string, []byte) ([]byte, error) {
			return []byte(`{"ok":false,"error":{"message":"fixture-secret"}}`), nil
		},
	} {
		if _, err := fetchBPSQuota(quotaFixtureRequest(), callback); err == nil || strings.Contains(err.Error(), "fixture-secret") {
			t.Fatal("host callback failure leaked or accepted")
		}
	}
	for _, req := range []quotaFetchRequest{{}, {QuotaFetchRequest: pluginapi.QuotaFetchRequest{Provider: "codex"}}, {QuotaFetchRequest: pluginapi.QuotaFetchRequest{Provider: "bps"}}} {
		_, err := fetchBPSQuota(req, func(string, []byte) ([]byte, error) { t.Fatal("invalid credential sent upstream"); return nil, nil })
		if err == nil {
			t.Fatal("invalid quota credential accepted")
		}
	}
}
