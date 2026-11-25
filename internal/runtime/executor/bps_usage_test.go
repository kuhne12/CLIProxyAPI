package executor

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"gopkg.in/yaml.v3"
)

type bpsUsageCapture struct {
	authID  string
	records chan usage.Record
}

func (p *bpsUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if record.AuthID == p.authID {
		select {
		case p.records <- record:
		default:
		}
	}
}

func TestBPSExecutorCacheCreationAsInput(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatCodex, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI} {
		for _, stream := range []bool{false, true} {
			for _, enabled := range []bool{false, true} {
				for _, incomplete := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%t/enabled=%t/incomplete=%t", format, stream, enabled, incomplete), func(t *testing.T) {
						doc := ojson.NewDocString(bpsCompletedFixture)
						_ = doc.SetRaw("response.usage", `{"input_tokens":1000,"output_tokens":2,"total_tokens":1002,"input_tokens_details":{"cached_tokens":100,"cache_write_tokens":200,"cache_write_tokens_details":{"ttl_30m_tokens":50,"ttl_12h_tokens":150}}}`)
						if incomplete {
							_ = doc.Set("type", "response.incomplete")
							_ = doc.Set("response.status", "incomplete")
							_ = doc.Set("response.incomplete_details.reason", "max_output_tokens")
						}
						e, _ := newBPSFixture(t, []string{doc.String()}, nil)
						var item config.PluginInstanceConfig
						if err := yaml.Unmarshal([]byte(fmt.Sprintf("enabled: true\ncache-creation-as-input: %t\n", enabled)), &item); err != nil {
							t.Fatal(err)
						}
						e.cfg.Plugins.Configs = map[string]config.PluginInstanceConfig{"bps": item}
						auth := bpsFixtureAuth()
						auth.ID = t.Name()
						capture := &bpsUsageCapture{authID: auth.ID, records: make(chan usage.Record, 1)}
						usage.RegisterNamedPlugin(t.Name(), capture)
						req, opts := bpsFixtureRequest(format)
						var payloads [][]byte
						if stream {
							result, err := e.ExecuteStream(t.Context(), auth, req, opts)
							if err != nil {
								t.Fatal(err)
							}
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
								if ojson.ValidBytes(chunk.Payload) {
									payloads = append(payloads, chunk.Payload)
									continue
								}
								for _, line := range bytes.Split(chunk.Payload, []byte("\n")) {
									if bytes.HasPrefix(line, []byte("data: ")) {
										payloads = append(payloads, bytes.TrimPrefix(line, []byte("data: ")))
									}
								}
							}
						} else {
							response, err := e.Execute(t.Context(), auth, req, opts)
							if err != nil {
								t.Fatal(err)
							}
							payloads = append(payloads, response.Payload)
						}
						found := false
						for _, payload := range payloads {
							for _, root := range []string{"usage", "response.usage"} {
								u := ojson.GetBytes(payload, root)
								if !u.IsObject() {
									continue
								}
								found = true
								input, details := "input_tokens", "input_tokens_details"
								if format == sdktranslator.FormatOpenAI {
									input, details = "prompt_tokens", "prompt_tokens_details"
								}
								wantWrite := int64(200)
								if enabled {
									wantWrite = 0
								}
								if u.Get(input).Int() != 1000 || u.Get("total_tokens").Int() != 1002 || u.Get(details+".cached_tokens").Int() != 100 || u.Get(details+".cache_write_tokens").Int() != wantWrite {
									t.Fatalf("incorrect downstream usage: %s", payload)
								}
								if enabled && u.Get(details+".cache_write_tokens_details").Exists() {
									t.Fatal("downstream TTL counts were retained")
								}
							}
						}
						if !found {
							t.Fatal("no downstream usage returned")
						}
						select {
						case record := <-capture.records:
							if record.Detail.InputTokens != 1000 || record.Detail.CacheCreationTokens != 200 || record.Detail.CacheReadTokens != 100 || record.Detail.TokenBreakdown.Input.UncachedTokens != 700 {
								t.Fatalf("original upstream accounting changed: %+v", record.Detail)
							}
						case <-time.After(5 * time.Second):
							t.Fatal("original usage was not published")
						}
					})
				}
			}
		}
	}
}
