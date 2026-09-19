package turnstate

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
)

var DefaultModels = []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}

type Config struct {
	AstraPolicyEnabled          bool   `json:"astra_policy_enabled"` // Group action only.
	AstraGroupFailureBatches    int    `json:"astra_group_failure_batches"`
	AstraPriorityPolicyEnabled  bool   `json:"astra_priority_policy_enabled"`
	AstraPriorityFailureBatches int    `json:"astra_priority_failure_batches"`
	AstraFailurePriority        *int64 `json:"astra_failure_priority"`
	AstraRecoveryPriority       int64  `json:"astra_recovery_priority"`
	AstraFailureGroupID         int64  `json:"astra_failure_group_id"`
	AstraRecoveryGroupID        int64  `json:"astra_recovery_group_id"`
	AstraRecheckMinutes         int    `json:"astra_recheck_minutes"`
	AstraMissThresholdPercent   int    `json:"astra_miss_threshold_percent"`
	astraPolicyEpoch            int64
	InjectEnabled               bool     `json:"inject_enabled"`
	AutoHarvest                 bool     `json:"auto_harvest"`
	IntervalMinutes             int      `json:"interval_minutes"`
	MaxAttempts                 int      `json:"max_attempts"`
	Models                      []string `json:"models"`
	Concurrency                 int      `json:"concurrency"`
	AccountConcurrency          int      `json:"account_concurrency"`
	PlanWeightPro               int      `json:"plan_weight_pro"`
	PlanWeightProlite           int      `json:"plan_weight_prolite"`
	PlanWeightPlus              int      `json:"plan_weight_plus"`
	CooldownMinutes             int      `json:"cooldown_minutes"`
	SkipTTLMinutes              int      `json:"skip_ttl_minutes"`
	HarvestProxyProvider        string   `json:"harvest_proxy_provider"`
	LitportHost                 string   `json:"litport_host"`
	LitportUsername             string   `json:"litport_username"`
	LitportPassword             string   `json:"litport_password,omitempty"`
	LitportRegion               string   `json:"litport_region"`
	LitportRegionMode           string   `json:"litport_region_mode"`
	LitportSessionSeconds       int      `json:"litport_session_seconds"`
	harvestSID                  string
	ZooHost                     string  `json:"zoo_host"`
	ZooUserPrefix               string  `json:"zoo_user_prefix"`
	ZooPassword                 string  `json:"zoo_password,omitempty"`
	ZooRegion                   string  `json:"zoo_region"`
	ZooRegionMode               string  `json:"zoo_region_mode"`
	ZooStickyMinutes            int     `json:"zoo_sticky_minutes"`
	DisabledAccountIDs          []int64 `json:"disabled_account_ids"`
}

type PublicConfig struct {
	Config
	ZooPasswordSet     bool `json:"zoo_password_set"`
	LitportPasswordSet bool `json:"litport_password_set"`
}

var configured atomic.Value // Config

func init() {
	configured.Store(DefaultConfig())
}

func DefaultConfig() Config {
	failurePriority := int64(-1)
	return Config{
		AstraGroupFailureBatches:    2,
		AstraPriorityFailureBatches: 1,
		AstraFailurePriority:        &failurePriority,
		AstraMissThresholdPercent:   80,
		AstraRecheckMinutes:         30,
		IntervalMinutes:             50,
		MaxAttempts:                 6,
		Models:                      append([]string(nil), DefaultModels...),
		Concurrency:                 2,
		AccountConcurrency:          1,
		PlanWeightPro:               3,
		PlanWeightProlite:           2,
		PlanWeightPlus:              1,
		CooldownMinutes:             15,
		SkipTTLMinutes:              15,
		HarvestProxyProvider:        providerZoo,
		LitportHost:                 "hub-us-10.litport.net:1337",
		LitportRegion:               "DE",
		LitportRegionMode:           regionModeFixed,
		LitportSessionSeconds:       600,
		ZooHost:                     "us-eu.zooproxy.com:5000",
		ZooRegion:                   "DE",
		ZooRegionMode:               regionModeFixed,
		ZooStickyMinutes:            120,
	}
}

func GetConfig() Config {
	if v, ok := configured.Load().(Config); ok {
		// The atomic snapshot must not expose mutable priority storage.
		if v.AstraFailurePriority != nil {
			priority := *v.AstraFailurePriority
			v.AstraFailurePriority = &priority
		}
		return v
	}
	return DefaultConfig()
}

func SetConfig(cfg Config) {
	configured.Store(NormalizeConfig(cfg))
}

func NormalizeConfig(cfg Config) Config {
	out := DefaultConfig()
	out.AstraPolicyEnabled = cfg.AstraPolicyEnabled
	out.AstraPriorityPolicyEnabled = cfg.AstraPriorityPolicyEnabled
	if cfg.AstraGroupFailureBatches != 0 {
		out.AstraGroupFailureBatches = clampInt(cfg.AstraGroupFailureBatches, 1, 50)
	}
	if cfg.AstraPriorityFailureBatches != 0 {
		out.AstraPriorityFailureBatches = clampInt(cfg.AstraPriorityFailureBatches, 1, 50)
	}
	if cfg.AstraFailurePriority != nil {
		priority := max(int64(-100), min(int64(100), *cfg.AstraFailurePriority))
		out.AstraFailurePriority = &priority
	}
	out.AstraRecoveryPriority = max(int64(-100), min(int64(100), cfg.AstraRecoveryPriority))
	if cfg.AstraMissThresholdPercent != 0 {
		out.AstraMissThresholdPercent = clampInt(cfg.AstraMissThresholdPercent, 1, 100)
	}
	out.AstraFailureGroupID, out.AstraRecoveryGroupID = cfg.AstraFailureGroupID, cfg.AstraRecoveryGroupID
	out.astraPolicyEpoch = cfg.astraPolicyEpoch
	if cfg.AstraRecheckMinutes > 0 {
		out.AstraRecheckMinutes = clampInt(cfg.AstraRecheckMinutes, 1, 24*60)
	}
	out.InjectEnabled = cfg.InjectEnabled
	out.AutoHarvest = cfg.AutoHarvest
	out.PlanWeightPro = normalizePlanWeight(cfg.PlanWeightPro, 3)
	out.PlanWeightProlite = normalizePlanWeight(cfg.PlanWeightProlite, 2)
	out.PlanWeightPlus = normalizePlanWeight(cfg.PlanWeightPlus, 1)
	if cfg.IntervalMinutes > 0 {
		out.IntervalMinutes = clampInt(cfg.IntervalMinutes, 5, 24*60)
	}
	if cfg.MaxAttempts > 0 {
		out.MaxAttempts = clampInt(cfg.MaxAttempts, 1, 50)
	}
	if cfg.Concurrency > 0 {
		out.Concurrency = clampInt(cfg.Concurrency, 1, 12)
	}
	if cfg.AccountConcurrency > 0 {
		out.AccountConcurrency = clampInt(cfg.AccountConcurrency, 1, 4)
	}
	if cfg.CooldownMinutes > 0 {
		out.CooldownMinutes = clampInt(cfg.CooldownMinutes, 1, 24*60)
	}
	if cfg.SkipTTLMinutes >= 0 && cfg.SkipTTLMinutes != 15 {
		out.SkipTTLMinutes = clampInt(cfg.SkipTTLMinutes, 0, 55)
	}
	if provider := strings.ToLower(strings.TrimSpace(cfg.HarvestProxyProvider)); provider != "" {
		out.HarvestProxyProvider = provider
	}
	if host := strings.TrimSpace(cfg.LitportHost); host != "" {
		out.LitportHost = host
	}
	out.LitportUsername = strings.TrimSpace(cfg.LitportUsername)
	out.LitportPassword = cfg.LitportPassword
	out.harvestSID = cfg.harvestSID
	if region := strings.ToUpper(strings.TrimSpace(cfg.LitportRegion)); region != "" {
		out.LitportRegion = region
	}
	if strings.EqualFold(strings.TrimSpace(cfg.LitportRegionMode), regionModeRotation) {
		out.LitportRegionMode = regionModeRotation
	}
	if cfg.LitportSessionSeconds != 0 {
		out.LitportSessionSeconds = clampInt(cfg.LitportSessionSeconds, 1, 86400)
	}
	if host := strings.TrimSpace(cfg.ZooHost); host != "" {
		out.ZooHost = host
	}
	out.ZooUserPrefix = strings.TrimSpace(cfg.ZooUserPrefix)
	out.ZooPassword = cfg.ZooPassword
	if strings.EqualFold(strings.TrimSpace(cfg.ZooRegionMode), regionModeRotation) {
		out.ZooRegionMode = regionModeRotation
	}
	if region := strings.ToUpper(strings.TrimSpace(cfg.ZooRegion)); region != "" {
		out.ZooRegion = region
	}
	if cfg.ZooStickyMinutes > 0 {
		out.ZooStickyMinutes = clampInt(cfg.ZooStickyMinutes, 1, 120)
	}
	if len(cfg.Models) > 0 {
		out.Models = uniqueModels(cfg.Models)
	}
	out.DisabledAccountIDs = uniqueIDs(cfg.DisabledAccountIDs)
	return out
}

func astraPolicyEnabled(cfg Config) bool {
	return cfg.AstraPolicyEnabled || cfg.AstraPriorityPolicyEnabled
}

// Settings changes fence admitted outcomes and start a new shared streak. The
// database retains ownership and per-action episode latches across this fence.
func astraPolicySettingsChanged(before, after Config) bool {
	before, after = NormalizeConfig(before), NormalizeConfig(after)
	return before.AstraPolicyEnabled != after.AstraPolicyEnabled ||
		before.AstraPriorityPolicyEnabled != after.AstraPriorityPolicyEnabled ||
		before.AstraGroupFailureBatches != after.AstraGroupFailureBatches ||
		before.AstraPriorityFailureBatches != after.AstraPriorityFailureBatches ||
		before.AstraFailureGroupID != after.AstraFailureGroupID ||
		before.AstraRecoveryGroupID != after.AstraRecoveryGroupID ||
		*before.AstraFailurePriority != *after.AstraFailurePriority ||
		before.AstraRecoveryPriority != after.AstraRecoveryPriority ||
		before.AstraMissThresholdPercent != after.AstraMissThresholdPercent
}

func ParseConfigJSON(raw string) (Config, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return DefaultConfig(), nil
	}
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return Config{}, fmt.Errorf("turn_state_config: %w", err)
	}
	var stored struct {
		Epoch int64 `json:"_astra_policy_epoch"`
	}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return Config{}, err
	}
	cfg.astraPolicyEpoch = stored.Epoch
	// Preserve legacy settings on load; readiness and writes validate the adapter.
	return NormalizeConfig(cfg), nil
}

func EncodeConfig(cfg Config) (string, error) {
	cfg = NormalizeConfig(cfg)
	if err := ValidateHarvestProxyConfig(cfg); err != nil {
		return "", err
	}
	b, err := json.Marshal(struct {
		Config
		Epoch int64 `json:"_astra_policy_epoch"`
	}{cfg, cfg.astraPolicyEpoch})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func Publicize(cfg Config) PublicConfig {
	cfg = NormalizeConfig(cfg)
	out := PublicConfig{Config: cfg, ZooPasswordSet: strings.TrimSpace(cfg.ZooPassword) != "", LitportPasswordSet: strings.TrimSpace(cfg.LitportPassword) != ""}
	out.ZooPassword, out.LitportPassword = "", ""
	out.harvestSID = ""
	out.ZooHost = publicHarvestHost(out.ZooHost)
	out.LitportHost = publicHarvestHost(out.LitportHost)
	return out
}

func HarvestReady(cfg Config) bool {
	cfg = NormalizeConfig(cfg)
	if ValidateHarvestProxyConfig(cfg) != nil {
		return false
	}
	if cfg.HarvestProxyProvider == providerLitport {
		return strings.TrimSpace(cfg.LitportUsername) != "" && strings.TrimSpace(cfg.LitportPassword) != ""
	}
	return strings.TrimSpace(cfg.ZooUserPrefix) != "" && strings.TrimSpace(cfg.ZooPassword) != ""
}

func AccountHarvestEnabled(cfg Config, accountID int64) bool {
	for _, id := range cfg.DisabledAccountIDs {
		if id == accountID {
			return false
		}
	}
	return true
}

func uniqueModels(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		key := strings.ToLower(m)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, m)
	}
	if len(out) == 0 {
		return append([]string(nil), DefaultModels...)
	}
	return out
}

func uniqueIDs(in []int64) []int64 {
	seen := map[int64]struct{}{}
	out := make([]int64, 0, len(in))
	for _, id := range in {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// Harvest plan aliases intentionally do not use auth's global normalization.
func normalizeHarvestPlan(plan string) string {
	plan = strings.ToLower(strings.TrimSpace(plan))
	switch plan {
	case "prolite", "pro_lite", "pro-lite":
		return "prolite"
	default:
		return plan
	}
}

func normalizePlanWeight(weight, fallback int) int {
	if weight == 0 {
		return fallback
	}
	return clampInt(weight, 1, 10)
}

func harvestPlanWeight(cfg Config, plan string) int {
	switch normalizeHarvestPlan(plan) {
	case "pro":
		return normalizePlanWeight(cfg.PlanWeightPro, 3)
	case "prolite":
		return normalizePlanWeight(cfg.PlanWeightProlite, 2)
	case "plus":
		return normalizePlanWeight(cfg.PlanWeightPlus, 1)
	default:
		return 1
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
