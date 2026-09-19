package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

type astraPolicyConfig struct {
	Enabled          bool  `json:"astra_policy_enabled"`
	PriorityEnabled  bool  `json:"astra_priority_policy_enabled"`
	Epoch            int64 `json:"_astra_policy_epoch"`
	Failure          int64 `json:"astra_failure_group_id"`
	Recovery         int64 `json:"astra_recovery_group_id"`
	Minutes          int   `json:"astra_recheck_minutes"`
	GroupBatches     int   `json:"astra_group_failure_batches"`
	PriorityBatches  int   `json:"astra_priority_failure_batches"`
	FailurePriority  *int  `json:"astra_failure_priority"`
	RecoveryPriority int   `json:"astra_recovery_priority"`
}

func decodeAstraPolicyConfig(raw string) (astraPolicyConfig, error) {
	failure := -1
	cfg := astraPolicyConfig{GroupBatches: 2, PriorityBatches: 1, FailurePriority: &failure}
	err := json.Unmarshal([]byte(raw), &cfg)
	if cfg.FailurePriority == nil {
		cfg.FailurePriority = &failure
	}
	if cfg.GroupBatches == 0 {
		cfg.GroupBatches = 2
	}
	cfg.GroupBatches = max(1, min(cfg.GroupBatches, 50))
	cfg.PriorityBatches = max(1, min(cfg.PriorityBatches, 50))
	*cfg.FailurePriority = max(-100, min(*cfg.FailurePriority, 100))
	cfg.RecoveryPriority = max(-100, min(cfg.RecoveryPriority, 100))
	return cfg, err
}

func loadAstraPolicyState(ctx context.Context, tx *sql.Tx, id int64) (AstraPolicyState, error) {
	s := AstraPolicyState{AccountID: id, OriginalGroupIDs: []int64{}}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT state_json FROM codex_astra_policy WHERE account_id=$1`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err == nil {
		err = decodeAstraState(raw, &s)
	}
	s.AccountID = id
	if s.OriginalGroupIDs == nil {
		s.OriginalGroupIDs = []int64{}
	}
	return s, err
}

func saveAstraPolicyState(ctx context.Context, tx *sql.Tx, s AstraPolicyState, groupVersion, priorityVersion int64) error {
	encoded, err := encodeAstraState(s)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO codex_astra_policy(account_id,state_json,membership_version,priority_version) VALUES($1,$2,$3,$4) ON CONFLICT(account_id) DO UPDATE SET state_json=excluded.state_json,membership_version=excluded.membership_version,priority_version=excluded.priority_version`, s.AccountID, string(encoded), groupVersion, priorityVersion)
	return err
}

func policyPriority(ctx context.Context, tx *sql.Tx, id int64) (int, error) {
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(credentials,'{}') FROM accounts WHERE id=$1`, id).Scan(&raw); err != nil {
		return 0, err
	}
	var row AccountRow
	if err := json.Unmarshal(raw, &row.Credentials); err != nil {
		return 0, err
	}
	priority, valid := row.GetCredentialInt64("scheduler_priority")
	if !valid {
		priority = 0
	}
	return int(max(int64(-100), min(priority, int64(100)))), nil
}

// writeAstraPriority intentionally bypasses the manual credential writer and
// does not advance credential_generation. Normal scheduler outbox triggers run.
func (db *DB) writeAstraPriority(ctx context.Context, tx *sql.Tx, id int64, priority int) error {
	q := `UPDATE accounts SET credentials=json_set(COALESCE(credentials,'{}'),'$.scheduler_priority',$1),updated_at=CURRENT_TIMESTAMP WHERE id=$2`
	if !db.isSQLite() {
		q = `UPDATE accounts SET credentials=jsonb_set(COALESCE(credentials,'{}'::jsonb),'{scheduler_priority}',to_jsonb($1::integer),true),updated_at=CURRENT_TIMESTAMP WHERE id=$2`
	}
	_, err := tx.ExecContext(ctx, q, priority, id)
	return err
}

func (s *AstraPolicyState) activeEpisode() bool {
	return s.ConsecutiveFailures > 0 || s.GroupTriggered || s.PriorityTriggered || s.GroupSuppressed || s.PrioritySuppressed || s.Demoted || s.PriorityDemoted
}

func (s *AstraPolicyState) suppressGroup(now time.Time) {
	s.Demoted, s.GroupSuppressed = false, true
	s.GroupSuppressedAt, s.GroupSuppressedAtNano = now.Unix(), now.UnixNano()
	s.NextRecoveryAt = 0
	s.Error = "manual_membership_changed"
}
func (s *AstraPolicyState) suppressPriority(now time.Time) {
	s.PriorityDemoted, s.PrioritySuppressed = false, true
	s.PrioritySuppressedAt, s.PrioritySuppressedAtNano = now.Unix(), now.UnixNano()
	s.Error = "manual_priority_changed"
}

// markManualPolicyPriority must be called iff the incoming credential map has
// scheduler_priority, including same-value assignments, in that write's tx.
// It locks only the account (no settings lock) and preserves the other action.
func (db *DB) markManualPolicyPriority(ctx context.Context, tx *sql.Tx, id int64) error {
	return db.markManualPolicyProperty(ctx, tx, id, true)
}

func (db *DB) markManualPolicyProperty(ctx context.Context, tx *sql.Tx, id int64, priority bool) error {
	if err := db.lockPolicyAccount(ctx, tx, id); err != nil {
		return err
	}
	s, err := loadAstraPolicyState(ctx, tx, id)
	if err != nil {
		return err
	}
	var groupVersion, priorityVersion int64
	err = tx.QueryRowContext(ctx, `SELECT membership_version,priority_version FROM codex_astra_policy WHERE account_id=$1`, id).Scan(&groupVersion, &priorityVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if priority {
		priorityVersion++
		if s.activeEpisode() {
			s.suppressPriority(time.Now())
		}
	} else {
		groupVersion++
		if s.activeEpisode() {
			s.suppressGroup(time.Now())
		}
	}
	return saveAstraPolicyState(ctx, tx, s, groupVersion, priorityVersion)
}

func astraFreshSuccess(o AstraPolicyOutcome, afterSeconds, afterNano, now int64) bool {
	return o.Outcome == "new_292" && o.Confirmed && o.ObtainedAtNano > afterNano && o.IssuedUnix >= afterSeconds && o.IssuedUnix <= now+60 && o.IssuedUnix+3600 > now
}

func (db *DB) applyAstraPolicyActions(ctx context.Context, tx *sql.Tx, s *AstraPolicyState, current *AstraPolicyExpectation, cfg astraPolicyConfig, o AstraPolicyOutcome, now time.Time) (bool, error) {
	groupExpected := current.Version == o.Expected.Version && slices.Equal(current.GroupIDs, o.Expected.GroupIDs)
	priorityExpected := current.PriorityVersion == o.Expected.PriorityVersion && current.Priority == o.Expected.Priority
	groupOwned := s.Demoted && current.Version == s.OwnedVersion && slices.Equal(current.GroupIDs, []int64{s.FailureGroupID})
	priorityOwned := s.PriorityDemoted && current.PriorityVersion == s.PriorityOwnedVersion && current.Priority == s.FailurePriority
	if s.Demoted && !groupOwned {
		s.suppressGroup(now)
	}
	if s.PriorityDemoted && !priorityOwned {
		s.suppressPriority(now)
	}
	if s.GroupSuppressed {
		s.Error = "manual_membership_changed"
	}
	if s.PrioritySuppressed {
		s.Error = "manual_priority_changed"
	}
	if o.Outcome == "ordinary_miss" {
		// A manual write during the very first in-flight batch had no active
		// episode to suppress yet. Its expectation mismatch protects it now.
		// An owned automatic assignment is not a manual override.
		if !groupExpected && !groupOwned && !s.GroupSuppressed {
			s.suppressGroup(now)
		}
		if !priorityExpected && !priorityOwned && !s.PrioritySuppressed {
			s.suppressPriority(now)
		}
		// Count accepted qualifying batches regardless of either action's latch.
		s.ConsecutiveFailures = min(s.ConsecutiveFailures+1, 50)
	}
	if o.Outcome == "new_292" {
		s.ConsecutiveFailures = 0
		s.Last292At = now.Unix()
		if o.Confirmed {
			s.LastConfirmed292At = now.Unix()
		}
	}
	changed := false
	groupRecover := cfg.Enabled && groupOwned && groupExpected && !s.GroupSuppressed && astraFreshSuccess(o, s.DemotedAt, s.DemotedAtNano, now.Unix())
	groupFire := cfg.Enabled && o.Outcome == "ordinary_miss" && groupExpected && !s.GroupTriggered && !s.GroupSuppressed && s.ConsecutiveFailures >= cfg.GroupBatches
	if groupFire || groupRecover {
		validation := error(nil)
		if cfg.Failure == cfg.Recovery {
			validation = errors.New("policy groups must be distinct")
		} else {
			for _, id := range []int64{cfg.Failure, cfg.Recovery} {
				if err := validateAstraGroup(ctx, tx, db.isSQLite(), id); err != nil {
					validation = err
					break
				}
			}
		}
		if validation != nil {
			s.Error = validation.Error()
		} else {
			target := cfg.Failure
			if groupRecover {
				target = cfg.Recovery
			}
			if !slices.Equal(current.GroupIDs, []int64{target}) {
				if _, err := tx.ExecContext(ctx, `DELETE FROM account_group_members WHERE account_id=$1`, s.AccountID); err != nil {
					return false, err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO account_group_members(account_id,group_id) VALUES($1,$2)`, s.AccountID, target); err != nil {
					return false, err
				}
				changed = true
			}
			current.Version++
			if groupRecover {
				s.Demoted, s.GroupTriggered = false, false
			} else {
				s.OriginalGroupIDs = append([]int64{}, current.GroupIDs...)
				s.FailureGroupID, s.OwnedVersion = target, current.Version
				s.Demoted, s.GroupTriggered = true, true
				s.DemotedAt, s.DemotedAtNano = now.Unix(), now.UnixNano()
			}
			current.GroupIDs = []int64{target}
		}
	}
	priorityRecover := cfg.PriorityEnabled && priorityOwned && priorityExpected && !s.PrioritySuppressed && astraFreshSuccess(o, s.PriorityDemotedAt, s.PriorityDemotedAtNano, now.Unix())
	priorityFire := cfg.PriorityEnabled && o.Outcome == "ordinary_miss" && priorityExpected && !s.PriorityTriggered && !s.PrioritySuppressed && s.ConsecutiveFailures >= cfg.PriorityBatches
	if priorityFire || priorityRecover {
		target := *cfg.FailurePriority
		if priorityRecover {
			target = cfg.RecoveryPriority
		}
		// Assign the canonical configured value even if the effective scheduler
		// value already matches (e.g. missing, string, or clamped credentials).
		// This executes once per action, not once per subsequent failed batch.
		if err := db.writeAstraPriority(ctx, tx, s.AccountID, target); err != nil {
			return false, err
		}
		changed = true
		current.PriorityVersion++
		if priorityRecover {
			s.PriorityDemoted, s.PriorityTriggered = false, false
		} else {
			s.FailurePriority, s.PriorityOwnedVersion = target, current.PriorityVersion
			s.PriorityDemoted, s.PriorityTriggered = true, true
			s.PriorityDemotedAt, s.PriorityDemotedAtNano = now.Unix(), now.UnixNano()
		}
		current.Priority = target
	}
	// A manual override ends ownership, never restores. Only a success obtained
	// after that override (with its corresponding expectation) rearms the latch.
	if s.GroupSuppressed && groupExpected && astraFreshSuccess(o, s.GroupSuppressedAt, s.GroupSuppressedAtNano, now.Unix()) {
		s.GroupSuppressed, s.GroupTriggered = false, false
	}
	if s.PrioritySuppressed && priorityExpected && astraFreshSuccess(o, s.PrioritySuppressedAt, s.PrioritySuppressedAtNano, now.Unix()) {
		s.PrioritySuppressed, s.PriorityTriggered = false, false
	}
	s.NextRecoveryAt = 0
	if s.Demoted {
		s.NextRecoveryAt = now.Unix() + int64(max(cfg.Minutes, 1))*60
	}
	return changed, nil
}
