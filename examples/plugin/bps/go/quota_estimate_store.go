package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

// Persist bounded accounting state and normalized quota snapshots. Credentials,
// request content and unhashed identifiers never enter this file.
type quotaCalibration struct {
	BaselineStarted time.Time `json:"baseline_started,omitempty"`
	Reset           time.Time `json:"reset"`
	Started         time.Time `json:"started"`
	LastUsed        float64   `json:"last_used"`
	TotalUSD        float64   `json:"total_usd"`
	SampleCost      float64   `json:"sample_cost"`
	SamplePoints    float64   `json:"sample_points"`
}

type quotaCalibrationAccount struct {
	Cost       float64                       `json:"cost,omitempty"`
	Unknown    uint64                        `json:"unknown,omitempty"`
	Seen       []string                      `json:"seen,omitempty"`
	Snapshot   *pluginapi.QuotaFetchResponse `json:"snapshot,omitempty"`
	ObservedAt time.Time                     `json:"observed_at,omitempty"`
	Key        string                        `json:"key"`
	Plan       string                        `json:"plan"`
	Touched    time.Time                     `json:"touched"`
	Windows    map[string]quotaCalibration   `json:"windows"`
}

// The bound covers 256 accounts with at most 4096 hashed request IDs each.
const quotaEstimateStateLimit = 80 << 20

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
	if errRead != nil || len(raw) > quotaEstimateStateLimit || json.Unmarshal(raw, &state) != nil || (state.Version != 1 && state.Version != 2) || len(state.Accounts) > quotaEstimateMaxAccounts {
		log.Warn("BPS quota estimate state is invalid; new samples are required")
		return
	}
	if state.ConfigHash != e.priceFingerprint() {
		return
	}
	for _, saved := range state.Accounts {
		keyBytes, errKey := hex.DecodeString(saved.Key)
		if errKey != nil || len(keyBytes) != 32 || len(saved.Windows) > 4 || len(saved.Seen) > quotaEstimateMaxRequests || !finiteQuotaValue(saved.Cost) {
			continue
		}
		key := [32]byte(keyBytes)
		a := &quotaEstimateAccount{Cost: saved.Cost, Unknown: saved.Unknown, Snapshot: saved.Snapshot, ObservedAt: saved.ObservedAt, RefreshFailed: true, Plan: saved.Plan, Touched: saved.Touched, Windows: make(map[string]*quotaEstimateWindow), Seen: make(map[[32]byte]bool)}
		for _, encoded := range saved.Seen {
			id, errID := hex.DecodeString(encoded)
			if errID == nil && len(id) == 32 && !a.Seen[[32]byte(id)] {
				a.Seen[[32]byte(id)] = true
				a.Order = append(a.Order, [32]byte(id))
			}
		}
		for name, w := range saved.Windows {
			if w.BaselineStarted.IsZero() {
				w.BaselineStarted = w.Started
			}
			if len(name) > 100 || w.Reset.IsZero() || w.BaselineStarted.IsZero() || !w.BaselineStarted.Before(w.Reset) || !finiteQuotaValue(w.TotalUSD) || !finiteQuotaValue(w.SampleCost) || !finiteQuotaValue(w.SamplePoints) || w.SamplePoints > 100+1e-8 || !finiteQuotaValue(w.LastUsed) || w.LastUsed > 100 {
				continue
			}
			if w.TotalUSD > 0 && (w.SampleCost <= 0 || w.SamplePoints+1e-8 < quotaEstimateMinPoints) {
				continue
			}
			// Version 1 keys included an unstable bucket position.
			if _, window, found := strings.Cut(name, ":"); found {
				name = window
			}
			a.Windows[name] = &quotaEstimateWindow{Reset: w.Reset, Started: w.BaselineStarted, SampleStarted: w.Started, LastUsed: w.LastUsed, TotalUSD: w.TotalUSD, SampleCost: w.SampleCost, SamplePoints: w.SamplePoints, Restored: true}
		}
		if len(a.Windows) > 0 || a.Cost > 0 || a.Unknown > 0 || a.Snapshot != nil {
			e.accounts[key] = a
		}
	}
}

func (e *bpsQuotaEstimator) saveCalibrationsLocked() {
	if e.statePath == "" {
		return
	}
	state := quotaCalibrationFile{Version: 2, ConfigHash: e.priceFingerprint()}
	for key, account := range e.accounts {
		saved := quotaCalibrationAccount{Cost: account.Cost, Unknown: account.Unknown, Snapshot: account.Snapshot, ObservedAt: account.ObservedAt, Key: hex.EncodeToString(key[:]), Plan: account.Plan, Touched: account.Touched, Windows: make(map[string]quotaCalibration)}
		for _, id := range account.Order {
			saved.Seen = append(saved.Seen, hex.EncodeToString(id[:]))
		}
		for name, w := range account.Windows {
			saved.Windows[name] = quotaCalibration{BaselineStarted: w.Started, Reset: w.Reset, Started: w.SampleStarted, LastUsed: w.LastUsed, TotalUSD: w.TotalUSD, SampleCost: w.SampleCost, SamplePoints: w.SamplePoints}
		}
		if len(saved.Windows) > 0 || saved.Cost > 0 || saved.Unknown > 0 || saved.Snapshot != nil {
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
