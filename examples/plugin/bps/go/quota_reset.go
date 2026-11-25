package main

import (
	"errors"
	"net/http"
	"sync"

	"github.com/MoeForge/moego/json"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type quotaResetRequest struct {
	pluginapi.QuotaResetRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

var bpsResetAccounts sync.Map

func resetBPSQuota(req quotaResetRequest, call func(string, []byte) ([]byte, error)) (pluginapi.QuotaResetResponse, error) {
	var empty pluginapi.QuotaResetResponse
	headers, errHeaders := bpsQuotaHeaders(quotaFetchRequest{QuotaFetchRequest: pluginapi.QuotaFetchRequest{
		Provider: req.Provider, Metadata: req.Metadata, StorageJSON: req.StorageJSON,
	}, HostCallbackID: req.HostCallbackID})
	if errHeaders != nil {
		return empty, errHeaders
	}
	account := headers.Get("ChatGPT-Account-Id")
	if _, loaded := bpsResetAccounts.LoadOrStore(account, struct{}{}); loaded {
		return empty, errors.New("BPS quota reset is already in progress")
	}
	defer bpsResetAccounts.Delete(account)
	// Recheck availability immediately before the only destructive request.
	count, known := fetchBPSResetCredits(req.HostCallbackID, headers, call)
	if !known {
		return empty, errors.New("BPS reset credit availability could not be verified; refresh quota first")
	}
	if count <= 0 {
		return empty, errors.New("BPS has no available quota reset credits")
	}
	body, errBody := json.Marshal(struct {
		RequestID string `json:"redeem_request_id"`
	}{RequestID: uuid.NewString()})
	if errBody != nil {
		return empty, errors.New("cannot construct BPS quota reset request")
	}
	headers.Set("Content-Type", "application/json")
	headers.Set("OpenAI-Beta", "codex-1")
	headers.Set("Originator", "Codex Desktop")
	raw, errMarshal := json.Marshal(quotaHTTPRequest{HostCallbackID: req.HostCallbackID, Method: http.MethodPost, URL: bpsResetCreditsURL + "/consume", Headers: headers, Body: body})
	if errMarshal != nil {
		return empty, errors.New("cannot construct BPS quota reset transport request")
	}
	// Never retry redemption: an interrupted response can still have consumed a credit.
	response, errCall := call(pluginabi.MethodHostHTTPDo, raw)
	if errCall != nil {
		return empty, errors.New("BPS quota reset result is unknown; refresh quota before retrying")
	}
	var envelope pluginabi.Envelope
	var upstream pluginapi.HTTPResponse
	if json.Unmarshal(response, &envelope) != nil || !envelope.OK || json.Unmarshal(envelope.Result, &upstream) != nil || len(upstream.Body) > 2<<20 {
		return empty, errors.New("BPS quota reset result is unknown; refresh quota before retrying")
	}
	var result struct {
		Code string `json:"code"`
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 || json.Unmarshal(upstream.Body, &result) != nil || (result.Code != "reset" && result.Code != "already_redeemed") {
		return empty, errors.New("BPS quota reset was not confirmed; refresh quota before retrying")
	}
	return pluginapi.QuotaResetResponse{Success: true}, nil
}
