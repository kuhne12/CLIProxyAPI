package management

import (
	"context"
	"fmt"
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

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestV8PluginBrowserLoginUsesExistingCallbackAndPersistence(t *testing.T) {
	const state = "bps-browser-callback-test"
	const callbackCode = "fixture-browser-code"
	authDir := t.TempDir()
	callbackPath := filepath.Join(authDir, ".oauth-bps-"+state+".oauth")
	host := pluginhost.New()
	host.RegisterPluginForTest("bps", pluginapi.Plugin{Capabilities: pluginapi.Capabilities{AuthProvider: &testAuthProvider{
		identifier: "bps",
		startLogin: func(_ context.Context, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
			if req.Metadata["method"] != "browser" {
				t.Error("browser method not passed to plugin")
			}
			return pluginapi.AuthLoginStartResponse{Provider: "bps", State: state, URL: "https://auth.openai.com/oauth/authorize?state=" + state, Interval: 2}, nil
		},
		pollLogin: func(context.Context, pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
			raw, errRead := os.ReadFile(callbackPath)
			if os.IsNotExist(errRead) {
				return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
			}
			var callback oauthCallbackFilePayload
			if errRead != nil || json.Unmarshal(raw, &callback) != nil || callback.Code != callbackCode || callback.State != state || callback.Error != "" {
				t.Fatal("submitted callback was not delivered to plugin")
			}
			return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Auth: pluginapi.AuthData{
				Provider: "bps", ID: "bps-browser.json", FileName: "bps-browser.json", StorageJSON: []byte(`{"type":"bps","access_token":"fixture-token"}`),
			}}, nil
		},
	}}})
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir, Port: 8317}, nil)
	handler.tokenStore = store
	handler.SetPluginHost(host)
	t.Cleanup(func() { oauthSessions.mu.Lock(); delete(oauthSessions.sessions, state); oauthSessions.mu.Unlock() })
	router := gin.New()
	router.GET("/v8/management/oauth/auth-url", handler.StartOAuthV8)
	router.POST("/v8/management/oauth/callback", handler.PostOAuthCallbackV8)
	router.GET("/v8/management/oauth/status", handler.GetAuthStatus)
	start := httptest.NewRecorder()
	router.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=bps&method=browser", nil))
	if start.Code != http.StatusOK || strings.Contains(start.Body.String(), "user_code") {
		t.Fatal("browser login unexpectedly requires a device code")
	}
	pending := httptest.NewRecorder()
	router.ServeHTTP(pending, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/status?state="+state, nil))
	if pending.Body.String() != `{"status":"wait"}` {
		t.Fatal("browser flow did not wait for callback")
	}
	body, _ := json.Marshal(map[string]string{"provider": "bps", "redirect_url": "https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback?" + url.Values{"code": {callbackCode}, "state": {state}}.Encode()})
	callback := httptest.NewRecorder()
	router.ServeHTTP(callback, httptest.NewRequest(http.MethodPost, "/v8/management/oauth/callback", strings.NewReader(string(body))))
	if callback.Code != http.StatusOK {
		t.Fatalf("callback rejected: %d", callback.Code)
	}
	for range 2 {
		result := httptest.NewRecorder()
		router.ServeHTTP(result, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/status?state="+state, nil))
		if result.Body.String() != `{"status":"ok"}` {
			t.Fatalf("browser login did not save: %s", result.Body.String())
		}
	}
	if _, errStat := os.Stat(filepath.Join(authDir, "bps-browser.json")); errStat != nil {
		t.Fatal("browser credential not persisted")
	}
}

func TestV8PluginDeviceLoginPublishesOnlyPublicFields(t *testing.T) {
	host := pluginhost.New()
	state := "device-public-fields-test"
	t.Cleanup(func() { CancelOAuthSession(state) })
	host.RegisterPluginForTest("bps", pluginapi.Plugin{Capabilities: pluginapi.Capabilities{AuthProvider: &testAuthProvider{
		identifier: "bps",
		startLogin: func(context.Context, pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
			return pluginapi.AuthLoginStartResponse{State: state, Provider: "bps", URL: "https://example.invalid/authorize",
				UserCode: "ABCD-EFGH", Interval: 5, ExpiresAt: time.Now().Add(15 * time.Minute),
				Metadata: map[string]any{"private_device_id": "never-public", "refresh_token": "never-public"}}, nil
		},
	}}})
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir(), Port: 8317}, nil)
	handler.SetPluginHost(host)
	router := gin.New()
	router.GET("/v8/management/oauth/auth-url", handler.StartOAuthV8)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=bps", nil))
	var body map[string]any
	if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != http.StatusOK {
		t.Fatalf("start failed: %d %s", response.Code, response.Body.String())
	}
	if body["user_code"] != "ABCD-EFGH" || body["interval"] != float64(5) || body["expires_at"] == nil || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing device fields: %v", body)
	}
	if strings.Contains(response.Body.String(), "never-public") || body["Metadata"] != nil || body["metadata"] != nil {
		t.Fatal("private OAuth metadata was exposed")
	}
}

type v8LoginStore struct {
	coreauth.Store
	saves atomic.Int32
}

func (s *v8LoginStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	s.saves.Add(1)
	return s.Store.Save(ctx, auth)
}

func TestV8PluginLoginCommitCancelsOrSavesExactlyOnce(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancel), func(t *testing.T) {
			state := fmt.Sprintf("v8-plugin-commit-%t", cancel)
			if err := RegisterPluginOAuthSession(state, "bps", nil); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { oauthSessions.mu.Lock(); delete(oauthSessions.sessions, state); oauthSessions.mu.Unlock() })
			started := make(chan struct{}, 2)
			release := make(chan struct{})
			host := pluginhost.New()
			host.RegisterPluginForTest("bps", pluginapi.Plugin{Capabilities: pluginapi.Capabilities{AuthProvider: &testAuthProvider{
				identifier: "bps",
				pollLogin: func(context.Context, pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
					started <- struct{}{}
					<-release
					return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Auth: pluginapi.AuthData{
						Provider: "bps", ID: "bps-test.json", FileName: "bps-test.json", StorageJSON: []byte(`{"type":"bps","access_token":"test"}`),
					}}, nil
				},
			}}})
			authDir := t.TempDir()
			fileStore := sdkauth.NewFileTokenStore()
			fileStore.SetBaseDir(authDir)
			store := &v8LoginStore{Store: fileStore}
			handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
			handler.tokenStore = store
			handler.SetPluginHost(host)
			router := gin.New()
			router.GET("/v8/management/oauth/status", handler.GetAuthStatus)
			var wait sync.WaitGroup
			for range 2 {
				wait.Go(func() {
					response := httptest.NewRecorder()
					router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/status?state="+state, nil))
					if response.Code != http.StatusOK {
						t.Errorf("poll failed: %d", response.Code)
					}
				})
			}
			<-started
			<-started
			if cancel && !CancelOAuthSession(state) {
				t.Fatal("pending authorization could not be cancelled")
			}
			close(release)
			wait.Wait()
			want := int32(1)
			if cancel {
				want = 0
			}
			if store.saves.Load() != want {
				t.Fatalf("credential saves=%d want=%d", store.saves.Load(), want)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/status?state="+state, nil))
			if !cancel && response.Body.String() != `{"status":"ok"}` {
				t.Fatalf("completed state not retained: %s", response.Body.String())
			}
		})
	}
}
