// Adapted from sunboyzoey/bps-local-public at 203c716. See ../../../NOTICE.
package bpsauth

import (
	"bytes"
	"strings"
	"time"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/json"
)

// AccountIDFromToken recovers the ChatGPT account id from an id_token when the
// auth file omits the explicit field.
func AccountIDFromToken(idToken string) string {
	claims := decodeJWT(idToken)
	if claims == nil {
		return ""
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	if auth == nil {
		return ""
	}
	id, _ := auth["chatgpt_account_id"].(string)
	return strings.TrimSpace(id)
}

// PlanType reports the subscription tier, for diagnostics only.
func PlanType(idToken string) string {
	claims := decodeJWT(idToken)
	if claims == nil {
		return ""
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	if auth == nil {
		return ""
	}
	plan, _ := auth["chatgpt_plan_type"].(string)
	return strings.TrimSpace(plan)
}

func jwtExpiry(token string) time.Time {
	claims := decodeJWT(token)
	if claims == nil {
		return time.Time{}
	}
	exp, ok := claims["exp"].(float64)
	if !ok {
		return time.Time{}
	}
	return time.Unix(int64(exp), 0)
}

func decodeJWT(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some encoders pad; retry with padding tolerated.
		payload, err = base64.URLEncoding.DecodeString(parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4))
		if err != nil {
			return nil
		}
	}
	var claims map[string]any
	if err := json.NewDecoder(bytes.NewReader(payload)).Decode(&claims); err != nil {
		return nil
	}
	return claims
}
