// Adapted from sunboyzoey/bps-local-public at c66fb410. See ../../../NOTICE.
package bpsauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/json"
	log "github.com/sirupsen/logrus"
)

// BrowserRedirect is registered to the BPS OAuth client and cannot be replaced
// with CPA's callback address. Users submit the resulting URL to CPA instead.
const BrowserRedirect = "https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback"

const maxCallbackBytes = 16 << 10

type browserFlow struct {
	verifier string
	url      string
	callback string
}

func startBrowser(state, authDir string) (*browserFlow, error) {
	var random [32]byte
	if _, errRandom := rand.Read(random[:]); errRandom != nil {
		return nil, errors.New("cannot initialize BPS browser authorization")
	}
	verifier := base64.RawURLEncoding.EncodeToString(random[:])
	digest := sha256.Sum256([]byte(verifier))
	params := url.Values{
		"client_id": {ExcelClientID}, "redirect_uri": {BrowserRedirect},
		"response_type": {"code"}, "scope": {"openid offline_access email profile organization.read"},
		"state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])},
		"code_challenge_method": {"S256"},
	}
	flow := &browserFlow{verifier: verifier, url: "https://auth.openai.com/oauth/authorize?" + params.Encode()}
	if strings.TrimSpace(authDir) != "" {
		// Reuse the host's authenticated OAuth callback submission endpoint.
		flow.callback = filepath.Join(authDir, ".oauth-bps-"+state+".oauth")
	}
	return flow, nil
}

func (f *browserFlow) cleanup() {
	if f == nil || f.callback == "" {
		return
	}
	if errRemove := os.Remove(f.callback); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
		log.Warn("bps OAuth callback cleanup failed")
	}
}

func browserAuthorizationCode(callback, state string) (string, error) {
	if len(callback) > maxCallbackBytes {
		return "", errors.New("BPS OAuth callback is too large")
	}
	u, errParse := url.Parse(callback)
	expected, _ := url.Parse(BrowserRedirect)
	if errParse != nil || u.Scheme != expected.Scheme || u.Host != expected.Host || u.Path != expected.Path || u.RawPath != "" || u.User != nil || strings.Contains(callback, "#") || u.Opaque != "" {
		return "", errors.New("BPS OAuth callback mismatch")
	}
	values, errQuery := url.ParseQuery(u.RawQuery)
	if errQuery != nil || len(values["state"]) != 1 || state == "" || subtle.ConstantTimeCompare([]byte(values.Get("state")), []byte(state)) != 1 {
		return "", errors.New("BPS OAuth state mismatch")
	}
	if _, exists := values["error"]; exists {
		return "", errors.New("BPS browser authorization was rejected")
	}
	if len(values["code"]) != 1 || strings.TrimSpace(values.Get("code")) == "" {
		return "", errors.New("BPS OAuth callback missing a single authorization code")
	}
	return values.Get("code"), nil
}

func (f *browserFlow) authorizationCode(state string, metadata map[string]any) (string, error) {
	if raw, exists := metadata["redirect_url"]; exists {
		callback, ok := raw.(string)
		if !ok {
			return "", errors.New("invalid BPS callback URL")
		}
		return browserAuthorizationCode(strings.TrimSpace(callback), state)
	}
	if f.callback == "" {
		return "", nil
	}
	file, errOpen := os.Open(f.callback)
	if errors.Is(errOpen, os.ErrNotExist) {
		return "", nil
	}
	if errOpen != nil {
		return "", errors.New("cannot read BPS OAuth callback")
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.Warn("bps OAuth callback close failed")
		}
	}()
	defer f.cleanup()
	raw, errRead := io.ReadAll(io.LimitReader(file, maxCallbackBytes+1))
	var payload struct {
		Code  string `json:"code"`
		State string `json:"state"`
		Error string `json:"error"`
	}
	if errRead != nil || len(raw) > maxCallbackBytes || json.Unmarshal(raw, &payload) != nil {
		return "", errors.New("invalid BPS OAuth callback")
	}
	if state == "" || subtle.ConstantTimeCompare([]byte(payload.State), []byte(state)) != 1 {
		return "", errors.New("BPS OAuth state mismatch")
	}
	if payload.Error != "" {
		return "", errors.New("BPS browser authorization was rejected")
	}
	if strings.TrimSpace(payload.Code) == "" {
		return "", errors.New("BPS OAuth callback missing authorization code")
	}
	return payload.Code, nil
}

func (c *DeviceClient) pollBrowser(ctx context.Context, flow *browserFlow, state string, metadata map[string]any) (*DeviceCredentials, error) {
	code, errCode := flow.authorizationCode(state, metadata)
	if errCode != nil || code == "" {
		return nil, errCode
	}
	credentials, errExchange := c.exchange(ctx, code, flow.verifier, BrowserRedirect)
	if errExchange != nil {
		return nil, errExchange
	}
	if strings.TrimSpace(credentials.RefreshToken) == "" {
		return nil, errors.New("BPS browser authorization did not return a refresh token")
	}
	return credentials, nil
}
