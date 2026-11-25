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
	log "github.com/sirupsen/logrus"
)

// Only completed estimates are persisted. Tokens, account IDs, request content,
// and partial traffic counters never enter this file.
type quotaCalibration struct {
	Reset        time.Time `json:"reset"`
	Started      time.Time `json:"started"`
	LastUsed     float64   `json:"last_used"`
	TotalUSD     float64   `json:"total_usd"`
	SampleCost   float64   `json:"sample_cost"`
	SamplePoints float64   `json:"sample_points"`
}

type quotaCalibrationAccount struct {
	Key     string                      `json:"key"`
	Plan    string                      `json:"plan"`
	Touched time.Time                   `json:"touched"`
	Windows map[string]quotaCalibration `json:"windows"`
}

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
	raw, errRead := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	var state quotaCalibrationFile
	if errRead != nil || len(raw) > 1<<20 || json.Unmarshal(raw, &state) != nil || state.Version != 1 || len(state.Accounts) > quotaEstimateMaxAccounts {
		log.Warn("BPS quota estimate state is invalid; new samples are required")
		return
	}
	if state.ConfigHash != e.priceFingerprint() {
		return
	}
	for _, saved := range state.Accounts {
		keyBytes, errKey := hex.DecodeString(saved.Key)
		if errKey != nil || len(keyBytes) != 32 || len(saved.Windows) > 4 {
			continue
		}
		key := [32]byte(keyBytes)
		a := &quotaEstimateAccount{Plan: saved.Plan, Touched: saved.Touched, Windows: make(map[string]*quotaEstimateWindow), Seen: make(map[[32]byte]bool)}
		for name, w := range saved.Windows {
			if len(name) > 100 || w.Reset.IsZero() || w.Started.IsZero() || !w.Started.Before(w.Reset) || !finiteQuotaValue(w.TotalUSD) || w.TotalUSD <= 0 || !finiteQuotaValue(w.SampleCost) || w.SampleCost <= 0 || !finiteQuotaValue(w.SamplePoints) || w.SamplePoints < quotaEstimateMinPoints || w.SamplePoints > 100 || !finiteQuotaValue(w.LastUsed) || w.LastUsed > 100 {
				continue
			}
			a.Windows[name] = &quotaEstimateWindow{Reset: w.Reset, SampleStarted: w.Started, LastUsed: w.LastUsed, TotalUSD: w.TotalUSD, SampleCost: w.SampleCost, SamplePoints: w.SamplePoints, Restored: true}
		}
		if len(a.Windows) > 0 {
			e.accounts[key] = a
		}
	}
}

func (e *bpsQuotaEstimator) saveCalibrationsLocked() {
	if e.statePath == "" {
		return
	}
	state := quotaCalibrationFile{Version: 1, ConfigHash: e.priceFingerprint()}
	for key, account := range e.accounts {
		saved := quotaCalibrationAccount{Key: hex.EncodeToString(key[:]), Plan: account.Plan, Touched: account.Touched, Windows: make(map[string]quotaCalibration)}
		for name, w := range account.Windows {
			if w.TotalUSD <= 0 {
				continue
			}
			saved.Windows[name] = quotaCalibration{Reset: w.Reset, Started: w.SampleStarted, LastUsed: w.LastUsed, TotalUSD: w.TotalUSD, SampleCost: w.SampleCost, SamplePoints: w.SamplePoints}
		}
		if len(saved.Windows) > 0 {
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
