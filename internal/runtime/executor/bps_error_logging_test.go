package executor

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestBPSExecutorLogsOriginalLimitBeforePublicMapping(t *testing.T) {
	logger := log.StandardLogger()
	hooks, level, output := logger.Hooks, logger.GetLevel(), logger.Out
	logger.ReplaceHooks(make(log.LevelHooks))
	hook := logtest.NewGlobal()
	logger.SetLevel(log.WarnLevel)
	logger.SetOutput(io.Discard)
	t.Cleanup(func() { logger.ReplaceHooks(hooks); logger.SetLevel(level); logger.SetOutput(output) })
	for _, stream := range []bool{false, true} {
		hook.Reset()
		original := strings.Repeat("upstream concurrent capacity detail; ", 100) + "fixture-token"
		raw, errMarshal := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"code": "concurrent_request_limit", "message": original, "max_concurrent": 4, "retry_after_seconds": 23}})
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		e, count := newBPSFixture(t, []string{string(raw)}, nil)
		req, opts := bpsFixtureRequest(sdktranslator.FormatOpenAIResponse)
		ctx := logging.WithRequestID(t.Context(), "bps-original-limit-fixture")
		var err error
		if stream {
			result, errOpen := e.ExecuteStream(ctx, bpsFixtureAuth(), req, opts)
			if errOpen != nil {
				t.Fatal(errOpen)
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					err = chunk.Err
				}
			}
		} else {
			_, err = e.Execute(ctx, bpsFixtureAuth(), req, opts)
		}
		var failure *pluginabi.Error
		if !errors.As(err, &failure) || failure.HTTPStatus != 429 || failure.Message != "Service is temporarily rate limited; retry later" || count.Load() != 1 {
			t.Fatalf("incorrect public error or unexpected replay: %v", err)
		}
		var found int
		for _, entry := range hook.AllEntries() {
			if entry.Message != "BPS upstream error" || entry.Data["request_id"] != "bps-original-limit-fixture" {
				continue
			}
			found++
			payload, _ := entry.Data["upstream_error_payload"].(string)
			if entry.Data["upstream_error_code"] != "concurrent_request_limit" || ojson.Get(payload, "error.message").String() != strings.ReplaceAll(original, "fixture-token", "[REDACTED]") || ojson.Get(payload, "error.retry_after_seconds").Int() != 23 || ojson.Get(payload, "error.max_concurrent").Int() != 4 {
				t.Fatal("original upstream error was lost or credential leaked")
			}
		}
		if found != 1 {
			t.Fatalf("expected one original error log, got %d", found)
		}
	}
}
