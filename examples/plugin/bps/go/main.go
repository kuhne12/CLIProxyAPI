package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/examples/plugin/bps/go/internal/bpsauth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

var provider atomic.Pointer[bpsauth.Provider]

func init() { resetProvider() }

func resetProvider() {
	if previous := provider.Swap(bpsauth.New()); previous != nil {
		previous.Close()
	}
}

func main() {}

func handleMethod(method string, raw []byte) ([]byte, error) {
	ctx := context.Background()
	authProvider := provider.Load()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfig := quotaEstimator.configure(raw, method == pluginabi.MethodPluginRegister); errConfig != nil {
			return nil, errConfig
		}
		// Registration also reactivates a quiesced instance after a failed
		// replacement. Ordinary configuration updates preserve pending logins.
		if method == pluginabi.MethodPluginRegister {
			resetProvider()
		}
		return okEnvelope(map[string]any{
			"schema_version": pluginabi.SchemaVersion,
			"metadata": pluginapi.Metadata{Name: "bps", Version: "0.5.6", Author: "CLIProxyAPI contributors",
				GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI", ConfigFields: []pluginapi.ConfigField{{
					Name: "cache-creation-as-input", Type: pluginapi.ConfigFieldTypeBoolean, Default: false,
					Description: "Report BPS cache creation as regular input to downstream clients. Total input and cache reads stay unchanged; CPA retains original upstream usage. Requires the matching CPA BPS executor.",
				}, {
					Name: "quota-model-prices", Type: pluginapi.ConfigFieldTypeObject, Default: map[string]any{},
					Description: "USD per million tokens by exact model ID: input, output, cache-read, cache-write; optional priority-multiplier, flex-multiplier, long-context-threshold, long-context-input-multiplier, long-context-output-multiplier. Estimates quota value from observed usage and percentage changes, not an official balance. Completed estimates survive restart; new sampling intervals start after restart. Price, plan or quota window changes invalidate estimates.",
				}, {
					Name: "image-relay-origin", Type: pluginapi.ConfigFieldTypeString, Default: "",
					Description: "Public HTTPS origin pointing to this CPA server (no path), for example https://cpa.example.com. Inline PNG/JPEG/GIF/WebP images are served through signed temporary URLs for at most 10 minutes. Empty keeps HTTPS-only image input. Requires the matching CPA executor.",
				}}},
			"capabilities": map[string]bool{"auth_provider": true, "command_line_plugin": true, "model_provider": true, "quota_provider": true, "usage_plugin": true, "management_api": true},
		})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(pluginapi.ManagementRegistrationResponse{Resources: []pluginapi.ResourceRoute{{
			Path: settingsPath, Menu: "BPS 设置", Description: "缓存计量、图片中转与模型价格",
		}}})
	case pluginabi.MethodManagementHandle:
		return dispatch(raw, serveSettings)
	case pluginabi.MethodUsageHandle:
		return dispatch(raw, func(record pluginapi.UsageRecord) (struct{}, error) {
			quotaEstimator.observe(record, time.Now())
			return struct{}{}, nil
		})
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		authProvider.Close()
		return okEnvelope(struct{}{})
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return dispatch(raw, func(req pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
			response := pluginapi.ModelResponse{Provider: "bps"}
			if req.AuthProvider != "" && req.AuthProvider != "bps" {
				return response, nil
			}
			for _, id := range []string{"gpt-6-astra", "gpt-6-sol", "gpt-6.2-astra", "gpt-6.2-sol"} {
				levels := []string{"low", "medium", "high", "xhigh"}
				if strings.Contains(id, "6.2-") {
					levels = append(levels, "max")
				}
				response.Models = append(response.Models, pluginapi.ModelInfo{ID: id, Object: "model", OwnedBy: "bps", Type: "bps", DisplayName: id, Name: id,
					SupportedInputModalities: []string{"text", "image"}, SupportedOutputModalities: []string{"text"}, Thinking: &pluginapi.ThinkingSupport{Levels: levels}})
			}
			return response, nil
		})
	case pluginabi.MethodAuthIdentifier, pluginabi.MethodQuotaIdentifier:
		return okEnvelope(map[string]string{"identifier": "bps"})
	case pluginabi.MethodQuotaDescribe:
		return okEnvelope(pluginapi.QuotaDescribeResponse{SupportedProviders: []string{"bps"}, DisplayName: "BPS", SupportsReset: true})
	case pluginabi.MethodQuotaFetch:
		return dispatch(raw, func(req quotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
			quota, errQuota := fetchBPSQuota(req, callHostAPI)
			if errQuota == nil {
				quotaEstimator.annotate(req, &quota, time.Now())
			}
			return quota, errQuota
		})
	case pluginabi.MethodQuotaReset:
		return dispatch(raw, func(req quotaResetRequest) (pluginapi.QuotaResetResponse, error) {
			return resetBPSQuota(req, callHostAPI)
		})
	case pluginabi.MethodAuthLoginStart:
		return dispatch(raw, func(req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
			return authProvider.StartLogin(ctx, req)
		})
	case pluginabi.MethodAuthLoginPoll:
		return dispatch(raw, func(req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
			return authProvider.PollLogin(ctx, req)
		})
	case pluginabi.MethodAuthParse:
		return dispatch(raw, func(req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
			return authProvider.ParseAuth(ctx, req)
		})
	case pluginabi.MethodAuthRefresh:
		return dispatch(raw, func(req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
			return authProvider.RefreshAuth(ctx, req)
		})
	case pluginabi.MethodCommandLineRegister:
		return okEnvelope(pluginapi.CommandLineRegistrationResponse{Flags: []pluginapi.CommandLineFlag{
			{Name: "bps-login", Type: "bool", Usage: "Log in to BPS using a browser authorization URL and pasted callback"},
			{Name: "bps-login-method", Type: "string", Usage: "BPS login method: browser (default) or device"},
			{Name: "bps-import", Type: "string", Usage: "Import an independent BPS OAuth JSON file"},
		}})
	case pluginabi.MethodCommandLineExecute:
		return dispatch(raw, func(req pluginapi.CommandLineExecutionRequest) (pluginapi.CommandLineExecutionResponse, error) {
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
			defer stop()
			return runCommand(ctx, authProvider, req, os.Stdout)
		})
	default:
		return errorEnvelope("unknown_method", "unknown plugin method"), nil
	}
}

func dispatch[Request, Response any](raw []byte, invoke func(Request) (Response, error)) ([]byte, error) {
	var req Request
	if json.Unmarshal(raw, &req) != nil {
		return nil, errors.New("invalid plugin request JSON")
	}
	response, err := invoke(req)
	if err != nil {
		return nil, err
	}
	return okEnvelope(response)
}

func okEnvelope(value any) ([]byte, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errors.New("cannot encode plugin response")
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	status := http.StatusBadRequest
	if strings.Contains(message, "invalid_grant") {
		status = http.StatusUnauthorized
	}
	raw, _ := pluginabi.NewErrorEnvelope(code, message, status)
	return raw
}

func runCommand(ctx context.Context, auth *bpsauth.Provider, req pluginapi.CommandLineExecutionRequest, output io.Writer) (pluginapi.CommandLineExecutionResponse, error) {
	return runCommandWithInput(ctx, auth, req, os.Stdin, output)
}

func runCommandWithInput(ctx context.Context, auth *bpsauth.Provider, req pluginapi.CommandLineExecutionRequest, input io.Reader, output io.Writer) (pluginapi.CommandLineExecutionResponse, error) {
	login := req.Flags["bps-login"].Value == "true"
	path := strings.TrimSpace(req.Flags["bps-import"].Value)
	method := strings.TrimSpace(req.Flags["bps-login-method"].Value)
	if login == (path != "") {
		return pluginapi.CommandLineExecutionResponse{}, errors.New("choose exactly one of --bps-login or --bps-import")
	}
	if method != "" && (!login || (method != "browser" && method != "device")) {
		return pluginapi.CommandLineExecutionResponse{}, errors.New("--bps-login-method requires --bps-login and must be browser or device")
	}
	var record pluginapi.AuthData
	if path != "" {
		file, errOpen := os.Open(path)
		if errOpen != nil {
			return pluginapi.CommandLineExecutionResponse{}, errors.New("cannot open BPS OAuth file")
		}
		defer func() {
			if errClose := file.Close(); errClose != nil {
				log.Warn("BPS import file close failed")
			}
		}()
		raw, errRead := io.ReadAll(io.LimitReader(file, (2<<20)+1))
		if errRead != nil {
			return pluginapi.CommandLineExecutionResponse{}, errors.New("cannot read BPS OAuth file")
		}
		parsed, errParse := auth.ParseAuth(ctx, pluginapi.AuthParseRequest{Provider: "bps", RawJSON: raw})
		if errParse != nil {
			return pluginapi.CommandLineExecutionResponse{}, errParse
		}
		if !parsed.Handled {
			return pluginapi.CommandLineExecutionResponse{}, errors.New("file is not an independent BPS OAuth grant")
		}
		record = parsed.Auth
	} else {
		start, errStart := auth.StartLogin(ctx, pluginapi.AuthLoginStartRequest{Provider: "bps", Host: req.Host, Metadata: map[string]any{"method": method}})
		if errStart != nil {
			return pluginapi.CommandLineExecutionResponse{}, errStart
		}
		// CommandLineExecutionResponse.Stdout is delayed until completion.
		if _, errWrite := fmt.Fprintf(output, "Open: %s\n", start.URL); errWrite != nil {
			return pluginapi.CommandLineExecutionResponse{}, errWrite
		}
		prompt := "Complete BPS login in your browser, then copy the full callback URL from the address bar."
		if start.UserCode != "" {
			prompt = "Device code: " + start.UserCode + "\nComplete BPS authorization in your browser. Waiting up to 15 minutes..."
		}
		if _, errWrite := fmt.Fprintln(output, prompt); errWrite != nil {
			return pluginapi.CommandLineExecutionResponse{}, errWrite
		}
		if req.Flags["no-browser"].Value != "true" {
			if errOpen := openBrowser(ctx, start.URL); errOpen != nil {
				_, _ = fmt.Fprintln(output, "Open the URL above manually; the browser could not be opened.")
			}
		}
		ctx, cancel := context.WithDeadline(ctx, start.ExpiresAt)
		defer cancel()
		var metadata map[string]any
		if start.UserCode == "" {
			if _, errWrite := fmt.Fprintln(output, "Paste the callback URL here (within 15 minutes):"); errWrite != nil {
				return pluginapi.CommandLineExecutionResponse{}, errWrite
			}
			callback, errRead := readBrowserCallback(ctx, input)
			if errRead != nil {
				return pluginapi.CommandLineExecutionResponse{}, errRead
			}
			metadata = map[string]any{"redirect_url": callback}
		}
		ticker := time.NewTicker(time.Duration(start.Interval) * time.Second)
		defer ticker.Stop()
		for record.Provider == "" {
			if start.UserCode != "" {
				select {
				case <-ctx.Done():
					return pluginapi.CommandLineExecutionResponse{}, ctx.Err()
				case <-ticker.C:
				}
			}
			polled, errPoll := auth.PollLogin(ctx, pluginapi.AuthLoginPollRequest{Provider: "bps", State: start.State, Host: req.Host, Metadata: metadata})
			if errPoll != nil {
				return pluginapi.CommandLineExecutionResponse{}, errPoll
			}
			if polled.Status == pluginapi.AuthLoginStatusSuccess {
				record = polled.Auth
			}
		}
	}
	return pluginapi.CommandLineExecutionResponse{Auths: []pluginapi.AuthData{record},
		Stdout: []byte("BPS OAuth grant acquired. Generation permission is not yet verified.\n")}, nil
}

func readBrowserCallback(ctx context.Context, input io.Reader) (string, error) {
	type result struct {
		value string
		err   error
	}
	read := make(chan result, 1)
	go func() {
		line, errRead := bufio.NewReader(io.LimitReader(input, (16<<10)+1)).ReadString('\n')
		if len(line) > 16<<10 || (errRead != nil && !errors.Is(errRead, io.EOF)) || strings.TrimSpace(line) == "" {
			read <- result{err: errors.New("cannot read BPS callback URL")}
			return
		}
		read <- result{value: strings.TrimSpace(line)}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case response := <-read:
		return response.value, response.err
	}
}

func openBrowser(ctx context.Context, address string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.CommandContext(ctx, "open", address)
	case "windows":
		command = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", address)
	default:
		command = exec.CommandContext(ctx, "xdg-open", address)
	}
	return command.Run()
}
