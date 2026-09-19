package turnstate

import (
	"math/rand/v2"
	"strings"
)

const (
	regionModeFixed    = "fixed"
	regionModeRotation = "rotation"
)

type regionPlanConfig struct {
	provider string
	region   string
	mode     string
}

func harvestRegionConfig(cfg Config) regionPlanConfig {
	cfg = NormalizeConfig(cfg)
	if cfg.HarvestProxyProvider == providerLitport {
		return regionPlanConfig{cfg.HarvestProxyProvider, cfg.LitportRegion, cfg.LitportRegionMode}
	}
	return regionPlanConfig{cfg.HarvestProxyProvider, cfg.ZooRegion, cfg.ZooRegionMode}
}

// One plan per account/model batch, regenerated only when selected routing changes.
func newBatchRegionPlan(cfg Config) [4]string {
	selected := harvestRegionConfig(cfg)
	region := strings.ToUpper(strings.TrimSpace(selected.region))
	if selected.mode != regionModeRotation {
		return [4]string{region, region, region, region}
	}
	europe := []string{"DE", "FR", "GB", "NL", "SE", "FI", "CH", "IE"}
	if selected.provider == providerLitport {
		europe = []string{"DE", "FR", "GB", "NL"}
		// A non-European preferred country must not displace the European stages.
		if region != "FR" && region != "GB" && region != "NL" {
			region = "DE"
		}
	}
	rand.Shuffle(len(europe), func(i, j int) { europe[i], europe[j] = europe[j], europe[i] })
	for i, candidate := range europe {
		if candidate == region {
			europe[0], europe[i] = europe[i], europe[0]
			break
		}
	}
	if selected.provider == providerLitport {
		return [4]string{europe[0], europe[1], europe[2], "JP"}
	}
	asia := [...]string{"JP", "KR", "TW"}
	return [4]string{europe[0], europe[1], europe[2], asia[rand.IntN(len(asia))]}
}

func (c *scheduledCell) syncRegionPlan(cfg Config) {
	selected := harvestRegionConfig(cfg)
	if c.regionConfig != selected {
		c.regionConfig = selected
		c.regionPlan = newBatchRegionPlan(cfg)
	}
}

func batchRegionStage(attempt, maxAttempts int) int {
	span := max(1, maxAttempts/4)
	return min(3, max(0, attempt-1)/span)
}

func (c scheduledCell) regionForAttempt() string {
	return c.regionPlan[batchRegionStage(c.attempt, c.max)]
}
