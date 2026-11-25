package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/examples/plugin/bps/go/internal/bpsauth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestCLIImportReturnsCredentialForHostPersistence(t *testing.T) {
	claims, _ := json.Marshal(map[string]any{"sub": "user", "exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "account"}})
	token := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".test"
	raw, _ := json.Marshal(map[string]string{"client_id": bpsauth.ExcelClientID, "access_token": token, "refresh_token": "secret-refresh"})
	path := filepath.Join(t.TempDir(), "oauth.json")
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	auth := bpsauth.New()
	defer auth.Close()
	response, err := runCommand(context.Background(), auth, pluginapi.CommandLineExecutionRequest{Flags: map[string]pluginapi.CommandLineFlagValue{
		"bps-import": {Value: path},
	}}, &bytes.Buffer{})
	if err != nil || len(response.Auths) != 1 || response.Auths[0].Provider != "bps" {
		t.Fatalf("import failed: auths=%d error=%v", len(response.Auths), err)
	}
	if strings.Contains(string(response.Stdout), token) || strings.Contains(string(response.Stdout), "secret-refresh") {
		t.Fatal("tokens printed to terminal")
	}
	current, errRead := os.ReadFile(path)
	if errRead != nil || !bytes.Equal(current, raw) {
		t.Fatal("import modified the source credential")
	}
}

func TestPluginRegistrationAndInvalidRequests(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodPluginRegister, nil)
	if err != nil || !json.Valid(raw) || !bytes.Contains(raw, []byte(`"auth_provider":true`)) {
		t.Fatalf("registration failed: %v", err)
	}
	if _, errInvalid := handleMethod(pluginabi.MethodAuthLoginStart, []byte("not-json")); errInvalid == nil {
		t.Fatal("invalid JSON accepted")
	}
	auth := bpsauth.New()
	defer auth.Close()
	for _, flags := range []map[string]pluginapi.CommandLineFlagValue{
		{}, {"bps-login": {Value: "true"}, "bps-import": {Value: "file.json"}},
		{"bps-login": {Value: "true"}, "bps-login-method": {Value: "invalid"}},
		{"bps-import": {Value: "file.json"}, "bps-login-method": {Value: "browser"}},
	} {
		if _, errCLI := runCommand(context.Background(), auth, pluginapi.CommandLineExecutionRequest{Flags: flags}, &bytes.Buffer{}); errCLI == nil {
			t.Fatal("ambiguous command accepted")
		}
	}
}

func TestCLIBrowserLoginPrintsURLWithoutDeviceCode(t *testing.T) {
	auth := bpsauth.New()
	defer auth.Close()
	var output bytes.Buffer
	response, err := runCommandWithInput(context.Background(), auth, pluginapi.CommandLineExecutionRequest{Flags: map[string]pluginapi.CommandLineFlagValue{
		"bps-login": {Value: "true"}, "no-browser": {Value: "true"},
	}}, strings.NewReader("https://wrong.invalid/callback?code=fixture-secret\n"), &output)
	if err == nil || len(response.Auths) != 0 {
		t.Fatal("invalid pasted callback unexpectedly acquired credentials")
	}
	if !strings.Contains(output.String(), "https://auth.openai.com/oauth/authorize?") ||
		!strings.Contains(output.String(), "Paste the callback URL") || strings.Contains(output.String(), "Device code:") ||
		strings.Contains(output.String()+err.Error(), "fixture-secret") {
		t.Fatal("browser CLI prompt missing or pasted callback leaked")
	}
}

func TestBrowserCallbackInputIsBounded(t *testing.T) {
	for _, value := range []string{"", "\n", strings.Repeat("x", (16<<10)+1)} {
		if _, err := readBrowserCallback(context.Background(), strings.NewReader(value)); err == nil {
			t.Fatal("empty or oversized callback input accepted")
		}
	}
}

func TestPluginRegistrationReactivatesQuiescedInstance(t *testing.T) {
	before := provider.Load()
	if _, err := handleMethod(pluginabi.MethodPluginReconfigure, nil); err != nil || provider.Load() != before {
		t.Fatal("reconfigure replaced pending login state")
	}
	if _, err := handleMethod(pluginabi.MethodPluginQuiesce, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := before.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{}); err == nil {
		t.Fatal("quiesced provider still accepts logins")
	}
	if _, err := handleMethod(pluginabi.MethodPluginRegister, nil); err != nil || provider.Load() == before {
		t.Fatal("rollback registration did not create a fresh provider")
	}
}
