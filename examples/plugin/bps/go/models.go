package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const bpsModelCatalogURL = "https://bps.openai.com/basispoints/api/responses/access?include_models=true"

type bpsModelRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type bpsModelCacheEntry struct {
	ready    chan struct{}
	response pluginapi.ModelResponse
	err      error
	expires  time.Time
}

type bpsModelCache struct {
	mu      sync.Mutex
	entries map[[32]byte]*bpsModelCacheEntry
	now     func() time.Time
}

var bpsModels = &bpsModelCache{entries: make(map[[32]byte]*bpsModelCacheEntry), now: time.Now}

// Short-lived cache coalesces the host's registration reconciliation calls.
// Tokens exist only inside an irreversible key and are never persisted/logged.
func (c *bpsModelCache) discover(req bpsModelRequest, call func(string, []byte) ([]byte, error)) (pluginapi.ModelResponse, error) {
	identity, errMarshal := json.Marshal(struct {
		ID, Provider string
		Metadata     map[string]any
		Storage      []byte
		Attributes   map[string]string
		Proxy        string
	}{req.AuthID, req.AuthProvider, req.Metadata, req.StorageJSON, req.Attributes, req.Host.ProxyURL})
	if errMarshal != nil {
		return pluginapi.ModelResponse{}, errors.New("invalid BPS discovery identity")
	}
	key := sha256.Sum256(identity)
	c.mu.Lock()
	if entry := c.entries[key]; entry != nil {
		select {
		case <-entry.ready:
			if c.now().Before(entry.expires) {
				c.mu.Unlock()
				return entry.response, entry.err
			}
		default:
			c.mu.Unlock()
			<-entry.ready
			return entry.response, entry.err
		}
	}
	for id, entry := range c.entries {
		select {
		case <-entry.ready:
			if !c.now().Before(entry.expires) {
				delete(c.entries, id)
			}
		default:
		}
	}
	entry := &bpsModelCacheEntry{ready: make(chan struct{})}
	if len(c.entries) < 256 {
		c.entries[key] = entry
	}
	c.mu.Unlock()
	entry.response, entry.err = fetchBPSModels(req, call)
	c.mu.Lock()
	entry.expires = c.now().Add(time.Minute)
	close(entry.ready)
	c.mu.Unlock()
	return entry.response, entry.err
}

// Discovery uses the same authenticated access endpoint as bps-local. The host
// retains its last published per-auth catalog when this method returns an error.
// An authoritative denial or an explicitly empty catalog instead revokes models.
func fetchBPSModels(req bpsModelRequest, call func(string, []byte) ([]byte, error)) (pluginapi.ModelResponse, error) {
	empty := pluginapi.ModelResponse{Provider: "bps"}
	if req.AuthProvider != "bps" {
		return empty, nil
	}
	headers, errHeaders := bpsQuotaHeaders(quotaFetchRequest{
		QuotaFetchRequest: pluginapi.QuotaFetchRequest{Provider: req.AuthProvider, Metadata: req.Metadata, StorageJSON: req.StorageJSON},
		HostCallbackID:    req.HostCallbackID,
	})
	if errHeaders != nil {
		return empty, errors.New("BPS model discovery requires a credential and host callback")
	}
	headers.Set("X-Openai-Account-Id", headers.Get("ChatGPT-Account-Id"))
	headers.Set("X-Basispoints-Auth-Mode", "chatgpt")
	headers.Set("Origin", "https://bps.openai.com")
	platform := "PC"
	if runtime.GOOS == "darwin" {
		platform = "Mac"
	}
	for key, value := range map[string]string{
		"Client-Product": "basispoints-excel-plugin", "Client-Agent-Profile": "excel",
		"Client-Platform": "excel", "Client-Editor": "excel", "Client-Host": "office",
		"Client-Runtime": "desktop", "Client-Platform-Class": platform,
		"Office-Host": "Excel", "Office-Platform": platform, "Browser-Name": "unknown",
	} {
		headers.Set("X-Openai-Internal-Basispoints-"+key, value)
	}
	raw, errMarshal := json.Marshal(quotaHTTPRequest{HostCallbackID: req.HostCallbackID, Method: http.MethodGet, URL: bpsModelCatalogURL, Headers: headers})
	if errMarshal != nil {
		return empty, errors.New("cannot construct BPS model discovery request")
	}
	response, errCall := call(pluginabi.MethodHostHTTPDo, raw)
	if errCall != nil {
		return empty, errors.New("BPS model discovery transport failed")
	}
	var envelope pluginabi.Envelope
	var upstream pluginapi.HTTPResponse
	if json.Unmarshal(response, &envelope) != nil || !envelope.OK || json.Unmarshal(envelope.Result, &upstream) != nil {
		return empty, errors.New("invalid BPS model discovery host response")
	}
	switch upstream.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return empty, nil
	case http.StatusOK:
		return parseBPSModelCatalog(upstream.Body)
	default:
		return empty, fmt.Errorf("BPS model discovery failed (HTTP %d)", upstream.StatusCode)
	}
}

func parseBPSModelCatalog(raw []byte) (pluginapi.ModelResponse, error) {
	result := pluginapi.ModelResponse{Provider: "bps"}
	var access struct {
		Allowed *bool `json:"allowed"`
		Catalog *struct {
			Models *[]struct {
				ID          string `json:"id"`
				Label       string `json:"label"`
				Description string `json:"description"`
				Efforts     []struct {
					Value string `json:"value"`
				} `json:"efforts"`
			} `json:"models"`
			Restricted []string `json:"restricted_models"`
		} `json:"model_catalog"`
	}
	invalid := errors.New("invalid BPS model catalog; retaining previously discovered models")
	if len(raw) > 1<<20 || json.Unmarshal(raw, &access) != nil || access.Allowed == nil {
		return result, invalid
	}
	if !*access.Allowed {
		return result, nil
	}
	if access.Catalog == nil || access.Catalog.Models == nil || len(*access.Catalog.Models) > 512 {
		return result, invalid
	}
	restricted := make(map[string]bool, len(access.Catalog.Restricted))
	for _, id := range access.Catalog.Restricted {
		restricted[id] = true
	}
	seen := make(map[string]bool)
	for _, model := range *access.Catalog.Models {
		if model.ID == "" || len(model.ID) > 256 || strings.IndexFunc(model.ID, unicode.IsSpace) >= 0 || strings.IndexFunc(model.ID, unicode.IsControl) >= 0 || seen[model.ID] {
			return pluginapi.ModelResponse{Provider: "bps"}, invalid
		}
		seen[model.ID] = true
		if restricted[model.ID] {
			continue
		}
		name := strings.TrimSpace(model.Label)
		if name == "" {
			name = model.ID
		}
		info := pluginapi.ModelInfo{ID: model.ID, Object: "model", OwnedBy: "bps", Type: "bps", Name: model.ID, DisplayName: name, Description: model.Description,
			SupportedInputModalities: []string{"text", "image"}, SupportedOutputModalities: []string{"text"}}
		levels := make([]string, 0, len(model.Efforts))
		seenEfforts := make(map[string]bool)
		for _, effort := range model.Efforts {
			level := strings.TrimSpace(effort.Value)
			if level == "" || seenEfforts[level] {
				continue
			}
			seenEfforts[level] = true
			levels = append(levels, level)
		}
		if len(levels) > 0 {
			info.Thinking = &pluginapi.ThinkingSupport{Levels: levels, ZeroAllowed: seenEfforts["none"]}
		}
		result.Models = append(result.Models, info)
	}
	return result, nil
}
