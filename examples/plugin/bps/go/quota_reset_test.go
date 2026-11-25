package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestBPSResetCreditCount(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		count float64
		known bool
	}{
		{`{"available_count":3}`, 3, true},
		{`{"available_count":"0"}`, 0, true},
		{`{"credits":[]}`, 0, true},
		{`{"available_count":null}`, 0, false},
		{`{"available_count":-1}`, 0, false},
		{`{"available_count":1.5}`, 0, false},
		{`{"available_count":"NaN"}`, 0, false},
		{`null`, 0, false},
		{`{}`, 0, false},
		{`{"credits":[{"reset_type":"codex_rate_limits","status":"available"},{"reset_type":"codex_rate_limits","status":"consumed"},{"reset_type":"other","status":"available"}]}`, 1, true},
	} {
		count, known := bpsResetCreditCount([]byte(tc.raw))
		if count != tc.count || known != tc.known {
			t.Fatalf("%s: got %v/%v", tc.raw, count, known)
		}
	}
}

func TestBPSQuotaResetConfirmation(t *testing.T) {
	for _, tc := range []struct {
		code    string
		status  int
		success bool
	}{
		{"reset", 200, true}, {"already_redeemed", 200, true},
		{"no_credit", 200, false}, {"nothing_to_reset", 200, false}, {"unknown", 200, false}, {"reset", 503, false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			fixture := quotaFixtureRequest()
			req := quotaResetRequest{QuotaResetRequest: pluginapi.QuotaResetRequest{Provider: fixture.Provider, Metadata: fixture.Metadata}, HostCallbackID: fixture.HostCallbackID}
			reads, writes := 0, 0
			result, err := resetBPSQuota(req, func(_ string, raw []byte) ([]byte, error) {
				var request quotaHTTPRequest
				if json.Unmarshal(raw, &request) != nil || request.Headers.Get("Authorization") != "Bearer fixture-token" || request.Headers.Get("ChatGPT-Account-Id") != "fixture-account" {
					t.Fatal("request context lost")
				}
				if request.Method == "GET" {
					reads++
					return okEnvelope(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"available_count":2}`)})
				}
				writes++
				var body struct {
					ID string `json:"redeem_request_id"`
				}
				if request.Method != "POST" || request.URL != bpsResetCreditsURL+"/consume" || request.Headers.Get("Content-Type") != "application/json" || json.Unmarshal(request.Body, &body) != nil {
					t.Fatal("invalid redemption")
				}
				if _, errID := uuid.Parse(body.ID); errID != nil {
					t.Fatal("missing redemption UUID")
				}
				return okEnvelope(pluginapi.HTTPResponse{StatusCode: tc.status, Body: []byte(`{"code":"` + tc.code + `","secret":"fixture-secret"}`)})
			})
			if reads != 1 || writes != 1 || result.Success != tc.success || (err == nil) != tc.success {
				t.Fatalf("unexpected result %+v %v, read=%d write=%d", result, err, reads, writes)
			}
			if err != nil && strings.Contains(err.Error(), "fixture-secret") {
				t.Fatal("upstream data leaked")
			}
		})
	}
}

func TestBPSQuotaResetNeverRetriesOrConsumesUnknownCredits(t *testing.T) {
	for _, credits := range []string{`{"available_count":0}`, `{}`, `{"available_count":1}`} {
		fixture := quotaFixtureRequest()
		req := quotaResetRequest{QuotaResetRequest: pluginapi.QuotaResetRequest{Provider: fixture.Provider, Metadata: fixture.Metadata}, HostCallbackID: fixture.HostCallbackID}
		writes := 0
		_, err := resetBPSQuota(req, func(_ string, raw []byte) ([]byte, error) {
			var request quotaHTTPRequest
			if json.Unmarshal(raw, &request) != nil {
				t.Fatal("invalid request")
			}
			if request.Method == "GET" {
				return okEnvelope(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(credits)})
			}
			writes++
			return nil, errors.New("fixture-secret")
		})
		expected := 0
		if credits == `{"available_count":1}` {
			expected = 1
		}
		if err == nil || writes != expected || strings.Contains(err.Error(), "fixture-secret") {
			t.Fatalf("unsafe redemption: writes=%d err=%v", writes, err)
		}
	}
}

func TestBPSOptionalResetCreditFailurePreservesUsageCount(t *testing.T) {
	quota, err := fetchBPSQuota(quotaFixtureRequest(), func(_ string, raw []byte) ([]byte, error) {
		var req quotaHTTPRequest
		if json.Unmarshal(raw, &req) != nil {
			t.Fatal("invalid request")
		}
		if req.URL == bpsResetCreditsURL {
			return nil, errors.New("fixture-secret")
		}
		return okEnvelope(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"plan_type":"pro","rate_limit_reset_credits":{"available_count":2}}`)})
	})
	if err != nil || len(quota.Summary) != 1 || quota.Summary[0].Value != 2 {
		t.Fatalf("lost fallback count: %+v %v", quota, err)
	}
}

func TestBPSQuotaResetSerializesSameAccountAndReleasesLock(t *testing.T) {
	fixture := quotaFixtureRequest()
	req := quotaResetRequest{QuotaResetRequest: pluginapi.QuotaResetRequest{Provider: fixture.Provider, Metadata: fixture.Metadata}, HostCallbackID: fixture.HostCallbackID}
	for attempt := 0; attempt < 2; attempt++ {
		reads, writes := 0, 0
		result, err := resetBPSQuota(req, func(_ string, raw []byte) ([]byte, error) {
			var request quotaHTTPRequest
			if json.Unmarshal(raw, &request) != nil {
				t.Fatal("invalid request")
			}
			if request.Method == "GET" {
				reads++
				_, errOverlap := resetBPSQuota(req, func(string, []byte) ([]byte, error) {
					t.Error("overlapping reset reached upstream")
					return nil, errors.New("unexpected callback")
				})
				if errOverlap == nil || !strings.Contains(errOverlap.Error(), "already in progress") {
					t.Fatalf("overlapping reset was not blocked: %v", errOverlap)
				}
				return okEnvelope(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"available_count":2}`)})
			}
			writes++
			return okEnvelope(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"code":"reset"}`)})
		})
		if err != nil || !result.Success || reads != 1 || writes != 1 {
			t.Fatalf("attempt %d: result=%+v err=%v reads=%d writes=%d", attempt, result, err, reads, writes)
		}
	}
}
