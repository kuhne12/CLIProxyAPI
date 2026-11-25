package main

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// BPS directs users to Codex usage settings; this is the same account quota API.
const bpsQuotaURL = "https://chatgpt.com/backend-api/wham/usage"
const bpsResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
const bpsResetCreditsMetric = "rate_limit_reset_credits_available_count"

type quotaFetchRequest struct {
	pluginapi.QuotaFetchRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type quotaHTTPRequest struct {
	HostCallbackID string      `json:"host_callback_id"`
	Method         string      `json:"method"`
	URL            string      `json:"url"`
	Headers        http.Header `json:"headers"`
	Body           []byte      `json:"body,omitempty"`
}

func fetchBPSQuota(req quotaFetchRequest, call func(string, []byte) ([]byte, error)) (pluginapi.QuotaFetchResponse, error) {
	var empty pluginapi.QuotaFetchResponse
	headers, errHeaders := bpsQuotaHeaders(req)
	if errHeaders != nil {
		return empty, errHeaders
	}
	raw, errMarshal := json.Marshal(quotaHTTPRequest{
		HostCallbackID: req.HostCallbackID, Method: http.MethodGet, URL: bpsQuotaURL,
		Headers: headers,
	})
	if errMarshal != nil {
		return empty, errors.New("cannot construct BPS quota request")
	}
	response, errCall := call(pluginabi.MethodHostHTTPDo, raw)
	if errCall != nil {
		return empty, errors.New("BPS quota transport failed; check connectivity and proxy settings")
	}
	var envelope pluginabi.Envelope
	var upstream pluginapi.HTTPResponse
	if json.Unmarshal(response, &envelope) != nil || !envelope.OK {
		return empty, errors.New("BPS quota host request failed")
	}
	if json.Unmarshal(envelope.Result, &upstream) != nil {
		return empty, errors.New("invalid BPS quota host response")
	}
	if upstream.StatusCode != http.StatusOK {
		switch upstream.StatusCode {
		case http.StatusUnauthorized:
			return empty, errors.New("BPS quota authorization expired; refresh or sign in again (HTTP 401)")
		case http.StatusForbidden:
			return empty, errors.New("BPS quota access was denied (HTTP 403)")
		case http.StatusTooManyRequests:
			return empty, errors.New("BPS quota query is rate limited; retry later (HTTP 429)")
		default:
			return empty, fmt.Errorf("BPS quota query failed (HTTP %d)", upstream.StatusCode)
		}
	}
	quota, errNormalize := normalizeBPSQuota(upstream.Body, upstream.Headers, time.Now())
	if errNormalize != nil {
		return empty, errNormalize
	}
	// Credit details may be newer than the usage snapshot. Failure must not hide
	// usable quota windows or replace a known inline count with zero.
	if count, ok := fetchBPSResetCredits(req.HostCallbackID, headers, call); ok {
		setBPSResetCredits(&quota, count)
	}
	return quota, nil
}

func bpsQuotaHeaders(req quotaFetchRequest) (http.Header, error) {
	if req.Provider != "bps" {
		return nil, errors.New("BPS quota requires a BPS credential")
	}
	metadata := req.Metadata
	if len(metadata) == 0 && len(req.StorageJSON) > 0 {
		if errDecode := json.Unmarshal(req.StorageJSON, &metadata); errDecode != nil {
			return nil, errors.New("invalid BPS credential data")
		}
	}
	token, _ := metadata["access_token"].(string)
	account, _ := metadata["account_id"].(string)
	if strings.TrimSpace(token) == "" || strings.TrimSpace(account) == "" {
		return nil, errors.New("BPS quota requires an access token and account ID; refresh or sign in again")
	}
	if req.HostCallbackID == "" {
		return nil, errors.New("BPS quota host request context is unavailable")
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("ChatGPT-Account-Id", account)
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", "Mozilla/5.0")
	return headers, nil
}

func fetchBPSResetCredits(callbackID string, headers http.Header, call func(string, []byte) ([]byte, error)) (float64, bool) {
	headers = headers.Clone()
	headers.Set("OpenAI-Beta", "codex-1")
	headers.Set("Originator", "Codex Desktop")
	raw, errMarshal := json.Marshal(quotaHTTPRequest{HostCallbackID: callbackID, Method: http.MethodGet, URL: bpsResetCreditsURL, Headers: headers})
	if errMarshal != nil {
		return 0, false
	}
	response, errCall := call(pluginabi.MethodHostHTTPDo, raw)
	if errCall != nil {
		return 0, false
	}
	var envelope pluginabi.Envelope
	var upstream pluginapi.HTTPResponse
	if json.Unmarshal(response, &envelope) != nil || !envelope.OK || json.Unmarshal(envelope.Result, &upstream) != nil || upstream.StatusCode != http.StatusOK || len(upstream.Body) > 2<<20 {
		return 0, false
	}
	return bpsResetCreditCount(upstream.Body)
}

func bpsResetCreditCount(raw []byte) (float64, bool) {
	var credits struct {
		Available json.RawMessage `json:"available_count"`
		Credits   []struct {
			Type   string `json:"reset_type"`
			Status string `json:"status"`
		} `json:"credits"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &credits) != nil {
		return 0, false
	}
	if count, ok := quotaNumber(credits.Available); ok && count == math.Trunc(count) && count <= 1<<53-1 {
		return count, true
	}
	// An explicit empty list is known zero. Missing/null details stay unknown.
	if credits.Credits == nil {
		return 0, false
	}
	count := 0
	for _, credit := range credits.Credits {
		if credit.Type == "codex_rate_limits" && credit.Status == "available" {
			count++
		}
	}
	return float64(count), true
}

func setBPSResetCredits(quota *pluginapi.QuotaFetchResponse, count float64) {
	for i := range quota.Summary {
		if quota.Summary[i].Key == bpsResetCreditsMetric {
			quota.Summary[i].Value = count
			return
		}
	}
	quota.Summary = append(quota.Summary, pluginapi.QuotaMetric{Key: bpsResetCreditsMetric, Label: "Available quota resets", Value: count, Format: "number"})
}

type quotaWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds int64    `json:"limit_window_seconds"`
	ResetAt            int64    `json:"reset_at"`
	ResetAfterSeconds  *int64   `json:"reset_after_seconds"`
}

type quotaRateLimit struct {
	Primary   *quotaWindow `json:"primary_window"`
	Secondary *quotaWindow `json:"secondary_window"`
}

type quotaUsage struct {
	ResetCredits json.RawMessage `json:"rate_limit_reset_credits"`
	PlanType     string          `json:"plan_type"`
	RateLimit    quotaRateLimit  `json:"rate_limit"`
	CodeReview   quotaRateLimit  `json:"code_review_rate_limit"`
	Additional   []struct {
		Name  string         `json:"limit_name"`
		Limit quotaRateLimit `json:"rate_limit"`
	} `json:"additional_rate_limits"`
	Credits *struct {
		Unlimited bool            `json:"unlimited"`
		Balance   json.RawMessage `json:"balance"`
	} `json:"credits"`
}

func normalizeBPSQuota(raw []byte, headers http.Header, now time.Time) (pluginapi.QuotaFetchResponse, error) {
	var out pluginapi.QuotaFetchResponse
	var usage quotaUsage
	if len(raw) > 2<<20 || json.Unmarshal(raw, &usage) != nil {
		return out, errors.New("invalid BPS account quota response")
	}
	serverTime := now
	if date, errDate := http.ParseTime(headers.Get("Date")); errDate == nil {
		serverTime = date
		out.ServerTimeOffsetMs = date.Sub(now).Milliseconds()
	}
	if usage.PlanType != "" {
		out.Subscription = &pluginapi.QuotaSubscription{Plan: usage.PlanType}
	}
	appendGroup := func(name string, limit quotaRateLimit) {
		group := pluginapi.QuotaGroup{DisplayName: name}
		for i, window := range []*quotaWindow{limit.Primary, limit.Secondary} {
			if bucket, ok := normalizeBPSWindow(window, i, serverTime); ok {
				group.Buckets = append(group.Buckets, bucket)
			}
		}
		if len(group.Buckets) > 0 {
			out.Groups = append(out.Groups, group)
		}
	}
	appendGroup("Account usage", usage.RateLimit)
	appendGroup("Code review", usage.CodeReview)
	for _, additional := range usage.Additional {
		if additional.Name != "" {
			appendGroup(additional.Name, additional.Limit)
		}
	}
	if usage.Credits != nil {
		if usage.Credits.Unlimited {
			out.Groups = append(out.Groups, pluginapi.QuotaGroup{DisplayName: "Credits", Buckets: []pluginapi.QuotaBucket{{Window: "credits", RemainingFraction: 1, Description: "Unlimited credits"}}})
		} else if balance, ok := quotaNumber(usage.Credits.Balance); ok {
			out.Summary = append(out.Summary, pluginapi.QuotaMetric{Key: "credits_balance", Label: "Credits balance", Value: balance, Unit: "credits", Format: "number"})
		}
	}
	if count, ok := bpsResetCreditCount(usage.ResetCredits); ok {
		setBPSResetCredits(&out, count)
	}
	if len(out.Groups) == 0 && len(out.Summary) == 0 && out.Subscription == nil {
		return pluginapi.QuotaFetchResponse{}, errors.New("BPS account quota response contains no supported quota fields")
	}
	return out, nil
}

func normalizeBPSWindow(window *quotaWindow, index int, now time.Time) (pluginapi.QuotaBucket, bool) {
	if window == nil || window.UsedPercent == nil || math.IsNaN(*window.UsedPercent) || math.IsInf(*window.UsedPercent, 0) || *window.UsedPercent < 0 || *window.UsedPercent > 100 {
		return pluginapi.QuotaBucket{}, false
	}
	label := "Primary window"
	if index == 1 {
		label = "Secondary window"
	}
	seconds := window.LimitWindowSeconds
	if seconds > 0 {
		switch {
		case seconds%86400 == 0:
			label = fmt.Sprintf("%dd", seconds/86400)
		case seconds%3600 == 0:
			label = fmt.Sprintf("%dh", seconds/3600)
		case seconds%60 == 0:
			label = fmt.Sprintf("%dm", seconds/60)
		default:
			label = fmt.Sprintf("%ds", seconds)
		}
	}
	bucket := pluginapi.QuotaBucket{Window: label, RemainingFraction: (100 - *window.UsedPercent) / 100}
	if window.ResetAt > 0 && window.ResetAt <= 253402300799 {
		bucket.ResetTime = time.Unix(window.ResetAt, 0).UTC().Format(time.RFC3339)
	} else if window.ResetAfterSeconds != nil && *window.ResetAfterSeconds >= 0 && *window.ResetAfterSeconds <= 31536000 {
		bucket.ResetTime = now.Add(time.Duration(*window.ResetAfterSeconds) * time.Second).UTC().Format(time.RFC3339)
	}
	return bucket, true
}

func quotaNumber(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var value float64
	if json.Unmarshal(raw, &value) != nil {
		var text string
		if json.Unmarshal(raw, &text) != nil || strings.TrimSpace(text) == "" {
			return 0, false
		}
		var errParse error
		value, errParse = strconv.ParseFloat(text, 64)
		if errParse != nil {
			return 0, false
		}
	}
	return value, value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
