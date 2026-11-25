//go:build cgo && (linux || darwin || freebsd)

package pluginhost

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/json"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestBPSSharedLibraryImportsAndPersistsIndependentGrant(t *testing.T) {
	if testing.Short() {
		t.Skip("shared library build")
	}
	dir := t.TempDir()
	pluginPath := filepath.Join(dir, "bps"+testPluginLibraryExtension())
	build := exec.Command("go", "build", "-buildmode=c-shared", "-o", pluginPath, "./examples/plugin/bps/go")
	build.Dir = filepath.Join("..", "..")
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if output, errBuild := build.CombinedOutput(); errBuild != nil {
		t.Fatalf("build BPS plugin: %v\n%s", errBuild, output)
	}
	t.Chdir(dir)
	host := New()
	client, errOpen := (dynamicLibraryLoader{}).Open(pluginFile{ID: "bps", Path: pluginPath, Version: "0.5.5"}, host)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	t.Cleanup(client.Shutdown)
	plugin, errRegister := registerRPCPlugin(context.Background(), host, "bps", client, pluginabi.MethodPluginRegister, nil)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	fields := plugin.Metadata.ConfigFields
	if plugin.Metadata.Version != "0.5.5" || len(fields) != 3 || fields[0].Name != "cache-creation-as-input" || fields[0].Type != pluginapi.ConfigFieldTypeBoolean || fields[0].Default != false || fields[1].Name != "quota-model-prices" || fields[1].Type != pluginapi.ConfigFieldTypeObject || fields[2].Name != "image-relay-origin" || fields[2].Type != pluginapi.ConfigFieldTypeString {
		t.Fatal("BPS downstream accounting configuration unavailable across C ABI")
	}
	if plugin.Capabilities.AuthProvider == nil || plugin.Capabilities.AuthProvider.Identifier() != "bps" || plugin.Capabilities.CommandLinePlugin == nil {
		t.Fatal("BPS capabilities unavailable across C ABI")
	}
	host.RegisterPluginForTest("bps", plugin)
	if plugin.Capabilities.QuotaProvider == nil || !host.HasQuotaProvider("bps") || plugin.Capabilities.UsagePlugin == nil {
		t.Fatal("BPS quota capability unavailable across C ABI")
	}
	description, _, errDescribe := host.DescribeQuota(context.Background(), "bps")
	if errDescribe != nil || !description.SupportsReset || len(description.SupportedProviders) != 1 || description.SupportedProviders[0] != "bps" {
		t.Fatalf("invalid BPS quota capabilities: %+v, %v", description, errDescribe)
	}
	quotaCalls := 0
	quotaBody := `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":15,"limit_window_seconds":18000}},"credits":{"balance":"20"}}`
	quotaCtx := context.WithValue(context.Background(), "cliproxy.roundtripper", bpsQuotaRoundTripper(func(req *http.Request) (*http.Response, error) {
		quotaCalls++
		if req.Method != http.MethodGet || req.Header.Get("Authorization") != "Bearer quota-test" || req.Header.Get("ChatGPT-Account-Id") != "quota-account" {
			t.Error("BPS quota callback lost credential scope")
		}
		body := quotaBody
		switch req.URL.String() {
		case "https://chatgpt.com/backend-api/wham/usage":
		case "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits":
			body = `{"available_count":2}`
		default:
			t.Errorf("unexpected BPS quota URL: %s", req.URL)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	quota, handledQuota, errQuota := host.FetchQuota(quotaCtx, pluginapi.QuotaFetchRequest{Provider: "bps", Metadata: map[string]any{"access_token": "quota-test", "account_id": "quota-account"}})
	if errQuota != nil || !handledQuota || quotaCalls != 2 || len(quota.Groups) != 1 || quota.Groups[0].Buckets[0].RemainingFraction != 0.85 || len(quota.Summary) != 2 || quota.Summary[0].Value != 20 || quota.Summary[1].Key != "rate_limit_reset_credits_available_count" || quota.Summary[1].Value != 2 {
		t.Fatalf("BPS quota callback failed across C ABI: %+v, %v, calls=%d", quota, errQuota, quotaCalls)
	}
	resetCalls := 0
	resetCtx := context.WithValue(context.Background(), "cliproxy.roundtripper", bpsQuotaRoundTripper(func(req *http.Request) (*http.Response, error) {
		resetCalls++
		if req.Header.Get("Authorization") != "Bearer quota-test" || req.Header.Get("ChatGPT-Account-Id") != "quota-account" {
			t.Error("BPS reset callback lost credential scope")
		}
		body := `{"available_count":2}`
		switch resetCalls {
		case 1:
			if req.Method != http.MethodGet || req.URL.String() != "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits" {
				t.Error("BPS reset did not recheck available credits")
			}
		case 2:
			var redemption struct {
				ID string `json:"redeem_request_id"`
			}
			raw, errRead := io.ReadAll(req.Body)
			if errRead != nil || json.Unmarshal(raw, &redemption) != nil || redemption.ID == "" || req.Method != http.MethodPost || req.URL.String() != "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume" {
				t.Error("BPS reset body did not survive the C ABI")
			}
			body = `{"code":"reset"}`
		default:
			t.Error("BPS reset retried redemption")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	resetResult, handledReset, errReset := host.ResetQuota(resetCtx, pluginapi.QuotaResetRequest{Provider: "bps", Metadata: map[string]any{"access_token": "quota-test", "account_id": "quota-account"}})
	if errReset != nil || !handledReset || !resetResult.Success || resetCalls != 2 {
		t.Fatalf("BPS reset failed across C ABI: %+v, %v, calls=%d", resetResult, errReset, resetCalls)
	}
	_, errConfigure := registerRPCPlugin(context.Background(), host, "bps", client, pluginabi.MethodPluginReconfigure, []byte("quota-model-prices:\n  gpt-6-astra:\n    input: 10\n    output: 50\n"))
	if errConfigure != nil {
		t.Fatal(errConfigure)
	}
	reset := time.Now().Add(24 * time.Hour).Unix()
	quotaBody = fmt.Sprintf(`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":15,"limit_window_seconds":18000,"reset_at":%d}},"credits":{"balance":"20"}}`, reset)
	quotaRequest := pluginapi.QuotaFetchRequest{Provider: "bps", AuthIndex: "quota-index", Metadata: map[string]any{"access_token": "quota-test", "account_id": "quota-account"}}
	baseline, _, errBaseline := host.FetchQuota(quotaCtx, quotaRequest)
	if errBaseline != nil || len(baseline.Summary) != 2 || !strings.Contains(baseline.Groups[0].Buckets[0].Description, "Collecting") {
		t.Fatalf("BPS USD baseline failed across C ABI: %+v, %v", baseline, errBaseline)
	}
	_, errUsage := callPlugin[rpcEmptyResponse](context.Background(), client, pluginabi.MethodUsageHandle, pluginapi.UsageRecord{Provider: "bps", Model: "gpt-6-astra", AuthIndex: "quota-index", Generate: true, RequestID: "quota-execution", Detail: pluginapi.UsageDetail{InputTokens: 100000, OutputTokens: 10000}})
	if errUsage != nil {
		t.Fatal(errUsage)
	}
	quotaBody = strings.Replace(quotaBody, `"used_percent":15`, `"used_percent":16`, 1)
	estimate, _, errEstimate := host.FetchQuota(quotaCtx, quotaRequest)
	if errEstimate != nil || len(estimate.Summary) != 4 || estimate.Summary[0].Unit != "credits" || estimate.Summary[2].Currency != "USD" || math.Abs(estimate.Summary[2].Value-126) > 1e-8 || math.Abs(estimate.Summary[3].Value-150) > 1e-8 {
		t.Fatalf("BPS USD estimate failed across C ABI: %+v, %v", estimate, errEstimate)
	}
	if _, errReload := registerRPCPlugin(context.Background(), host, "bps", client, pluginabi.MethodPluginRegister, []byte("quota-model-prices:\n  gpt-6-astra:\n    input: 10\n    output: 50\n")); errReload != nil {
		t.Fatal(errReload)
	}
	restored, _, errRestored := host.FetchQuota(quotaCtx, quotaRequest)
	if errRestored != nil || len(restored.Summary) != 4 || math.Abs(restored.Summary[2].Value-126) > 1e-8 {
		t.Fatalf("BPS USD estimate did not persist across registration: %+v, %v", restored, errRestored)
	}
	if plugin.Capabilities.ModelProvider == nil {
		t.Fatal("BPS model provider unavailable across C ABI")
	}
	models, errModels := plugin.Capabilities.ModelProvider.ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{AuthProvider: "bps"})
	if errModels != nil || models.Provider != "bps" || len(models.Models) != 4 || models.Models[0].ID != "gpt-6-astra" {
		t.Fatalf("BPS credential model discovery failed: %v, models=%v", errModels, models.Models)
	}
	start, handledStart, errStart := host.StartLogin(context.Background(), "bps", "http://127.0.0.1:8317/v8/management/oauth/callback")
	if errStart != nil || !handledStart || start.UserCode != "" {
		t.Fatalf("default browser login through ABI: handled=%t error=%v", handledStart, errStart)
	}
	authorize, errURL := url.Parse(start.URL)
	if errURL != nil || authorize.Host != "auth.openai.com" || authorize.Path != "/oauth/authorize" || authorize.Query().Get("state") != start.State || authorize.Query().Get("code_challenge") == "" {
		t.Fatal("browser login URL unavailable through C ABI")
	}
	pending, handledPoll, errPoll := host.PollLogin(context.Background(), "bps", start.State)
	if errPoll != nil || !handledPoll || pending.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("pending browser login through ABI: handled=%t error=%v", handledPoll, errPoll)
	}
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "sub": "fixture-user",
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "fixture-account"}})
	token := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".test"
	raw, _ := json.Marshal(map[string]any{"client_id": "app_fnr0pYvVwwFDocDumLG3H2Bp", "access_token": token,
		"refresh_token": "fixture-refresh", "disabled": true, "prefix": "bps", "weight": 3})
	inputPath := filepath.Join(dir, "oauth.json")
	if errWrite := os.WriteFile(inputPath, raw, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	response, errCommand := plugin.Capabilities.CommandLinePlugin.ExecuteCommandLine(context.Background(), pluginapi.CommandLineExecutionRequest{
		Flags: map[string]pluginapi.CommandLineFlagValue{"bps-import": {Value: inputPath}},
	})
	if errCommand != nil || len(response.Auths) != 1 {
		t.Fatalf("CLI import through ABI: %v, count=%d", errCommand, len(response.Auths))
	}
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(filepath.Join(dir, "auths"))
	originalStore := sdkauth.GetTokenStore()
	sdkauth.RegisterTokenStore(store)
	t.Cleanup(func() { sdkauth.RegisterTokenStore(originalStore) })
	paths, errSave := host.persistCommandLineAuths(context.Background(), response.Auths)
	if errSave != nil {
		t.Fatal(errSave)
	}
	if len(paths) != 1 {
		t.Fatal("CLI import did not create the disabled credential")
	}
	path := paths[0]
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatal(errStat)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatal("credential not saved with private permissions")
	}
	saved, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	parsed, handled, errParse := host.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		Provider: "bps", RawJSON: saved, Path: path, FileName: filepath.Base(path),
	})
	if errParse != nil || !handled || parsed == nil || parsed.Metadata["refresh_token"] != "fixture-refresh" || parsed.Prefix != "bps" || !parsed.Disabled {
		t.Fatalf("credential reload failed: handled=%t error=%v", handled, errParse)
	}
}

type bpsQuotaRoundTripper func(*http.Request) (*http.Response, error)

func (f bpsQuotaRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
