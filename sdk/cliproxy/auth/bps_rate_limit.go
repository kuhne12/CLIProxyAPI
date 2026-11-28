package auth

import (
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

const bpsAccountCoolingCode = "bps_account_cooling"

type bpsRateLimitState struct {
	consecutive int
	until       time.Time
}

// recordBPSRateLimitLocked applies completed generation results in observation order.
// Once armed, a cooldown runs to its original deadline, including when older
// in-flight requests return success or rate limits after the threshold is hit.
func (m *Manager) recordBPSRateLimitLocked(auth *Auth, result Result, now time.Time) bool {
	if auth.Provider != "bps" || !m.bpsRateLimitPolicy.Enabled || auth.Disabled || auth.Status == StatusDisabled || result.SkipQuotaObservation ||
		(result.Error != nil && result.Error.Code == bpsAccountCoolingCode) {
		return false
	}
	state := &auth.bpsRateLimit
	if state.until.After(now) {
		return false
	}
	state.until = time.Time{}
	if result.Success || result.Error == nil || result.Error.Code != pluginabi.ErrorCodeBPSRateLimit || statusCodeFromResult(result.Error) != http.StatusTooManyRequests {
		state.consecutive = 0
		return false
	}
	state.consecutive++
	if state.consecutive < m.bpsRateLimitPolicy.Threshold {
		return false
	}
	state.consecutive = 0
	state.until = now.Add(time.Duration(m.bpsRateLimitPolicy.Seconds) * time.Second)
	m.generations.notifyLocked()
	return true
}

func (a *Auth) credentialRoutingRetryAfter() time.Time {
	if a.bpsRateLimit.until.After(a.credentialRetryAfter) {
		return a.bpsRateLimit.until
	}
	return a.credentialRetryAfter
}
