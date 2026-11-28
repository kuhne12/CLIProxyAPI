package helps

import (
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/bpsconfig"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// ApplyBPSServiceTierDoc overrides the caller tier for opted-in BPS credentials.
// Explicit administrator payload rules remain the final semantic barrier.
func ApplyBPSServiceTierDoc(doc *ojson.Doc, cfg *config.Config, auth *coreauth.Auth) {
	if doc == nil || cfg == nil || auth == nil || auth.Provider != "bps" {
		return
	}
	plugin, exists := cfg.Plugins.Configs["bps"]
	if !exists || plugin.Raw.Kind == 0 {
		return
	}
	var policy bpsconfig.ServiceTier
	if plugin.Raw.Decode(&policy) != nil || policy.Validate() != nil || !policy.Forced(auth.ID, auth.FileName) {
		return
	}
	_ = doc.Set("service_tier", "ultrafast")
}
