package cliproxy

import (
	"context"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type bpsQuotaFixture struct {
	fetch func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error)
}

func (p *bpsQuotaFixture) Identifier() string { return "bps" }
func (p *bpsQuotaFixture) DescribeQuota(context.Context, pluginapi.QuotaDescribeRequest) (pluginapi.QuotaDescribeResponse, error) {
	return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{"bps"}}, nil
}
func (p *bpsQuotaFixture) FetchQuota(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	return p.fetch(ctx, req)
}
func (p *bpsQuotaFixture) ResetQuota(context.Context, pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
	panic("automatic refresh must never redeem credits")
}

func TestBPSQuotaRefreshUsesActiveAccountsWithoutPanel(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	host := pluginhost.New()
	host.SetAuthManager(manager)
	service := &Service{coreManager: manager, pluginHost: host}
	var mu sync.Mutex
	calls := make(map[string]int)
	provider := &bpsQuotaFixture{fetch: func(_ context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		if req.Provider != "bps" || req.AuthIndex == "" || req.HTTPClient == nil {
			t.Error("missing account or host transport context")
		}
		mu.Lock()
		calls[req.AuthID]++
		mu.Unlock()
		return pluginapi.QuotaFetchResponse{}, nil
	}}
	host.RegisterPluginForTest("bps", pluginapi.Plugin{Capabilities: pluginapi.Capabilities{QuotaProvider: provider}})
	for _, auth := range []*coreauth.Auth{{ID: "active", Provider: "bps"}, {ID: "disabled", Provider: "bps", Disabled: true}, {ID: "other", Provider: "codex"}} {
		if _, err := manager.Register(t.Context(), auth); err != nil {
			t.Fatal(err)
		}
	}
	service.refreshBPSQuotas(t.Context())
	if calls["active"] != 1 || len(calls) != 1 {
		t.Fatalf("queried wrong accounts: %v", calls)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	service.refreshBPSQuotas(ctx)
	if calls["active"] != 1 {
		t.Fatal("canceled refresh queried upstream")
	}
}

func TestBPSQuotaRefreshStopsAndDoesNotStartTwice(t *testing.T) {
	service := &Service{coreManager: coreauth.NewManager(nil, nil, nil), pluginHost: pluginhost.New()}
	ctx, cancel := context.WithCancel(t.Context())
	service.startBPSQuotaRefresh(ctx)
	first := service.bpsQuotas
	service.startBPSQuotaRefresh(ctx)
	if first == nil || first != service.bpsQuotas {
		t.Fatal("duplicate quota poller")
	}
	cancel()
	<-first.done
}
