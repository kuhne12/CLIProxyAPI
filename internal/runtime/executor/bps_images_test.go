package executor

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"gopkg.in/yaml.v3"
)

func TestBPSInlineImagesAndFinalPayloadRules(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes())
	for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse} {
		for _, stream := range []bool{false, true} {
			for _, filter := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%v/filter=%v", format, stream, filter), func(t *testing.T) {
					relay := &helps.BPSImageRelay{}
					t.Cleanup(func() {
						if err := relay.Close(); err != nil {
							t.Error(err)
						}
					})
					e, _ := newBPSFixture(t, []string{bpsCompletedFixture}, func(raw []byte) {
						if bytes.Contains(raw, []byte("data:image")) {
							t.Error("base64 reached upstream")
						}
						prefix := "https://relay.example/api/bps-images/"
						i := strings.Index(string(raw), prefix)
						if filter {
							if i >= 0 {
								t.Error("relay restored filtered input")
							}
							return
						}
						if i < 0 || len(raw) < i+len(prefix)+43 {
							t.Error("missing signed image URL")
							return
						}
						url := string(raw[i : i+len(prefix)+43])
						w := httptest.NewRecorder()
						relay.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
						if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), encoded.Bytes()) {
							t.Error("relay did not serve original image")
						}
					})
					if err := yaml.Unmarshal([]byte("plugins:\n  configs:\n    bps:\n      image-relay-origin: https://relay.example\n"), e.cfg); err != nil {
						t.Fatal(err)
					}
					e.WithImageRelay(relay)
					if filter {
						e.cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "gpt-6-astra", Protocol: "codex"}}, Params: []string{"input"}}}
					}
					part := map[string]any{"type": "input_image", "image_url": dataURL}
					field := "input"
					if format == sdktranslator.FormatOpenAI {
						field = "messages"
						part = map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}}
					}
					raw, errMarshal := json.Marshal(map[string]any{"model": "gpt-6-astra", field: []any{map[string]any{"role": "user", "content": []any{part}}}})
					if errMarshal != nil {
						t.Fatal(errMarshal)
					}
					req := coreexecutor.Request{Model: "gpt-6-astra", Format: format, Payload: raw}
					opts := coreexecutor.Options{SourceFormat: format, OriginalRequest: raw}
					if stream {
						response, err := e.ExecuteStream(t.Context(), bpsFixtureAuth(), req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range response.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, err := e.Execute(t.Context(), bpsFixtureAuth(), req, opts); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}
