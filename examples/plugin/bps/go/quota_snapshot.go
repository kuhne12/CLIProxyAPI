package main

import (
	"errors"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const bpsQuotaCacheTTL = time.Minute

// fetchSnapshot shares each account's refresh across background and UI callers.
// Credentials and callbacks belong only to this call, never to the saved cache.
func (e *bpsQuotaEstimator) fetchSnapshot(req quotaFetchRequest, fetch func() (pluginapi.QuotaFetchResponse, error), now time.Time) (pluginapi.QuotaFetchResponse, error) {
	key, ok := quotaEstimateKey(req.AuthIndex, req.AuthID)
	if !ok || req.Provider != "bps" {
		return fetch()
	}
	e.mu.Lock()
	a := e.account(key, now)
	if a.Fetching != nil {
		if a.Snapshot != nil {
			q := cloneQuotaSnapshot(a, now)
			e.mu.Unlock()
			return q, nil
		}
		done := a.Fetching
		e.mu.Unlock()
		<-done
		return e.fetchSnapshot(req, fetch, now)
	}
	if !req.ForceRefresh && !a.AttemptedAt.IsZero() && now.Sub(a.AttemptedAt) < bpsQuotaCacheTTL {
		q := cloneQuotaSnapshot(a, now)
		hasSnapshot := a.Snapshot != nil
		e.mu.Unlock()
		if !hasSnapshot {
			return q, errors.New("Quota is temporarily unavailable; automatic refresh will retry")
		}
		return q, nil
	}
	a.AttemptedAt = now
	done := make(chan struct{})
	a.Fetching = done
	e.mu.Unlock()

	q, errFetch := fetch()
	e.mu.Lock()
	defer e.mu.Unlock()
	defer close(done)
	a.Fetching = nil
	if e.accounts[key] != a {
		return pluginapi.QuotaFetchResponse{}, errors.New("Quota configuration changed; refresh again")
	}
	a.RefreshFailed = errFetch != nil
	if errFetch == nil {
		observedAt, errObserved := time.Parse(time.RFC3339Nano, q.ObservedAt)
		if errObserved != nil {
			observedAt = now
		}
		e.annotateLocked(req, &q, observedAt)
		q.ObservedAt = observedAt.UTC().Format(time.RFC3339Nano)
		q.Stale = false
		a.Snapshot, a.ObservedAt = &q, observedAt
		e.saveCalibrationsLocked()
	}
	if a.Snapshot != nil {
		return cloneQuotaSnapshot(a, now), nil
	}
	return q, errFetch
}

func cloneQuotaSnapshot(a *quotaEstimateAccount, now time.Time) pluginapi.QuotaFetchResponse {
	var q pluginapi.QuotaFetchResponse
	if a.Snapshot != nil {
		raw, _ := json.Marshal(a.Snapshot)
		_ = json.Unmarshal(raw, &q)
		q.Stale = a.RefreshFailed || now.Sub(a.ObservedAt) >= 2*bpsQuotaCacheTTL
	}
	return q
}

func (e *bpsQuotaEstimator) invalidateSnapshot(index, id string, resetAt time.Time) {
	key, ok := quotaEstimateKey(index, id)
	if !ok {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	previous := e.accounts[key]
	// Replace the entry so a quota request started before redemption cannot restore it.
	delete(e.accounts, key)
	a := e.account(key, resetAt)
	a.ManualReset = resetAt
	if previous != nil {
		a.Seen, a.Order = previous.Seen, previous.Order
	}
	e.saveCalibrationsLocked()
}
