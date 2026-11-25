package bpsauth

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestBrowserLoginDefaultUsesPKCEAndExchangesOnce(t *testing.T) {
	for _, mode := range []string{"callback_url", "host_callback_file"} {
		t.Run(mode, func(t *testing.T) {
			p := New()
			defer p.Close()
			var exchanges atomic.Int32
			var challenge string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				exchanges.Add(1)
				_ = r.ParseForm()
				verifier := r.Form.Get("code_verifier")
				digest := sha256.Sum256([]byte(verifier))
				if r.URL.Path != "/oauth/token" || r.Method != http.MethodPost || r.URL.Query().Get("unified") != "true" ||
					r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("client_id") != ExcelClientID ||
					r.Form.Get("redirect_uri") != BrowserRedirect || r.Form.Get("code") != "fixture-code" ||
					len(verifier) < 43 || base64.RawURLEncoding.EncodeToString(digest[:]) != challenge {
					t.Error("unexpected OAuth exchange or PKCE parameters")
				}
				_ = json.NewEncoder(w).Encode(DeviceCredentials{
					AccessToken:  testToken("account", "user", ExcelClientID, time.Now().Add(time.Hour)),
					RefreshToken: "fixture-refresh", TokenType: "Bearer", ExpiresIn: 3600,
				})
			}))
			defer server.Close()
			p.newClient = func(proxy string) (*DeviceClient, error) {
				if proxy != "direct" {
					t.Error("session proxy not passed to browser exchange client")
				}
				return &DeviceClient{HTTP: server.Client(), base: server.URL, now: time.Now}, nil
			}
			host := pluginapi.HostConfigSummary{AuthDir: t.TempDir(), ProxyURL: "direct"}
			start, errStart := p.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{Host: host})
			if errStart != nil {
				t.Fatal(errStart)
			}
			u, _ := url.Parse(start.URL)
			query := u.Query()
			challenge = query.Get("code_challenge")
			if u.Scheme != "https" || u.Host != "auth.openai.com" || u.Path != "/oauth/authorize" ||
				query.Get("state") != start.State || query.Get("client_id") != ExcelClientID ||
				query.Get("redirect_uri") != BrowserRedirect || query.Get("code_challenge_method") != "S256" ||
				query.Get("scope") != "openid offline_access email profile organization.read" ||
				query.Get("response_type") != "code" || challenge == "" || start.UserCode != "" || start.Interval != 2 {
				t.Fatal("invalid browser authorization response")
			}
			raw, _ := json.Marshal(start)
			if strings.Contains(string(raw), p.sessions[start.State].browser.verifier) {
				t.Fatal("private verifier escaped in the start response")
			}
			req := pluginapi.AuthLoginPollRequest{State: start.State, Host: host}
			pending, errPoll := p.PollLogin(context.Background(), req)
			if errPoll != nil || pending.Status != pluginapi.AuthLoginStatusPending || exchanges.Load() != 0 {
				t.Fatal("browser start or pending poll contacted device/token endpoints")
			}
			second, errSecond := p.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{Host: host, Metadata: map[string]any{"method": "browser"}})
			if errSecond != nil || second.State == start.State || strings.Contains(second.URL, url.QueryEscape(challenge)) {
				t.Fatal("browser login reused state or PKCE challenge")
			}
			callbackPath := filepath.Join(host.AuthDir, ".oauth-bps-"+start.State+".oauth")
			if mode == "callback_url" {
				req.Metadata = map[string]any{"redirect_url": BrowserRedirect + "?" + url.Values{"state": {start.State}, "code": {"fixture-code"}}.Encode()}
			} else {
				payload, _ := json.Marshal(map[string]string{"state": start.State, "code": "fixture-code"})
				if errWrite := os.WriteFile(callbackPath, payload, 0o600); errWrite != nil {
					t.Fatal(errWrite)
				}
			}
			var wait sync.WaitGroup
			for range 8 {
				wait.Go(func() {
					result, errPoll := p.PollLogin(context.Background(), req)
					if errPoll != nil || result.Status != pluginapi.AuthLoginStatusSuccess || result.Auth.Provider != "bps" ||
						result.Auth.Metadata["refresh_token"] != "fixture-refresh" || result.Auth.ProxyURL != "direct" {
						t.Errorf("browser completion failed: %v", errPoll)
					}
				})
			}
			wait.Wait()
			if exchanges.Load() != 1 || p.sessions[start.State].browser.verifier != "" {
				t.Fatal("exchange duplicated or verifier retained")
			}
			if _, errStat := os.Stat(callbackPath); !os.IsNotExist(errStat) {
				t.Fatal("consumed callback file retained")
			}
		})
	}
}

func TestBrowserLoginRejectsInvalidCallbackBeforeExchange(t *testing.T) {
	for _, mode := range []string{"host", "http", "path", "encoded_path", "userinfo", "fragment", "duplicate_code", "duplicate_state", "state", "missing_code", "error", "bad_query", "oversized", "file_state", "file_error", "expired", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			p := New()
			defer p.Close()
			var exchanges atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { exchanges.Add(1); w.WriteHeader(500) }))
			defer server.Close()
			p.newClient = func(string) (*DeviceClient, error) {
				return &DeviceClient{HTTP: server.Client(), base: server.URL, now: time.Now}, nil
			}
			host := pluginapi.HostConfigSummary{AuthDir: t.TempDir()}
			start, errStart := p.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{Host: host})
			if errStart != nil {
				t.Fatal(errStart)
			}
			query := url.Values{"state": {start.State}, "code": {"fixture-secret"}}
			target, suffix := BrowserRedirect, ""
			switch mode {
			case "host":
				target = "https://untrusted.invalid/callback"
			case "http":
				target = strings.Replace(target, "https:", "http:", 1)
			case "path":
				target += "/"
			case "encoded_path":
				target = strings.Replace(target, "callback", "%63allback", 1)
			case "userinfo":
				target = strings.Replace(target, "https://", "https://fixture-secret@", 1)
			case "fragment":
				suffix = "#"
			case "duplicate_code":
				query.Add("code", "other")
			case "duplicate_state":
				query.Add("state", start.State)
			case "state":
				query.Set("state", "other")
			case "missing_code":
				query.Del("code")
			case "error":
				query.Set("error", "fixture-secret")
			case "bad_query":
				suffix = "&bad=%zz"
			case "oversized":
				query.Set("code", strings.Repeat("x", maxCallbackBytes))
			case "expired":
				p.now = func() time.Time { return start.ExpiresAt }
			}
			req := pluginapi.AuthLoginPollRequest{State: start.State, Host: host, Metadata: map[string]any{"redirect_url": target + "?" + query.Encode() + suffix}}
			if strings.HasPrefix(mode, "file_") {
				payload := map[string]string{"state": start.State, "code": "fixture-secret"}
				if mode == "file_state" {
					payload["state"] = "other"
				} else {
					payload["error"] = "fixture-secret"
				}
				raw, _ := json.Marshal(payload)
				if errWrite := os.WriteFile(p.sessions[start.State].browser.callback, raw, 0o600); errWrite != nil {
					t.Fatal(errWrite)
				}
				req.Metadata = nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			_, errPoll := p.PollLogin(ctx, req)
			if errPoll == nil || strings.Contains(errPoll.Error(), "fixture-secret") || exchanges.Load() != 0 {
				t.Fatalf("invalid callback reached exchange or leaked contents: %v", errPoll)
			}
		})
	}
}

func TestBrowserExchangeFailureIsNotRetried(t *testing.T) {
	for _, mode := range []string{"invalid_grant", "missing_refresh", "malformed_json"} {
		t.Run(mode, func(t *testing.T) {
			p := New()
			defer p.Close()
			var exchanges atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				exchanges.Add(1)
				switch mode {
				case "invalid_grant":
					w.WriteHeader(400)
					_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"fixture-secret"}`))
				case "missing_refresh":
					_, _ = w.Write([]byte(`{"access_token":"fixture-secret","token_type":"Bearer","expires_in":3600}`))
				default:
					_, _ = w.Write([]byte("fixture-secret"))
				}
			}))
			defer server.Close()
			p.newClient = func(string) (*DeviceClient, error) {
				return &DeviceClient{HTTP: server.Client(), base: server.URL, now: time.Now}, nil
			}
			start, errStart := p.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{})
			if errStart != nil {
				t.Fatal(errStart)
			}
			req := pluginapi.AuthLoginPollRequest{State: start.State, Metadata: map[string]any{"redirect_url": BrowserRedirect + "?code=fixture-code&state=" + start.State}}
			for range 2 {
				if _, errPoll := p.PollLogin(context.Background(), req); errPoll == nil || strings.Contains(errPoll.Error(), "fixture-secret") {
					t.Fatal("exchange failure lost or secret leaked")
				}
			}
			if exchanges.Load() != 1 || p.sessions[start.State].browser.verifier != "" {
				t.Fatal("failed exchange retried or verifier retained")
			}
		})
	}
}
