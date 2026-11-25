// Adapted from sunboyzoey/bps-local-public at 203c716. See ../../../NOTICE.
package bpsauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// ExcelClientID identifies the Excel application; it is not a per-login secret.
const ExcelClientID = "app_fnr0pYvVwwFDocDumLG3H2Bp"
const deviceRedirect = "https://auth.openai.com/basispoints/deviceauth/callback"

// DeviceClient implements the BPS browser/device authorization and token endpoints.
type DeviceClient struct {
	HTTP *http.Client
	base string
	now  func() time.Time
}

type DeviceCredentials struct {
	AccessToken       string          `json:"access_token"`
	RefreshToken      string          `json:"refresh_token,omitempty"`
	IDToken           string          `json:"id_token,omitempty"`
	TokenType         string          `json:"token_type"`
	ExpiresIn         int64           `json:"expires_in"`
	Scope             string          `json:"scope"`
	ObtainedAt        string          `json:"obtained_at"`
	ClientID          string          `json:"client_id"`
	EarliestRefreshAt json.RawMessage `json:"earliest_refresh_at,omitempty"`
}

// NewDeviceClient uses CPA proxy settings without routing OAuth secrets through request logs.
func NewDeviceClient(proxyAddress string) (*DeviceClient, error) {
	tr, _, err := proxyutil.BuildHTTPTransport(proxyAddress)
	if err != nil {
		return nil, errors.New("invalid OAuth proxy URL")
	}
	if tr == nil {
		tr = http.DefaultTransport.(*http.Transport).Clone()
	}
	return &DeviceClient{HTTP: &http.Client{Transport: tr, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		base: "https://auth.openai.com", now: time.Now}, nil
}

func (c *DeviceClient) Close() { c.HTTP.CloseIdleConnections() }

func (c *DeviceClient) post(ctx context.Context, path, kind string, data io.Reader, out any) (int, string, error) {
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, data)
	if e != nil {
		return 0, "", errors.New("cannot construct OAuth request")
	}
	req.Header.Set("Content-Type", kind)
	req.Header.Set("Accept", "application/json")
	resp, e := c.HTTP.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return 0, "", ctx.Err()
		}
		return 0, "", errors.New("OAuth transport failed (check proxy and connectivity)")
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Warn("bps OAuth response close failed")
		}
	}()
	raw, e := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if e != nil || len(raw) > 2<<20 {
		return resp.StatusCode, "", errors.New("cannot read OAuth response")
	}
	if resp.StatusCode != 200 {
		var v struct {
			Error json.RawMessage `json:"error"`
		}
		_ = json.Unmarshal(raw, &v)
		var code string
		if json.Unmarshal(v.Error, &code) != nil {
			var detail struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(v.Error, &detail)
			code = detail.Code
		}
		// Only known protocol codes may leave this boundary; descriptions and
		// arbitrary identifiers can contain credential material.
		switch code {
		case "invalid_grant", "invalid_client", "invalid_request", "unauthorized_client", "unsupported_grant_type", "invalid_scope", "access_denied", "authorization_pending", "deviceauth_authorization_pending", "slow_down", "expired_token", "server_error", "temporarily_unavailable":
		default:
			code = ""
		}
		return resp.StatusCode, code, nil
	}
	if json.Unmarshal(raw, out) != nil {
		return 200, "", errors.New("invalid OAuth JSON response")
	}
	return 200, "", nil
}
func jsonBody(v any) io.Reader { b, _ := json.Marshal(v); return strings.NewReader(string(b)) }

func (c *DeviceClient) exchange(ctx context.Context, authorizationCode, verifier, redirect string) (*DeviceCredentials, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {authorizationCode}, "client_id": {ExcelClientID}, "redirect_uri": {redirect}, "code_verifier": {verifier}}
	var creds DeviceCredentials
	status, code, e := c.post(ctx, "/oauth/token?unified=true", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), &creds)
	if e != nil {
		return nil, e
	}
	if status != 200 {
		return nil, fmt.Errorf("OAuth token exchange failed: HTTP %d (%s)", status, code)
	}
	if creds.AccessToken == "" || !strings.EqualFold(creds.TokenType, "Bearer") || creds.ExpiresIn <= 0 || creds.ExpiresIn > 31536000 {
		return nil, errors.New("invalid OAuth token response")
	}
	creds.ObtainedAt = c.now().UTC().Format(time.RFC3339)
	creds.ClientID = ExcelClientID
	return &creds, nil
}
