//go:build cgo && (linux || darwin || freebsd)

package pluginhost

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
	host := New()
	client, errOpen := (dynamicLibraryLoader{}).Open(pluginFile{ID: "bps", Path: pluginPath, Version: "0.3.0"}, host)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	t.Cleanup(client.Shutdown)
	plugin, errRegister := registerRPCPlugin(context.Background(), host, "bps", client, pluginabi.MethodPluginRegister, nil)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	if plugin.Capabilities.AuthProvider == nil || plugin.Capabilities.AuthProvider.Identifier() != "bps" || plugin.Capabilities.CommandLinePlugin == nil {
		t.Fatal("BPS capabilities unavailable across C ABI")
	}
	host.RegisterPluginForTest("bps", plugin)
	if plugin.Capabilities.ModelProvider == nil {
		t.Fatal("BPS model provider unavailable across C ABI")
	}
	models, errModels := plugin.Capabilities.ModelProvider.ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{AuthProvider: "bps"})
	if errModels != nil || models.Provider != "bps" || len(models.Models) != 4 || models.Models[0].ID != "bps-gpt-6-astra" {
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
