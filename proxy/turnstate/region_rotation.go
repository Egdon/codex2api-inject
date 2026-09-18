package turnstate

import (
	"math/rand/v2"
	"strings"
)

const (
	regionModeFixed    = "fixed"
	regionModeRotation = "rotation"
)

// Plans are small value arrays, owned by one account/model batch. They are
// generated once at admission, never per retry or per shared global job.
func newBatchRegionPlan(cfg Config) [4]string {
	region := strings.ToUpper(strings.TrimSpace(cfg.ZooRegion))
	if region == "" {
		region = "DE"
	}
	if cfg.ZooRegionMode != regionModeRotation {
		return [4]string{region, region, region, region}
	}
	europe := []string{"DE", "FR", "GB", "NL", "SE", "FI", "CH", "IE"}
	rand.Shuffle(len(europe), func(i, j int) { europe[i], europe[j] = europe[j], europe[i] })
	// Honor the preferred first region only if it belongs to the European pool.
	for i, candidate := range europe {
		if candidate == region {
			europe[0], europe[i] = europe[i], europe[0]
			break
		}
	}
	asia := [...]string{"JP", "KR", "TW"}
	return [4]string{europe[0], europe[1], europe[2], asia[rand.IntN(len(asia))]}
}

func batchRegionStage(attempt, maxAttempts int) int {
	span := max(1, maxAttempts/4)
	return min(3, max(0, attempt-1)/span)
}

func (c scheduledCell) regionForAttempt() string {
	return c.regionPlan[batchRegionStage(c.attempt, c.max)]
}
