// Adapted from sunboyzoey/bps-local-public at 203c716. See ../../../NOTICE.
package bpsauth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/json"
)

const loginLifetime = 15 * time.Minute

type deviceFlow struct {
	id       string
	code     string
	interval time.Duration
	url      string
}

// Start and poll preserve the upstream device protocol while allowing the host
// to own user interaction, cancellation, and credential persistence.
func (c *DeviceClient) start(ctx context.Context) (deviceFlow, error) {
	var response struct {
		ID       string          `json:"device_auth_id"`
		Code     string          `json:"user_code"`
		Interval json.RawMessage `json:"interval"`
	}
	status, code, err := c.post(ctx, "/api/accounts/deviceauth/usercode", "application/json", jsonBody(map[string]string{"client_id": ExcelClientID}), &response)
	if err != nil {
		return deviceFlow{}, err
	}
	if status != 200 {
		return deviceFlow{}, fmt.Errorf("device-code request failed: HTTP %d (%s)", status, code)
	}
	if response.ID == "" || response.Code == "" {
		return deviceFlow{}, errors.New("device-code response missing required fields")
	}
	seconds := 5
	if len(response.Interval) > 0 {
		parsed, errParse := strconv.Atoi(strings.Trim(string(response.Interval), `"`))
		if errParse != nil || parsed < 1 || parsed > 300 {
			return deviceFlow{}, errors.New("invalid device polling interval")
		}
		seconds = parsed
	}
	return deviceFlow{id: response.ID, code: response.Code, interval: time.Duration(seconds) * time.Second,
		url: c.base + "/api/accounts/deviceauth/authorize?client_id=" + url.QueryEscape(ExcelClientID)}, nil
}

func (c *DeviceClient) poll(ctx context.Context, flow *deviceFlow) (*DeviceCredentials, error) {
	var grant struct {
		Code      string `json:"authorization_code"`
		Verifier  string `json:"code_verifier"`
		Challenge string `json:"code_challenge"`
	}
	status, code, err := c.post(ctx, "/api/accounts/deviceauth/token", "application/json", jsonBody(map[string]string{
		"client_id": ExcelClientID, "device_auth_id": flow.id, "user_code": flow.code,
	}), &grant)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		switch {
		case (status == 400 || status == 403) && (code == "authorization_pending" || code == "deviceauth_authorization_pending"):
			return nil, nil
		case (status == 400 || status == 429) && code == "slow_down":
			flow.interval = min(flow.interval+5*time.Second, 300*time.Second)
			return nil, nil
		default:
			return nil, fmt.Errorf("device authorization failed: HTTP %d (%s)", status, code)
		}
	}
	if grant.Code == "" || len(grant.Verifier) < 43 || len(grant.Verifier) > 128 || grant.Challenge == "" {
		return nil, errors.New("device authorization missing valid code/PKCE fields")
	}
	digest := sha256.Sum256([]byte(grant.Verifier))
	if base64.RawURLEncoding.EncodeToString(digest[:]) != grant.Challenge {
		return nil, errors.New("device authorization PKCE mismatch")
	}
	return c.exchange(ctx, grant.Code, grant.Verifier, deviceRedirect)
}
