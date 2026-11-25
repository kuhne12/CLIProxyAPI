package main

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const bpsCatalogFixture = `{"allowed":true,"model_catalog":{"models":[{"id":"gpt-5.6-sol","label":"GPT-5.6 Sol","efforts":[{"value":"none"},{"value":"low"},{"value":"xhigh"}]},{"id":"gpt-6.1-sol","efforts":[{"value":"low"},{"value":"high"}]},{"id":"future-model","efforts":[{"value":"max"}]},{"id":"restricted-model"}],"restricted_models":["restricted-model"]}}`

func modelRequestFixture() bpsModelRequest {
	return bpsModelRequest{AuthModelRequest: pluginapi.AuthModelRequest{AuthID: "a", AuthProvider: "bps", Metadata: map[string]any{"access_token": "secret-token", "account_id": "account-a"}}, HostCallbackID: "callback"}
}

func catalogReply(status int, body string) []byte {
	raw, _ := okEnvelope(pluginapi.HTTPResponse{StatusCode: status, Body: []byte(body)})
	return raw
}

func TestBPSModelCatalogDiscoversUpstreamModelsAndEfforts(t *testing.T) {
	result, err := parseBPSModelCatalog([]byte(bpsCatalogFixture))
	if err != nil || result.Provider != "bps" || len(result.Models) != 3 {
		t.Fatalf("catalog = %+v, %v", result, err)
	}
	if result.Models[0].ID != "gpt-5.6-sol" || result.Models[1].ID != "gpt-6.1-sol" || result.Models[2].ID != "future-model" {
		t.Fatal("model IDs were filtered by a local list")
	}
	if result.Models[0].DisplayName != "GPT-5.6 Sol" || result.Models[1].DisplayName != "gpt-6.1-sol" || !result.Models[0].Thinking.ZeroAllowed || !reflect.DeepEqual(result.Models[0].Thinking.Levels, []string{"none", "low", "xhigh"}) {
		t.Fatal("upstream metadata was lost")
	}
	if !reflect.DeepEqual(result.Models[2].Thinking.Levels, []string{"max"}) {
		t.Fatal("future model effort was lost")
	}
}

func TestBPSModelCatalogInvalidDoesNotBecomeEmptySuccess(t *testing.T) {
	for _, raw := range []string{`{`, `{}`, `{"allowed":true}`, `{"allowed":true,"model_catalog":{}}`, `{"allowed":true,"model_catalog":{"models":null}}`, `{"allowed":true,"model_catalog":{"models":[{"id":""}]}}`, `{"allowed":true,"model_catalog":{"models":[{"id":"bad model"}]}}`, `{"allowed":true,"model_catalog":{"models":[{"id":"same"},{"id":"same"}]}}`, strings.Repeat(" ", (1<<20)+1)} {
		if _, err := parseBPSModelCatalog([]byte(raw)); err == nil {
			t.Fatal("invalid catalog accepted")
		}
	}
	for _, raw := range []string{`{"allowed":false}`, `{"allowed":true,"model_catalog":{"models":[]}}`} {
		result, err := parseBPSModelCatalog([]byte(raw))
		if err != nil || result.Provider != "bps" || len(result.Models) != 0 {
			t.Fatal("authoritative empty catalog did not revoke models", err)
		}
	}
}

func TestBPSModelDiscoveryUsesHostTransportAndSanitizesFailures(t *testing.T) {
	for _, storage := range []bool{false, true} {
		req := modelRequestFixture()
		if storage {
			req.StorageJSON, _ = json.Marshal(req.Metadata)
			req.Metadata = nil
		}
		result, err := fetchBPSModels(req, func(method string, raw []byte) ([]byte, error) {
			var request quotaHTTPRequest
			if method != pluginabi.MethodHostHTTPDo || json.Unmarshal(raw, &request) != nil || request.HostCallbackID != "callback" || request.Method != http.MethodGet || request.URL != bpsModelCatalogURL || len(request.Body) != 0 {
				t.Fatal("invalid discovery host request")
			}
			if request.Headers.Get("Authorization") != "Bearer secret-token" || request.Headers.Get("ChatGPT-Account-Id") != "account-a" || request.Headers.Get("X-Openai-Account-Id") != "account-a" || request.Headers.Get("X-Openai-Internal-Basispoints-Client-Product") != "basispoints-excel-plugin" {
				t.Fatal("credential/profile headers were lost")
			}
			return catalogReply(200, bpsCatalogFixture), nil
		})
		if err != nil || len(result.Models) != 3 {
			t.Fatal("discovery failed", err)
		}
	}
	for _, status := range []int{401, 403, 429, 500} {
		result, err := fetchBPSModels(modelRequestFixture(), func(string, []byte) ([]byte, error) { return catalogReply(status, "secret-token"), nil })
		if status == 401 || status == 403 {
			if err != nil || len(result.Models) != 0 {
				t.Fatal("denied account retained models")
			}
		} else if err == nil || strings.Contains(err.Error(), "secret-token") {
			t.Fatal("transient error was lost or leaked upstream content")
		}
	}
	_, err := fetchBPSModels(modelRequestFixture(), func(string, []byte) ([]byte, error) { return nil, errors.New("secret-token") })
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatal("transport error leaked credential")
	}
	for _, req := range []bpsModelRequest{{AuthModelRequest: pluginapi.AuthModelRequest{AuthProvider: "bps"}}, {AuthModelRequest: pluginapi.AuthModelRequest{AuthProvider: "codex"}}} {
		_, _ = fetchBPSModels(req, func(string, []byte) ([]byte, error) {
			t.Fatal("unrelated/missing credential reached upstream")
			return nil, nil
		})
	}
}

func TestBPSModelCatalogCacheIsolationExpiryAndCoalescing(t *testing.T) {
	now := time.Unix(100, 0)
	cache := &bpsModelCache{entries: make(map[[32]byte]*bpsModelCacheEntry), now: func() time.Time { return now }}
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	call := func(string, []byte) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return catalogReply(200, bpsCatalogFixture), nil
	}
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			if _, err := cache.discover(modelRequestFixture(), call); err != nil {
				t.Error(err)
			}
		})
	}
	<-started
	close(release)
	group.Wait()
	if calls.Load() != 1 {
		t.Fatal("concurrent registration was not coalesced")
	}
	now = now.Add(time.Minute)
	_, _ = cache.discover(modelRequestFixture(), call)
	if calls.Load() != 2 {
		t.Fatal("expired catalog did not refresh")
	}
	req := modelRequestFixture()
	req.Metadata["account_id"] = "other"
	_, _ = cache.discover(req, call)
	req.Metadata["access_token"] = "rotated"
	_, _ = cache.discover(req, call)
	if calls.Load() != 4 {
		t.Fatal("accounts or changed credentials shared a cached catalog")
	}
}

func TestBPSStaticCatalogHasNoHardcodedModels(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodModelStatic, nil)
	var envelope pluginabi.Envelope
	var models pluginapi.ModelResponse
	if err != nil || json.Unmarshal(raw, &envelope) != nil || json.Unmarshal(envelope.Result, &models) != nil || models.Provider != "bps" || len(models.Models) != 0 {
		t.Fatal("static catalog advertised unverified models")
	}
}
