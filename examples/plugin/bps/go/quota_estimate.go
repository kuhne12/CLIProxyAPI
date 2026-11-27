package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// Prices are administrator-supplied USD per million tokens, not account balances.
type quotaModelPrice struct {
	Input       *float64 `yaml:"input"`
	Output      *float64 `yaml:"output"`
	CacheRead   *float64 `yaml:"cache-read"`
	CacheWrite  *float64 `yaml:"cache-write"`
	Priority    *float64 `yaml:"priority-multiplier"`
	Flex        *float64 `yaml:"flex-multiplier"`
	LongContext int64    `yaml:"long-context-threshold"`
	LongInput   *float64 `yaml:"long-context-input-multiplier"`
	LongOutput  *float64 `yaml:"long-context-output-multiplier"`
}

type quotaEstimateConfig struct {
	CacheCreationAsInput bool                       `yaml:"cache-creation-as-input"`
	Prices               map[string]quotaModelPrice `yaml:"quota-model-prices"`
}

type quotaCostEntry struct {
	At  time.Time `json:"at"`
	USD float64   `json:"usd"`
}

type quotaEstimateWindow struct {
	Start     time.Time `json:"start"`
	Reset     time.Time `json:"reset"`
	LastUsed  float64   `json:"last_used"`
	OlderCost float64   `json:"older_cost"`
	Complete  bool      `json:"complete"`
}

type quotaEstimateAccount struct {
	Plan          string
	Touched       time.Time
	Since         time.Time
	ManualReset   time.Time
	HistoryFloor  time.Time
	History       []quotaCostEntry
	Windows       map[string]*quotaEstimateWindow
	Seen          map[[32]byte]bool
	Order         [][32]byte
	Snapshot      *pluginapi.QuotaFetchResponse
	ObservedAt    time.Time
	AttemptedAt   time.Time
	RefreshFailed bool
	Fetching      chan struct{}
}

type bpsQuotaEstimator struct {
	mu            sync.Mutex
	config        quotaEstimateConfig
	accounts      map[[32]byte]*quotaEstimateAccount
	statePath     string
	configured    bool
	trackingSince time.Time
}

const quotaEstimateMaxAccounts = 256
const quotaEstimateMaxRequests = 4096
const quotaEstimateMaxEntries = 4096

var quotaEstimator = newPersistentBPSQuotaEstimator("plugins/bps-quota-estimates.json")

func newBPSQuotaEstimator() *bpsQuotaEstimator {
	return &bpsQuotaEstimator{accounts: make(map[[32]byte]*quotaEstimateAccount)}
}

func (e *bpsQuotaEstimator) configure(raw []byte, reset bool) error {
	var request struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	var cfg quotaEstimateConfig
	if len(raw) > 0 && json.Unmarshal(raw, &request) != nil {
		return errors.New("invalid BPS plugin configuration request")
	}
	if len(request.ConfigYAML) > 0 && yaml.Unmarshal(request.ConfigYAML, &cfg) != nil {
		return errors.New("invalid BPS quota price configuration")
	}
	if len(cfg.Prices) > 64 {
		return errors.New("BPS quota supports at most 64 model prices")
	}
	for model, price := range cfg.Prices {
		if strings.TrimSpace(model) != model || model == "" || price.Input == nil || price.Output == nil || price.LongContext < 0 {
			return errors.New("BPS quota prices require an exact model ID, input and output prices, and a nonnegative context threshold")
		}
		for _, value := range []*float64{price.Input, price.Output, price.CacheRead, price.CacheWrite, price.Priority, price.Flex, price.LongInput, price.LongOutput} {
			if value != nil && (!finiteQuotaValue(*value) || *value > 1e9) {
				return errors.New("BPS quota prices and multipliers must be finite nonnegative numbers")
			}
		}
		if price.LongContext > 0 && (price.LongInput == nil || price.LongOutput == nil) {
			return errors.New("BPS quota long context pricing requires both input and output multipliers")
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	changed := !reflect.DeepEqual(e.config, cfg)
	if reset || changed {
		e.trackingSince = time.Now()
		e.accounts = make(map[[32]byte]*quotaEstimateAccount)
	}
	e.config = cfg
	if !e.configured || reset {
		e.loadCalibrationsLocked()
	} else if changed {
		e.saveCalibrationsLocked()
	}
	e.configured = true
	return nil
}

func quotaEstimateKey(index, id string) ([32]byte, bool) {
	if index != "" {
		return sha256.Sum256([]byte("index:" + index)), true
	}
	if id != "" {
		return sha256.Sum256([]byte("id:" + id)), true
	}
	return [32]byte{}, false
}

// Only bounded numeric counters, quota snapshots and hashed identifiers are retained.
func (e *bpsQuotaEstimator) account(key [32]byte, now time.Time) *quotaEstimateAccount {
	a := e.accounts[key]
	if a == nil {
		if len(e.accounts) >= quotaEstimateMaxAccounts {
			var oldest [32]byte
			var oldestTime time.Time
			for candidate, value := range e.accounts {
				if oldestTime.IsZero() || value.Touched.Before(oldestTime) {
					oldest, oldestTime = candidate, value.Touched
				}
			}
			delete(e.accounts, oldest)
		}
		a = &quotaEstimateAccount{Since: e.trackingSince, Windows: make(map[string]*quotaEstimateWindow), Seen: make(map[[32]byte]bool)}
		e.accounts[key] = a
	}
	a.Touched = now
	return a
}

func (e *bpsQuotaEstimator) observe(record pluginapi.UsageRecord, now time.Time) {
	if record.Provider != "bps" || !record.Generate {
		return
	}
	key, ok := quotaEstimateKey(record.AuthIndex, record.AuthID)
	if !ok {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.config.Prices) == 0 {
		return
	}
	a := e.account(key, now)
	if record.RequestID != "" && !a.remember(sha256.Sum256([]byte(record.RequestID))) {
		return
	}
	cost, priced := quotaUsageCost(record, e.config)
	if record.Failed || !priced || cost == 0 {
		return
	}
	at := record.RequestedAt
	if at.IsZero() {
		at = now
	}
	a.addCost(quotaCostEntry{At: at, USD: cost})
	e.saveCalibrationsLocked()
}

func (a *quotaEstimateAccount) remember(id [32]byte) bool {
	if a.Seen[id] {
		return false
	}
	if len(a.Order) == quotaEstimateMaxRequests {
		delete(a.Seen, a.Order[0])
		a.Order = a.Order[1:]
	}
	a.Seen[id] = true
	a.Order = append(a.Order, id)
	return true
}

func (a *quotaEstimateAccount) addCost(cost quotaCostEntry) {
	a.History = append(a.History, cost)
	if len(a.History) > quotaEstimateMaxEntries {
		oldest := 0
		for i, entry := range a.History {
			if entry.At.Before(a.History[oldest].At) {
				oldest = i
			}
		}
		entry := a.History[oldest]
		a.History[oldest] = a.History[len(a.History)-1]
		a.History = a.History[:len(a.History)-1]
		if entry.At.After(a.HistoryFloor) {
			a.HistoryFloor = entry.At
		}
		for _, window := range a.Windows {
			if !entry.At.Before(window.Start) && entry.At.Before(window.Reset) {
				window.OlderCost += entry.USD
			}
		}
	}
}

func quotaUsageCost(record pluginapi.UsageRecord, cfg quotaEstimateConfig) (float64, bool) {
	model := record.ResponseModel
	if model == "" {
		model = record.Model
	}
	price, ok := cfg.Prices[model]
	if !ok {
		return 0, false
	}
	d := record.Detail
	if d.InputTokens == 0 && d.OutputTokens == 0 {
		return 0, false
	}
	read := d.CacheReadTokens
	if read == 0 {
		read = d.CachedTokens
	}
	write := d.CacheCreationTokens
	if d.InputTokens < 0 || d.OutputTokens < 0 || read < 0 || write < 0 || read > d.InputTokens || write > d.InputTokens-read {
		return 0, false
	}
	if cfg.CacheCreationAsInput {
		write = 0
	}
	if (read > 0 && price.CacheRead == nil) || (write > 0 && price.CacheWrite == nil) {
		return 0, false
	}
	input, output := *price.Input, *price.Output
	readPrice, writePrice := 0.0, 0.0
	if price.CacheRead != nil {
		readPrice = *price.CacheRead
	}
	if price.CacheWrite != nil {
		writePrice = *price.CacheWrite
	}
	if price.LongContext > 0 && d.InputTokens > price.LongContext {
		input *= *price.LongInput
		readPrice *= *price.LongInput
		writePrice *= *price.LongInput
		output *= *price.LongOutput
	}
	tier := record.ResponseServiceTier
	if tier == "" {
		tier = record.EffectiveServiceTier
	}
	if tier == "" {
		tier = record.ServiceTier
	}
	multiplier := 1.0
	switch tier {
	case "", "default", "auto", "standard":
	case "priority":
		if price.Priority == nil {
			return 0, false
		}
		multiplier = *price.Priority
	case "flex":
		if price.Flex == nil {
			return 0, false
		}
		multiplier = *price.Flex
	default:
		return 0, false
	}
	// Input includes cache tokens; output already includes reasoning tokens.
	cost := (float64(d.InputTokens-read-write)*input + float64(read)*readPrice + float64(write)*writePrice + float64(d.OutputTokens)*output) * multiplier / 1e6
	return cost, finiteQuotaValue(cost)
}

func finiteQuotaValue(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func (e *bpsQuotaEstimator) annotate(req quotaFetchRequest, quota *pluginapi.QuotaFetchResponse, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.annotateLocked(req, quota, now)
	e.saveCalibrationsLocked()
}

func quotaWindowDuration(label string) time.Duration {
	if len(label) < 2 {
		return 0
	}
	unit := map[byte]time.Duration{'d': 24 * time.Hour, 'h': time.Hour, 'm': time.Minute, 's': time.Second}[label[len(label)-1]]
	count, errCount := strconv.ParseInt(label[:len(label)-1], 10, 64)
	if errCount != nil || unit == 0 || count <= 0 || count > int64(365*24*time.Hour/unit) {
		return 0
	}
	return time.Duration(count) * unit
}

func clearBPSQuotaEstimates(quota *pluginapi.QuotaFetchResponse) {
	quota.Summary = slices.DeleteFunc(quota.Summary, func(metric pluginapi.QuotaMetric) bool {
		return strings.HasPrefix(metric.Key, "bps_estimated_")
	})
	for i := range quota.Groups {
		if quota.Groups[i].DisplayName == "Account usage" {
			for j := range quota.Groups[i].Buckets {
				quota.Groups[i].Buckets[j].Description = ""
			}
		}
	}
}

func (e *bpsQuotaEstimator) annotateLocked(req quotaFetchRequest, quota *pluginapi.QuotaFetchResponse, now time.Time) {
	clearBPSQuotaEstimates(quota)
	key, identified := quotaEstimateKey(req.AuthIndex, req.AuthID)
	if !identified || len(e.config.Prices) == 0 {
		return
	}
	a := e.account(key, now)
	plan := ""
	if quota.Subscription != nil {
		plan = quota.Subscription.Plan
	}
	if a.Plan != "" && plan != "" && a.Plan != plan {
		a.Since, a.ManualReset, a.HistoryFloor = now, time.Time{}, time.Time{}
		a.History = nil
		clear(a.Windows)
	}
	if plan != "" {
		a.Plan = plan
	}
	e.importQuotaHistoryLocked(key, a)
	for _, group := range quota.Groups {
		if group.DisplayName != "Account usage" {
			continue
		}
		for bi, bucket := range group.Buckets {
			reset, errReset := time.Parse(time.RFC3339, bucket.ResetTime)
			duration := quotaWindowDuration(bucket.Window)
			serverNow := now.Add(time.Duration(quota.ServerTimeOffsetMs) * time.Millisecond)
			if errReset != nil || !reset.After(serverNow) || duration == 0 || !finiteQuotaValue(bucket.RemainingFraction) || bucket.RemainingFraction > 1 {
				continue
			}
			used := 1 - bucket.RemainingFraction
			start := reset.Add(-duration)
			if a.ManualReset.After(start) {
				start = a.ManualReset
			}
			w := a.Windows[bucket.Window]
			if w == nil || math.Abs(reset.Sub(w.Reset).Seconds()) > 2 || used+1e-8 < w.LastUsed {
				uncertainReset := w != nil && used+1e-8 < w.LastUsed && !start.After(w.Start)
				if uncertainReset {
					// A reset with an unchanged upstream deadline has no exact start.
					start = now
				}
				complete := !start.Before(a.Since) && start.After(a.HistoryFloor)
				if uncertainReset && used > 1e-8 {
					complete = false
				}
				if used <= 1e-8 && (start.Before(a.Since) || !start.After(a.HistoryFloor)) {
					// Zero usage anchors a window that started before tracking began.
					start, complete = now, true
				}
				if len(a.Windows) >= 4 && w == nil {
					clear(a.Windows)
				}
				w = &quotaEstimateWindow{Start: start, Reset: reset, Complete: complete}
				a.Windows[bucket.Window] = w
			}
			w.LastUsed = used
			if !w.Complete || used <= 1e-8 {
				continue
			}
			cost := w.OlderCost
			for _, entry := range a.History {
				if !entry.At.Before(w.Start) && !entry.At.After(now) && entry.At.Before(w.Reset) {
					cost += entry.USD
				}
			}
			total := cost / used
			if total <= 0 || !finiteQuotaValue(total) {
				continue
			}
			prefix := fmt.Sprintf("bps_estimated_%d_", bi)
			quota.Summary = append(quota.Summary,
				pluginapi.QuotaMetric{Key: prefix + "remaining_usd", Label: "Estimated remaining (" + bucket.Window + ")", Value: total * bucket.RemainingFraction, Format: "currency", Currency: "USD"},
				pluginapi.QuotaMetric{Key: prefix + "total_usd", Label: "Estimated total (" + bucket.Window + ")", Value: total, Format: "currency", Currency: "USD"},
			)
		}
	}
}
