package helps

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/bpsbridge"
)

// BPSImageRelay owns one bounded store per service. Only authenticated generation
// creates images; downloads require an unguessable, expiring HMAC URL.
type BPSImageRelay struct {
	mu          sync.Mutex
	relay       *bpsbridge.ImageRelay
	closed      bool
	storageRoot string
}

func BPSImageRelayOrigin(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	plugin, exists := cfg.Plugins.Configs["bps"]
	if !exists || plugin.Raw.Kind == 0 {
		return ""
	}
	var settings struct {
		Origin string `yaml:"image-relay-origin"`
	}
	if plugin.Raw.Decode(&settings) != nil {
		return ""
	}
	return strings.TrimSpace(settings.Origin)
}

// RewriteDoc runs before the executor's final payload configuration barrier.
// The origin is taken from the immutable configuration of this request.
func (r *BPSImageRelay) RewriteDoc(doc *ojson.Doc, scope, origin string) error {
	if r == nil || origin == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return bpsbridge.ErrImageRelayStorage
	}
	if r.relay == nil {
		root := r.storageRoot
		if root == "" {
			root = filepath.Join(os.TempDir(), "cliproxy-bps-images")
		}
		var err error
		r.relay, err = bpsbridge.NewImageRelay(origin, root)
		if err != nil {
			return err
		}
	} else if err := r.relay.SetPublicOrigin(origin); err != nil {
		return err
	}
	return r.relay.RewriteDoc(doc, scope)
}

func (r *BPSImageRelay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	relay, closed := r.relay, r.closed
	r.mu.Unlock()
	if relay == nil || closed {
		http.NotFound(w, req)
		return
	}
	relay.ServeHTTP(w, req)
}

func (r *BPSImageRelay) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return r.relay.Close()
}
