package management

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	log "github.com/sirupsen/logrus"
)

// servePluginAuthURLV8 publishes public device fields without exposing private metadata.
func (h *Handler) servePluginAuthURLV8(c *gin.Context) bool {
	if h == nil || c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	h.mu.Lock()
	host := h.pluginHost
	h.mu.Unlock()
	if host == nil {
		return false
	}
	provider, ok := pluginAuthProviderFromURL(c.Request.URL)
	if !ok && strings.TrimSpace(c.Request.URL.Path) == "/v8/management/oauth/auth-url" {
		normalized, errNormalize := NormalizePluginOAuthCallbackProvider(oauthSessionProviderFromContext(c))
		if errNormalize == nil {
			provider, ok = normalized, true
		}
	}
	if !ok || !host.HasAuthProvider(provider) {
		return false
	}

	sessionProxyURL := oauthSessionProxyURLFromContext(c)
	ctx := PopulateAuthContext(context.Background(), c)
	if sessionProxyURL != "" {
		ctx = pluginhost.WithOAuthProxyURL(ctx, sessionProxyURL)
	}
	query := c.Request.URL.Query()
	query.Del("provider")
	baseURL, errBaseURL := h.managementCallbackURL("/v8/management/oauth/callback")
	if errBaseURL != nil {
		log.WithError(errBaseURL).Error("failed to compute plugin auth callback URL")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return true
	}
	metadata := queryValuesToMetadata(query)
	if extraMeta := oauthSessionExtraMetadataFromContext(c); len(extraMeta) > 0 {
		if metadata == nil {
			metadata = make(map[string]any, len(extraMeta))
		}
		for k, v := range extraMeta {
			if _, exists := metadata[k]; !exists {
				metadata[k] = v
			}
		}
	}
	resp, handled, errStart := host.StartLogin(ctx, provider, baseURL, metadata)
	if !handled {
		return false
	}
	if errStart != nil {
		log.Errorf("failed to start plugin auth login: %s", redactSessionProxyInString(errStart.Error(), sessionProxyURL))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return true
	}
	state := strings.TrimSpace(resp.State)
	if state == "" {
		log.WithField("provider", provider).Error("plugin auth provider returned empty state")
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid oauth state"})
		return true
	}
	if errState := ValidateOAuthState(state); errState != nil {
		log.WithError(errState).WithField("provider", provider).Error("plugin auth provider returned invalid state")
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid oauth state"})
		return true
	}
	if errRegister := RegisterPluginOAuthSessionWithProxy(state, provider, resp.Metadata, sessionProxyURL); errRegister != nil {
		log.WithError(errRegister).WithField("provider", provider).Error("failed to register plugin oauth session")
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to generate authorization url"})
		return true
	}
	sessionTarget, hasSessionTarget := oauthSessionTargetFromContext(c)
	if hasSessionTarget {
		SetOAuthSessionTarget(state, sessionTarget)
	}
	respPayload := gin.H{"status": "ok", "url": resp.URL, "state": state}
	if hasSessionTarget {
		respPayload["target_auth_id"] = sessionTarget.AuthID
	}
	if resp.UserCode != "" {
		respPayload["user_code"] = resp.UserCode
	}
	if resp.Interval > 0 {
		respPayload["interval"] = resp.Interval
	}
	if !resp.ExpiresAt.IsZero() {
		respPayload["expires_at"] = resp.ExpiresAt
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, respPayload)
	return true
}
