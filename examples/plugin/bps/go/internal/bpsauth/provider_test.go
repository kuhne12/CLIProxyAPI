package bpsauth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type oauthFixture struct {
	provider  *Provider
	now       atomic.Int64
	polls     atomic.Int32
	exchanges atomic.Int32
	mode      string
}

func testToken(account, subject, clientID string, expires time.Time) string {
	claims, _ := json.Marshal(map[string]any{"sub": subject, "client_id": clientID, "exp": expires.Unix(),
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account}})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".test"
}

func newFixture(t *testing.T, mode string) *oauthFixture {
	t.Helper()
	f := &oauthFixture{provider: New(), mode: mode}
	f.now.Store(time.Now().UnixNano())
	clock := func() time.Time { return time.Unix(0, f.now.Load()) }
	verifier := strings.Repeat("v", 43)
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			t.Error("OAuth must use POST")
		}
		write := func(value any) { _ = json.NewEncoder(w).Encode(value) }
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req["client_id"] != ExcelClientID {
				t.Error("wrong device client")
			}
			interval := "5"
			if mode == "bad_interval" {
				interval = "0"
			}
			write(map[string]any{"device_auth_id": "private-device-id", "user_code": "ABCD-EFGH", "interval": interval})
		case "/api/accounts/deviceauth/token":
			count := f.polls.Add(1)
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req["client_id"] != ExcelClientID || req["device_auth_id"] != "private-device-id" || req["user_code"] != "ABCD-EFGH" {
				t.Error("wrong poll fields")
			}
			if count == 1 && mode == "pending" {
				w.WriteHeader(http.StatusForbidden)
				write(map[string]any{"error": map[string]string{"code": "deviceauth_authorization_pending"}})
				return
			}
			if count == 1 && mode == "slow_down" {
				w.WriteHeader(http.StatusTooManyRequests)
				write(map[string]string{"error": "slow_down"})
				return
			}
			if mode == "denied" {
				w.WriteHeader(http.StatusForbidden)
				write(map[string]string{"error": "access_denied", "error_description": "secret-must-not-leak"})
				return
			}
			if mode == "bad_pkce" {
				challenge = "wrong"
			}
			write(map[string]string{"authorization_code": "private-code", "code_verifier": verifier, "code_challenge": challenge})
		case "/oauth/token":
			f.exchanges.Add(1)
			_ = r.ParseForm()
			if r.URL.Query().Get("unified") != "true" || r.Form.Get("client_id") != ExcelClientID {
				t.Error("wrong BPS token exchange")
			}
			if r.Form.Get("grant_type") == "authorization_code" && (r.Form.Get("redirect_uri") != deviceRedirect || r.Form.Get("code_verifier") != verifier) {
				t.Error("wrong PKCE exchange")
			}
			if mode == "invalid_grant" || mode == "ambiguous" {
				status := http.StatusBadRequest
				if mode == "ambiguous" {
					status = http.StatusInternalServerError
				}
				w.WriteHeader(status)
				write(map[string]string{"error": "invalid_grant", "error_description": "secret-must-not-leak"})
				return
			}
			account := "acct-test"
			if mode == "different_account" {
				account = "acct-other"
			}
			write(map[string]any{"access_token": testToken(account, "user-1", ExcelClientID, clock().Add(time.Hour)),
				"refresh_token": "new-refresh", "token_type": "Bearer", "expires_in": 3600, "earliest_refresh_at": 12345})
		default:
			t.Errorf("unexpected OAuth endpoint %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	f.provider.now = clock
	f.provider.newClient = func(string) (*DeviceClient, error) {
		return &DeviceClient{HTTP: server.Client(), base: server.URL, now: clock}, nil
	}
	t.Cleanup(f.provider.Close)
	return f
}

func (f *oauthFixture) advance(duration time.Duration) { f.now.Add(int64(duration)) }

func TestDeviceLoginPollingAndConcurrentCompletion(t *testing.T) {
	f := newFixture(t, "pending")
	start, err := f.provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{Metadata: map[string]any{"method": "device"}})
	if err != nil || start.UserCode != "ABCD-EFGH" || start.Interval != 5 {
		t.Fatalf("start: code=%q interval=%d err=%v", start.UserCode, start.Interval, err)
	}
	raw, _ := json.Marshal(start)
	if strings.Contains(string(raw), "private-device-id") {
		t.Fatal("private login state leaked")
	}
	req := pluginapi.AuthLoginPollRequest{State: start.State}
	response, err := f.provider.PollLogin(context.Background(), req)
	if err != nil || response.Status != pluginapi.AuthLoginStatusPending || f.polls.Load() != 0 {
		t.Fatal("early poll reached upstream")
	}
	f.advance(5 * time.Second)
	response, err = f.provider.PollLogin(context.Background(), req)
	if err != nil || response.Status != pluginapi.AuthLoginStatusPending || f.polls.Load() != 1 {
		t.Fatal("pending poll failed")
	}
	f.advance(5 * time.Second)
	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			result, errPoll := f.provider.PollLogin(context.Background(), req)
			if errPoll != nil || result.Status != pluginapi.AuthLoginStatusSuccess || result.Auth.Provider != "bps" {
				t.Errorf("completion: status=%s error=%v", result.Status, errPoll)
			}
			if !strings.HasPrefix(result.Auth.FileName, "bps-") || result.Auth.Metadata["expired"] == "" {
				t.Error("missing stable credential identity or expiry")
			}
		})
	}
	wait.Wait()
	if f.exchanges.Load() != 1 || f.polls.Load() != 2 {
		t.Fatalf("duplicate exchange: polls=%d exchanges=%d", f.polls.Load(), f.exchanges.Load())
	}
}

func TestDeviceFailuresNeverExchangeOrLeakSecrets(t *testing.T) {
	for _, mode := range []string{"bad_interval", "bad_pkce", "denied", "expired", "cancelled", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, mode)
			start, err := f.provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{Metadata: map[string]any{"method": "device"}})
			if mode != "bad_interval" {
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				f.advance(5 * time.Second)
				if mode == "expired" {
					f.advance(loginLifetime)
				}
				if mode == "cancelled" {
					cancel()
				}
				if mode == "shutdown" {
					f.provider.Close()
				}
				_, err = f.provider.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: start.State})
			}
			if err == nil || strings.Contains(err.Error(), "secret-must-not-leak") || f.exchanges.Load() != 0 {
				t.Fatalf("failure handling: error=%v exchanges=%d", err, f.exchanges.Load())
			}
		})
	}
}

func TestSlowDownHonorsIncreasedInterval(t *testing.T) {
	f := newFixture(t, "slow_down")
	start, err := f.provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{Metadata: map[string]any{"method": "device"}})
	if err != nil {
		t.Fatal(err)
	}
	req := pluginapi.AuthLoginPollRequest{State: start.State}
	f.advance(5 * time.Second)
	_, _ = f.provider.PollLogin(context.Background(), req)
	f.advance(5 * time.Second)
	_, _ = f.provider.PollLogin(context.Background(), req)
	if f.polls.Load() != 1 {
		t.Fatal("slow_down interval ignored")
	}
	f.advance(5 * time.Second)
	result, err := f.provider.PollLogin(context.Background(), req)
	if err != nil || result.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("completion: %v", err)
	}
}

func testCredential(t *testing.T) Credential {
	t.Helper()
	credential, err := normalizeCredential(DeviceCredentials{AccessToken: testToken("acct-test", "user-1", ExcelClientID, time.Now().Add(time.Hour)),
		RefreshToken: "old-refresh", ClientID: ExcelClientID, TokenType: "Bearer"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

func TestImportAndRefreshPreserveSettingsAndReplaceTokens(t *testing.T) {
	f := newFixture(t, "success")
	initial, err := authData(testCredential(t), map[string]any{"prefix": "bps", "disabled": true, "weight": 7, "proxy_url": "direct"})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := f.provider.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: initial.StorageJSON, FileName: "existing.json"})
	if err != nil || !parsed.Handled || parsed.Auth.ID != "existing.json" || !parsed.Auth.Disabled {
		t.Fatalf("import failed: handled=%t err=%v", parsed.Handled, err)
	}
	refreshed, err := f.provider.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{
		AuthID: "existing.json", AuthProvider: "bps", StorageJSON: initial.StorageJSON, Metadata: initial.Metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Auth.Metadata["refresh_token"] != "new-refresh" || fmt.Sprint(refreshed.Auth.Metadata["weight"]) != "7" || !refreshed.Auth.Disabled || refreshed.Auth.Prefix != "bps" {
		t.Fatal("refresh lost settings or retained old tokens")
	}
	if refreshed.Auth.ID != "existing.json" || refreshed.Auth.FileName != "" || f.exchanges.Load() != 1 {
		t.Fatal("refresh changed file identity or exchanged twice")
	}
	credential, _, _, err := parseCredential(refreshed.Auth.StorageJSON, true)
	if err != nil || credential.RefreshToken != "new-refresh" {
		t.Fatal("persisted token disagrees with runtime metadata")
	}
}

func TestRefreshErrorsAreSanitizedAndNeverRetried(t *testing.T) {
	for _, mode := range []string{"invalid_grant", "ambiguous", "different_account"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, mode)
			initial, _ := authData(testCredential(t), nil)
			_, err := f.provider.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{AuthProvider: "bps", StorageJSON: initial.StorageJSON})
			if err == nil || f.exchanges.Load() != 1 || strings.Contains(err.Error(), "secret-must-not-leak") {
				t.Fatalf("refresh error=%v calls=%d", err, f.exchanges.Load())
			}
			if errors.Is(err, ErrRefreshGrantRejected) != (mode == "invalid_grant") {
				t.Fatal("ambiguous failure treated as definitive rejection")
			}
		})
	}
}

func TestImportRejectsCodexAndAmbiguousCredentials(t *testing.T) {
	p := New()
	t.Cleanup(p.Close)
	valid := testCredential(t)
	for _, tc := range []struct {
		name string
		doc  map[string]any
	}{
		{"codex", map[string]any{"type": "codex", "client_id": ExcelClientID}},
		{"wrong_client", map[string]any{"type": "bps", "client_id": "codex", "access_token": valid.AccessToken}},
		{"both", map[string]any{"type": "bps", "client_id": ExcelClientID, "access_token": valid.AccessToken, "tokens": map[string]string{"access_token": valid.AccessToken}}},
		{"opaque_no_expiry", map[string]any{"type": "bps", "client_id": ExcelClientID, "access_token": "opaque", "account_id": "acct-test", "expires_in": 3600}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.doc)
			parsed, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: raw})
			if err == nil && parsed.Handled {
				t.Fatal("invalid credential accepted")
			}
		})
	}
	flat, _ := json.Marshal(valid.DeviceCredentials)
	parsed, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: flat})
	if err != nil || !parsed.Handled {
		t.Fatalf("original bps-local format rejected: %v", err)
	}
	for range 2 {
		parsedAgain, errParse := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: parsed.Auth.StorageJSON})
		if errParse != nil || parsedAgain.Auth.ID != parsed.Auth.ID {
			t.Fatal("credential identity changed on reload")
		}
	}
}

func TestLoginSessionCapacityAndExpiry(t *testing.T) {
	f := newFixture(t, "success")
	for i := range maxSessions {
		f.provider.sessions[fmt.Sprint(i)] = &loginSession{expires: f.provider.now().Add(loginLifetime), client: &DeviceClient{HTTP: http.DefaultClient}}
	}
	if _, err := f.provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{Metadata: map[string]any{"method": "device"}}); err == nil {
		t.Fatal("unbounded login session allocation")
	}
	f.advance(loginLifetime)
	if _, err := f.provider.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{Metadata: map[string]any{"method": "device"}}); err != nil {
		t.Fatalf("expired sessions were not released: %v", err)
	}
}
