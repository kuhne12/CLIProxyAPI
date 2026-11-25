package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"reflect"
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

type quotaEstimateWindow struct {
	SampleStarted time.Time
	Restored      bool
	Reset         time.Time
	Started       time.Time
	BaseUsed      float64
	LastUsed      float64
	LastCost      float64
	BaseCost      float64
	BaseUnknown   uint64
	TotalUSD      float64
	SampleCost    float64
	SamplePoints  float64
}

type quotaEstimateAccount struct {
	Cost    float64
	Unknown uint64
	Plan    string
	Touched time.Time
	Windows map[string]*quotaEstimateWindow
	Seen    map[[32]byte]bool
	Order   [][32]byte
}

type bpsQuotaEstimator struct {
	mu         sync.Mutex
	config     quotaEstimateConfig
	accounts   map[[32]byte]*quotaEstimateAccount
	statePath  string
	configured bool
}

const quotaEstimateMaxAccounts = 256
const quotaEstimateMaxRequests = 4096
const quotaEstimateMinPoints = 1.0

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

// Only bounded numeric counters and hashed identifiers are retained in memory.
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
		a = &quotaEstimateAccount{Windows: make(map[string]*quotaEstimateWindow), Seen: make(map[[32]byte]bool)}
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
	if record.RequestID != "" {
		id := sha256.Sum256([]byte(record.RequestID))
		if a.Seen[id] {
			return
		}
		if len(a.Order) == quotaEstimateMaxRequests {
			delete(a.Seen, a.Order[0])
			a.Order = a.Order[1:]
		}
		a.Seen[id] = true
		a.Order = append(a.Order, id)
	}
	// Failed executions can consume upstream quota without complete usage.
	cost, priced := quotaUsageCost(record, e.config)
	if record.Failed || !priced {
		a.Unknown++
		return
	}
	a.Cost += cost
	if !finiteQuotaValue(a.Cost) {
		a.Cost = 0
		a.Unknown++
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
	key, identified := quotaEstimateKey(req.AuthIndex, req.AuthID)
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.saveCalibrationsLocked()
	if len(e.config.Prices) == 0 {
		return
	}
	plan := ""
	if quota.Subscription != nil {
		plan = quota.Subscription.Plan
	}
	for gi := range quota.Groups {
		group := &quota.Groups[gi]
		if group.DisplayName != "Account usage" {
			continue
		}
		for bi := range group.Buckets {
			bucket := &group.Buckets[bi]
			bucket.Description = "USD estimate unavailable: credential identity and a future quota reset time are required."
			reset, errReset := time.Parse(time.RFC3339, bucket.ResetTime)
			serverNow := now.Add(time.Duration(quota.ServerTimeOffsetMs) * time.Millisecond)
			if !identified || errReset != nil || !reset.After(serverNow) {
				continue
			}
			a := e.account(key, now)
			if a.Plan != plan || len(a.Windows) > 4 {
				a.Plan = plan
				a.Windows = make(map[string]*quotaEstimateWindow)
			}
			used := (1 - bucket.RemainingFraction) * 100
			windowKey := fmt.Sprintf("%d:%s", bi, bucket.Window)
			w := a.Windows[windowKey]
			// Relative reset times can vary slightly because of HTTP date precision.
			resetChanged := w != nil && math.Abs(reset.Sub(w.Reset).Seconds()) > 60
			// Restarted traffic counters cannot account for activity while offline.
			// Keep the completed calibration, but start a new observation interval.
			if w != nil && w.Restored {
				w.Restored = false
				w.Started, w.BaseUsed, w.BaseCost, w.BaseUnknown = now, used, a.Cost, a.Unknown
			}
			if w == nil || resetChanged || used+1e-8 < w.LastUsed {
				w = &quotaEstimateWindow{Reset: reset, Started: now, BaseUsed: used, LastUsed: used, BaseCost: a.Cost, BaseUnknown: a.Unknown}
				a.Windows[windowKey] = w
			}
			if w.BaseUnknown != a.Unknown || a.Cost < w.BaseCost || (w.TotalUSD > 0 && used > w.LastUsed+1e-8 && a.Cost == w.LastCost) {
				w.Started, w.BaseUsed, w.BaseCost, w.BaseUnknown = now, used, a.Cost, a.Unknown
			}
			w.LastUsed, w.LastCost = used, a.Cost
			deltaUsed, deltaCost := used-w.BaseUsed, a.Cost-w.BaseCost
			if deltaUsed+1e-8 >= quotaEstimateMinPoints && deltaCost == 0 {
				w.Started, w.BaseUsed, w.BaseCost, w.BaseUnknown = now, used, a.Cost, a.Unknown
				deltaUsed = 0
			}
			if deltaUsed+1e-8 >= quotaEstimateMinPoints && deltaCost > 0 {
				estimate := deltaCost * 100 / deltaUsed
				if finiteQuotaValue(estimate) && estimate > 0 {
					w.TotalUSD, w.SampleCost, w.SamplePoints = estimate, deltaCost, deltaUsed
					w.SampleStarted = w.Started
				}
			}
			if w.TotalUSD <= 0 {
				bucket.Description = fmt.Sprintf("Collecting USD estimate samples: $%.4f priced usage, %.2f percentage points since %s. Requires at least 1 percentage point and priced BPS usage; re-query after normal usage.", deltaCost, deltaUsed, w.Started.UTC().Format(time.RFC3339))
				continue
			}
			prefix := fmt.Sprintf("bps_estimated_%d_", bi)
			quota.Summary = append(quota.Summary,
				pluginapi.QuotaMetric{Key: prefix + "remaining_usd", Label: "Estimated remaining (" + bucket.Window + ")", Value: w.TotalUSD * bucket.RemainingFraction, Format: "currency", Currency: "USD"},
				pluginapi.QuotaMetric{Key: prefix + "total_usd", Label: "Estimated total (" + bucket.Window + ")", Value: w.TotalUSD, Format: "currency", Currency: "USD"},
			)
			bucket.Description = fmt.Sprintf("Estimated API value, not an official or redeemable USD balance. Sample: $%.4f / %.2f percentage points since %s. Uses configured model prices and observed CPA traffic; external usage, model mix and rounded quota percentages affect accuracy.", w.SampleCost, w.SamplePoints, w.SampleStarted.UTC().Format(time.RFC3339))
		}
	}
}
