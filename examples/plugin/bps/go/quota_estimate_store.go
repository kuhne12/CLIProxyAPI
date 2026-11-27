package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

// Persist bounded accounting state and normalized quota snapshots. Credentials,
// request content and unhashed identifiers never enter this file.
type quotaCalibrationAccount struct {
	Key          string                          `json:"key"`
	Plan         string                          `json:"plan"`
	Touched      time.Time                       `json:"touched"`
	Since        time.Time                       `json:"since,omitempty"`
	ManualReset  time.Time                       `json:"manual_reset,omitempty"`
	HistoryFloor time.Time                       `json:"history_floor,omitempty"`
	History      []quotaCostEntry                `json:"history,omitempty"`
	Seen         []string                        `json:"seen,omitempty"`
	Snapshot     *pluginapi.QuotaFetchResponse   `json:"snapshot,omitempty"`
	ObservedAt   time.Time                       `json:"observed_at,omitempty"`
	Windows      map[string]*quotaEstimateWindow `json:"windows"`
}

// Allow bounded histories and deduplication entries for every persisted account.
const quotaEstimateStateLimit = 192 << 20

type quotaCalibrationFile struct {
	Version    int                       `json:"version"`
	ConfigHash string                    `json:"config_hash"`
	Accounts   []quotaCalibrationAccount `json:"accounts"`
}

func newPersistentBPSQuotaEstimator(path string) *bpsQuotaEstimator {
	e := newBPSQuotaEstimator()
	e.statePath = path
	return e
}

func (e *bpsQuotaEstimator) priceFingerprint() string {
	raw, _ := json.Marshal(e.config)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Caller holds e.mu. Restored calibrations are also checked against the live
// plan, reset time and utilization on the next quota fetch.
func (e *bpsQuotaEstimator) loadCalibrationsLocked() {
	if e.statePath == "" {
		return
	}
	f, errOpen := os.Open(e.statePath)
	if os.IsNotExist(errOpen) {
		return
	}
	if errOpen != nil {
		log.Warn("BPS quota estimate state could not be opened")
		return
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.Warn("BPS quota state close failed")
		}
	}()
	raw, errRead := io.ReadAll(io.LimitReader(f, quotaEstimateStateLimit+1))
	var state quotaCalibrationFile
	if errRead != nil || len(raw) > quotaEstimateStateLimit || json.Unmarshal(raw, &state) != nil || (state.Version < 1 || state.Version > 3) || len(state.Accounts) > quotaEstimateMaxAccounts {
		log.Warn("BPS quota estimate state is invalid; new samples are required")
		return
	}
	if state.ConfigHash != e.priceFingerprint() {
		return
	}
	for _, saved := range state.Accounts {
		keyBytes, errKey := hex.DecodeString(saved.Key)
		if errKey != nil || len(keyBytes) != 32 || len(saved.Windows) > 4 || len(saved.Seen) > quotaEstimateMaxRequests || len(saved.History) > quotaEstimateMaxEntries {
			continue
		}
		key := [32]byte(keyBytes)
		a := &quotaEstimateAccount{Since: e.trackingSince, Snapshot: saved.Snapshot, ObservedAt: saved.ObservedAt, RefreshFailed: true, Plan: saved.Plan, Touched: saved.Touched, Windows: make(map[string]*quotaEstimateWindow), Seen: make(map[[32]byte]bool)}
		if state.Version == 3 {
			if !saved.Since.IsZero() {
				a.Since = saved.Since
			}
			a.ManualReset, a.HistoryFloor = saved.ManualReset, saved.HistoryFloor
			for _, entry := range saved.History {
				if !entry.At.IsZero() && finiteQuotaValue(entry.USD) && entry.USD > 0 {
					a.History = append(a.History, entry)
				}
			}
			for name, window := range saved.Windows {
				if len(name) <= 100 && window != nil && !window.Start.IsZero() && window.Start.Before(window.Reset) && finiteQuotaValue(window.OlderCost) && finiteQuotaValue(window.LastUsed) && window.LastUsed <= 1 {
					a.Windows[name] = window
				}
			}
			for _, encoded := range saved.Seen {
				id, errID := hex.DecodeString(encoded)
				if errID == nil && len(id) == 32 && !a.Seen[[32]byte(id)] {
					a.Seen[[32]byte(id)] = true
					a.Order = append(a.Order, [32]byte(id))
				}
			}
		} else if a.Snapshot != nil {
			clearBPSQuotaEstimates(a.Snapshot)
		}

		if len(a.Windows) > 0 || len(a.History) > 0 || len(a.Seen) > 0 || !a.ManualReset.IsZero() || a.Snapshot != nil {
			e.accounts[key] = a
		}
	}
}

func (e *bpsQuotaEstimator) saveCalibrationsLocked() {
	if e.statePath == "" {
		return
	}
	state := quotaCalibrationFile{Version: 3, ConfigHash: e.priceFingerprint()}
	for key, account := range e.accounts {
		saved := quotaCalibrationAccount{Key: hex.EncodeToString(key[:]), Plan: account.Plan, Touched: account.Touched, Since: account.Since, ManualReset: account.ManualReset, HistoryFloor: account.HistoryFloor, History: account.History, Snapshot: account.Snapshot, ObservedAt: account.ObservedAt, Windows: account.Windows}
		for _, id := range account.Order {
			saved.Seen = append(saved.Seen, hex.EncodeToString(id[:]))
		}
		if len(saved.Windows) > 0 || len(saved.History) > 0 || len(saved.Seen) > 0 || !saved.ManualReset.IsZero() || saved.Snapshot != nil {
			state.Accounts = append(state.Accounts, saved)
		}
	}
	if len(state.Accounts) == 0 {
		if _, errStat := os.Stat(e.statePath); os.IsNotExist(errStat) {
			return
		}
	}
	if errSave := writeQuotaCalibrations(e.statePath, state); errSave != nil {
		log.Warn("BPS quota estimates could not be saved; current estimates remain available until restart")
	}
}

// An operator can recover priced request history for an active window during an upgrade.
// The file contains only timestamps, amounts and hashed identifiers for one account.
type quotaHistorySeed struct {
	Key        string               `json:"key"`
	ConfigHash string               `json:"config_hash"`
	Since      time.Time            `json:"since"`
	Through    time.Time            `json:"through"`
	Entries    []quotaRecoveredCost `json:"entries"`
}

type quotaRecoveredCost struct {
	quotaCostEntry
	Hash string `json:"hash"`
}

func (e *bpsQuotaEstimator) importQuotaHistoryLocked(key [32]byte, a *quotaEstimateAccount) {
	if e.statePath == "" || !a.HistoryFloor.IsZero() {
		return
	}
	file, errOpen := os.Open(filepath.Join(filepath.Dir(e.statePath), "bps-quota-history.json"))
	if errOpen != nil {
		return
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.Warn("BPS quota history seed close failed")
		}
	}()
	const limit = 1 << 20
	raw, errRead := io.ReadAll(io.LimitReader(file, limit+1))
	var seed quotaHistorySeed
	if errRead != nil || len(raw) > limit || json.Unmarshal(raw, &seed) != nil || seed.Key != hex.EncodeToString(key[:]) || seed.ConfigHash != e.priceFingerprint() {
		return
	}
	if seed.Since.IsZero() || !seed.Since.Before(a.Since) || seed.Through.Before(a.Since) || len(seed.Entries) == 0 || len(seed.Entries)+len(a.History) > quotaEstimateMaxEntries {
		return
	}
	ids := make([][32]byte, len(seed.Entries))
	for i, entry := range seed.Entries {
		id, errID := hex.DecodeString(entry.Hash)
		if errID != nil || len(id) != 32 || entry.At.Before(seed.Since) || entry.At.After(seed.Through) || !finiteQuotaValue(entry.USD) || entry.USD <= 0 {
			return
		}
		ids[i] = [32]byte(id)
	}
	for i, entry := range seed.Entries {
		if a.remember(ids[i]) {
			a.addCost(entry.quotaCostEntry)
		}
	}
	a.Since = seed.Since
	clear(a.Windows)
}

func writeQuotaCalibrations(path string, state quotaCalibrationFile) error {
	raw, errMarshal := json.Marshal(state)
	if errMarshal != nil {
		return fmt.Errorf("encode BPS quota estimates: %w", errMarshal)
	}
	dir := filepath.Dir(path)
	if errMkdir := os.MkdirAll(dir, 0700); errMkdir != nil {
		return errMkdir
	}
	f, errCreate := os.CreateTemp(dir, ".bps-quota-*.tmp")
	if errCreate != nil {
		return errCreate
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, errWrite := f.Write(raw)
	errSync := f.Sync()
	errClose := f.Close()
	if errWrite != nil {
		return errWrite
	}
	if errSync != nil {
		return errSync
	}
	if errClose != nil {
		return errClose
	}
	return os.Rename(f.Name(), path)
}
