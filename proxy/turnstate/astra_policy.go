package turnstate

import (
	"context"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/codex2api/database"
)

const astraModel = "gpt-6-astra"

// PolicySnapshot is safe to embed in admin JSON. It never exposes a ticket,
// credential, transport error, or the internal settings epoch.
func (h *Harvester) PolicySnapshot(accountID int64) database.AstraPolicyState {
	cfg := GetConfig()
	s := database.AstraPolicyState{AccountID: accountID, Enabled: astraPolicyEnabled(cfg), OriginalGroupIDs: []int64{}}
	if h.db == nil {
		return s
	}
	h.policyMu.Lock()
	defer h.policyMu.Unlock()
	if time.Since(h.policyLoaded) > 5*time.Second {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		states, err := h.db.ListAstraPolicyStates(ctx)
		cancel()
		h.policyLoaded = time.Now()
		h.policyError = err != nil
		if err == nil {
			h.policyStates = make(map[int64]database.AstraPolicyState, len(states))
			for _, state := range states {
				h.policyStates[state.AccountID] = state
			}
		}
	}
	if saved, ok := h.policyStates[accountID]; ok {
		s = saved
	}
	s.Enabled = astraPolicyEnabled(cfg)
	if s.Epoch != cfg.astraPolicyEpoch {
		s.ConsecutiveFailures = 0
	}
	if h.policyError {
		s.Error = "policy_state_unavailable"
	}
	if message := h.policyErrors[accountID]; message != "" {
		s.Error = message
	}
	s.OriginalGroupIDs = append([]int64{}, s.OriginalGroupIDs...)
	s.LatestBatch = s.LatestBatch.Clone()
	return s
}

func (h *Harvester) automaticPolicyAllowed(id int64, model string, cfg Config) bool {
	if h.db == nil || !cfg.AstraPolicyEnabled {
		return true
	}
	s := h.PolicySnapshot(id)
	if s.Error == "policy_state_unavailable" {
		return false
	}
	if !s.Demoted {
		return true
	}
	return cfg.AstraPolicyEnabled && strings.EqualFold(model, astraModel) && time.Now().Unix() >= s.NextRecoveryAt
}

func (h *Harvester) preparePolicyBatch(ctx context.Context, c *scheduledCell, cfg Config) {
	c.batchID = randomSID() + randomSID()
	c.ordinaryMisses = 0
	c.missThresholdPercent = NormalizeConfig(cfg).AstraMissThresholdPercent
	if h.db == nil || !astraPolicyEnabled(cfg) || !strings.EqualFold(c.model, astraModel) {
		return
	}
	expected, err := h.db.AstraPolicyExpectation(ctx, c.accountID)
	if err != nil {
		return
	} // fail closed: no captured expectation, no mutation
	c.policyEpoch = cfg.astraPolicyEpoch
	c.expectedGroups = expected
	c.batchSequence = expected.Sequence
}

func classifyPolicyBatch(c *scheduledCell, r attemptResult) (string, *database.AstraBatchEvidence) {
	e := &database.AstraBatchEvidence{
		Attempts: c.attempt, MaxAttempts: c.max, OrdinaryMisses: c.ordinaryMisses,
		ThresholdPercent: c.missThresholdPercent, Exhausted: r.phase == "exhausted",
		Reason: "not_exhausted",
	}
	if r.new292 {
		e.Reason = "success"
		if !r.confirmed {
			e.Reason = "confirmation_warning"
		}
		return "new_292", e
	}
	if r.phase == "cancelled" || r.phase == "skipped" {
		e.Reason = "interrupted"
	} else if e.Exhausted && c.attempt == c.max && c.max > 0 {
		e.Reason = "insufficient_misses"
		if c.missThresholdPercent >= 1 && c.missThresholdPercent <= 100 &&
			c.ordinaryMisses >= 0 && c.ordinaryMisses <= c.attempt &&
			c.ordinaryMisses*100 >= c.max*c.missThresholdPercent {
			e.Reason = "qualified_miss"
			return "ordinary_miss", e
		}
	}
	return "inconclusive", e
}

// Called under h.mu, before retiring the generation. SaveConfig uses the same
// lock. publishMu closes the manual-paste/clear versus policy-commit race.
func (h *Harvester) finishPolicyBatch(ctx context.Context, c *scheduledCell, r attemptResult) {
	if h.db == nil || c.policyEpoch == 0 || ctx.Err() != nil {
		return
	}
	h.publishMu.Lock()
	defer h.publishMu.Unlock()
	if h.generations[c.key] != c.generation || c.generation.version != c.version {
		return
	}
	outcome, evidence := classifyPolicyBatch(c, r)
	changed, err := h.db.ApplyAstraPolicyOutcome(ctx, database.AstraPolicyOutcome{
		LatestBatch: evidence,
		AccountID:   c.accountID, BatchID: c.batchID, BatchSequence: c.batchSequence, Epoch: c.policyEpoch, Expected: c.expectedGroups,
		Outcome: outcome, Confirmed: r.confirmed, ObtainedAtNano: r.obtainedAtNano, IssuedUnix: r.issuedUnix,
	})
	h.policyMu.Lock()
	h.policyLoaded = time.Time{}
	h.policyError = err != nil
	if h.policyErrors == nil {
		h.policyErrors = map[int64]string{}
	}
	if err != nil {
		h.policyErrors[c.accountID] = "policy_update_failed"
	} else {
		delete(h.policyErrors, c.accountID)
	}
	h.policyMu.Unlock()
	if err != nil {
		log.Printf("[turn-state] policy persistence failed for account %d", c.accountID)
		return
	}
	if changed {
		if store, ok := h.store.(interface{ ApplyAccountGroups(int64, []int64) bool }); ok {
			groups, err := h.db.GetAccountGroupIDs(ctx, c.accountID)
			for tries := 0; err == nil && tries < 3; tries++ {
				store.ApplyAccountGroups(c.accountID, groups)
				var latest []int64
				latest, err = h.db.GetAccountGroupIDs(ctx, c.accountID)
				if err != nil || slices.Equal(groups, latest) {
					break
				}
				groups = latest
			}
		}
		if store, ok := h.store.(interface{ ApplyAccountSchedulerPriority(int64, *int64) bool }); ok {
			row, err := h.db.GetAccountByID(ctx, c.accountID)
			for tries := 0; err == nil && row != nil && tries < 3; tries++ {
				priority := astraSchedulerPriority(row)
				store.ApplyAccountSchedulerPriority(c.accountID, &priority)
				row, err = h.db.GetAccountByID(ctx, c.accountID)
				if err != nil || row == nil || priority == astraSchedulerPriority(row) {
					break
				}
			}
		}
	}
}

// Match the scheduler's credential decoding and bounds, including integral JSON
// numbers formatted with exponents. Missing or malformed priority defaults to zero.
func astraSchedulerPriority(row *database.AccountRow) int64 {
	if row == nil {
		return 0
	}
	priority, valid := row.GetCredentialInt64("scheduler_priority")
	if !valid {
		return 0
	}
	return max(int64(-100), min(int64(100), priority))
}
