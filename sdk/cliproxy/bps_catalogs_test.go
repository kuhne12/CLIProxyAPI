package cliproxy

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type bpsDiscoveryFixture struct {
	*issue6718ModelProvider
	discover func(context.Context, pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error)
}

func (p *bpsDiscoveryFixture) ModelsForAuth(ctx context.Context, req pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	return p.discover(ctx, req)
}

func TestBPSCatalogRefreshPreservesFailureAndSynchronizesAccounts(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	host := pluginhost.New()
	service := &Service{cfg: &config.Config{}, coreManager: manager, pluginHost: host}
	var mu sync.Mutex
	calls := map[string]int{}
	models := map[string][]string{"first": {"gpt-5.6-sol", "old-model"}, "second": {"gpt-6.1-sol"}}
	fail := true
	provider := &bpsDiscoveryFixture{issue6718ModelProvider: &issue6718ModelProvider{provider: "bps"}}
	provider.discover = func(_ context.Context, req pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		calls[req.AuthID]++
		if fail {
			return pluginapi.ModelResponse{}, errors.New("temporary discovery failure")
		}
		result := pluginapi.ModelResponse{Provider: "bps"}
		for _, id := range models[req.Metadata["test_account"].(string)] {
			result.Models = append(result.Models, pluginapi.ModelInfo{ID: id})
		}
		return result, nil
	}
	host.RegisterPluginForTest("bps", pluginapi.Plugin{Capabilities: pluginapi.Capabilities{AuthProvider: provider, ModelProvider: provider}})
	ids := map[string]string{}
	for _, name := range []string{"first", "second", "disabled"} {
		auth := &coreauth.Auth{ID: t.Name() + name, Provider: "bps", Disabled: name == "disabled", Metadata: map[string]any{"test_account": name}}
		if _, err := manager.Register(t.Context(), auth); err != nil {
			t.Fatal(err)
		}
		ids[name] = auth.ID
		t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
	}
	check := func(name string, want ...string) {
		t.Helper()
		actual := GlobalModelRegistry().GetModelsForClient(ids[name])
		if len(actual) != len(want) {
			t.Fatalf("%s: models=%v want=%v", name, actual, want)
		}
		for _, id := range want {
			if !GlobalModelRegistry().ClientSupportsModel(ids[name], id) {
				t.Fatalf("%s missing %s", name, id)
			}
		}
	}
	service.refreshBPSCatalogs(t.Context())
	check("first")
	check("second")
	fail = false
	service.refreshBPSCatalogs(t.Context())
	check("first", "gpt-5.6-sol", "old-model")
	check("second", "gpt-6.1-sol")
	check("disabled")
	if calls[ids["disabled"]] != 0 {
		t.Fatal("disabled credential was queried")
	}
	fail = true
	service.refreshBPSCatalogs(t.Context())
	check("first", "gpt-5.6-sol", "old-model")
	check("second", "gpt-6.1-sol")
	fail = false
	models["first"] = []string{"gpt-5.6-sol", "new-model"}
	models["second"] = nil
	service.refreshBPSCatalogs(t.Context())
	check("first", "gpt-5.6-sol", "new-model")
	check("second")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := calls[ids["first"]]
	service.refreshBPSCatalogs(ctx)
	if calls[ids["first"]] != before {
		t.Fatal("canceled refresh reached upstream")
	}
}

func TestBPSCatalogRefreshStopsWithService(t *testing.T) {
	service := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	ctx, cancel := context.WithCancel(context.Background())
	service.startBPSCatalogRefresh(ctx)
	first := service.bpsCatalogs
	service.startBPSCatalogRefresh(ctx)
	if first == nil || first != service.bpsCatalogs {
		t.Fatal("duplicate catalog refresher started")
	}
	cancel()
	<-first.done
}
