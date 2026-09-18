package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// AstraBatchEvidence describes only the latest accepted batch, not a history.
type AstraBatchEvidence struct {
	Attempts         int    `json:"attempts"`
	MaxAttempts      int    `json:"max_attempts"`
	OrdinaryMisses   int    `json:"ordinary_misses"`
	ThresholdPercent int    `json:"threshold_percent"`
	Exhausted        bool   `json:"exhausted"`
	Reason           string `json:"reason"`
}

func (e *AstraBatchEvidence) Clone() *AstraBatchEvidence {
	if e == nil {
		return nil
	}
	copy := *e
	return &copy
}

// AstraPolicyState contains audit metadata only; never credentials or tickets.
// Times exposed to callers are Unix seconds.
type AstraPolicyState struct {
	LatestBatch         *AstraBatchEvidence `json:"latest_batch,omitempty"`
	AccountID           int64               `json:"account_id"`
	Enabled             bool                `json:"enabled"`
	ConsecutiveFailures int                 `json:"consecutive_failures"`
	Demoted             bool                `json:"demoted"`
	FailureGroupID      int64               `json:"failure_group_id"`
	OriginalGroupIDs    []int64             `json:"original_group_ids"`
	DemotedAt           int64               `json:"demoted_at"`
	LastBatchID         string              `json:"last_batch_id"`
	LastOutcome         string              `json:"last_outcome"`
	LastOutcomeAt       int64               `json:"last_outcome_at"`
	Last292At           int64               `json:"last_292_at"`
	LastConfirmed292At  int64               `json:"last_confirmed_292_at"`
	NextRecoveryAt      int64               `json:"next_recovery_at"`
	Error               string              `json:"error"`
	Epoch               int64               `json:"-"`
	OwnedVersion        int64               `json:"-"`
	LastBatchSequence   int64               `json:"-"`
	DemotedAtNano       int64               `json:"-"`
}

type astraPolicyStored struct {
	AstraPolicyState
	Epoch             int64 `json:"epoch"`
	OwnedVersion      int64 `json:"owned_version"`
	LastBatchSequence int64 `json:"last_batch_sequence"`
	DemotedAtNano     int64 `json:"demoted_at_nano"`
}

func encodeAstraState(s AstraPolicyState) ([]byte, error) {
	return json.Marshal(astraPolicyStored{s, s.Epoch, s.OwnedVersion, s.LastBatchSequence, s.DemotedAtNano})
}
func decodeAstraState(raw string, s *AstraPolicyState) error {
	var stored astraPolicyStored
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return err
	}
	*s = stored.AstraPolicyState
	s.LastBatchSequence = stored.LastBatchSequence
	s.Epoch, s.OwnedVersion, s.DemotedAtNano = stored.Epoch, stored.OwnedVersion, stored.DemotedAtNano
	return nil
}

func (db *DB) ensureAstraPolicySchema(ctx context.Context) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS codex_astra_policy (account_id BIGINT PRIMARY KEY, state_json TEXT NOT NULL DEFAULT '{}', membership_version BIGINT NOT NULL DEFAULT 0)`,
	} {
		if _, err := db.conn.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("astra policy schema: %w", err)
		}
	}
	return nil
}

func (db *DB) AstraPolicySnapshot(ctx context.Context, id int64) (AstraPolicyState, error) {
	s := AstraPolicyState{AccountID: id, OriginalGroupIDs: []int64{}}
	var raw string
	err := db.conn.QueryRowContext(ctx, `SELECT state_json FROM codex_astra_policy WHERE account_id=$1`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	err = decodeAstraState(raw, &s)
	s.AccountID = id
	if s.OriginalGroupIDs == nil {
		s.OriginalGroupIDs = []int64{}
	}
	return s, err
}

// lockPolicyAccount is shared with membership writers. SQLite callers already
// hold the write gate/immediate transaction; PostgreSQL serializes on accounts.
func (db *DB) lockPolicyAccount(ctx context.Context, tx *sql.Tx, id int64) error {
	q := `SELECT id FROM accounts WHERE id=$1`
	if !db.isSQLite() {
		q += ` FOR UPDATE`
	}
	var found int64
	return tx.QueryRowContext(ctx, q, id).Scan(&found)
}

// markManualPolicyGroups invalidates ownership even for a same-value assignment.
// It intentionally does not restore groups or count outcomes while disabled.
func (db *DB) markManualPolicyGroups(ctx context.Context, tx *sql.Tx, ids []int64) error {
	ids = append([]int64(nil), ids...)
	slices.Sort(ids)
	for _, id := range ids {
		if err := db.lockPolicyAccount(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO codex_astra_policy (account_id,membership_version) VALUES ($1,1) ON CONFLICT(account_id) DO UPDATE SET membership_version=codex_astra_policy.membership_version+1`, id); err != nil {
			return err
		}
	}
	return nil
}

type AstraPolicyExpectation struct {
	GroupIDs []int64
	Version  int64
	Sequence int64
}

func policyMembership(ctx context.Context, tx *sql.Tx, id int64) (AstraPolicyExpectation, error) {
	out := AstraPolicyExpectation{GroupIDs: []int64{}}
	rows, err := tx.QueryContext(ctx, `SELECT group_id FROM account_group_members WHERE account_id=$1 ORDER BY group_id`, id)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var gid int64
		if err = rows.Scan(&gid); err != nil {
			rows.Close()
			return out, err
		}
		out.GroupIDs = append(out.GroupIDs, gid)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	err = tx.QueryRowContext(ctx, `SELECT membership_version FROM codex_astra_policy WHERE account_id=$1`, id).Scan(&out.Version)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return out, err
}

func (db *DB) AstraPolicyExpectation(ctx context.Context, id int64) (AstraPolicyExpectation, error) {
	var out AstraPolicyExpectation
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if err := db.lockPolicyAccount(ctx, tx, id); err != nil {
			return err
		}
		var err error
		out, err = policyMembership(ctx, tx, id)
		if err != nil {
			return err
		}
		var raw string
		err = tx.QueryRowContext(ctx, `SELECT state_json FROM codex_astra_policy WHERE account_id=$1`, id).Scan(&raw)
		var state AstraPolicyState
		if err == nil {
			if err = decodeAstraState(raw, &state); err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		out.Sequence = max(time.Now().UnixNano(), state.LastBatchSequence+1)
		return nil
	})
	return out, err
}

func validateAstraGroup(ctx context.Context, tx *sql.Tx, sqlite bool, id int64) error {
	if id <= 0 {
		return errors.New("policy group must be selected")
	}
	q := `SELECT COALESCE(channel,'codex') FROM account_groups WHERE id=$1`
	if !sqlite {
		q += ` FOR SHARE`
	}
	var channel string
	if err := tx.QueryRowContext(ctx, q, id).Scan(&channel); err != nil {
		return errors.New("policy group does not exist")
	}
	if channel != "" && !strings.EqualFold(strings.TrimSpace(channel), "codex") {
		return errors.New("policy group must use Codex channel")
	}
	return nil
}
func (db *DB) ValidateAstraPolicyGroups(ctx context.Context, failure, recovery int64) error {
	if failure == recovery {
		return errors.New("failure and recovery groups must be distinct")
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		for _, id := range []int64{failure, recovery} {
			if err := validateAstraGroup(ctx, tx, db.isSQLite(), id); err != nil {
				return err
			}
		}
		return nil
	})
}

type AstraPolicyOutcome struct {
	LatestBatch    *AstraBatchEvidence
	AccountID      int64
	BatchSequence  int64
	BatchID        string
	Epoch          int64
	Expected       AstraPolicyExpectation
	Outcome        string // ordinary_miss, new_292, inconclusive
	Confirmed      bool
	ObtainedAtNano int64
	IssuedUnix     int64
}

// ApplyAstraPolicyOutcome atomically checks settings, receipt, membership/version,
// updates state, and replaces memberships. The caller must refresh runtime groups
// from the DB after commit, never apply a previously computed desired slice.
func (db *DB) ApplyAstraPolicyOutcome(ctx context.Context, o AstraPolicyOutcome) (changed bool, err error) {
	if o.BatchID == "" || o.Epoch == 0 {
		return false, nil
	}
	err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		changed = false
		q := `SELECT COALESCE(turn_state_config,'{}') FROM system_settings WHERE id=1`
		if !db.isSQLite() {
			q += ` FOR SHARE`
		}
		var raw string
		if err := tx.QueryRowContext(ctx, q).Scan(&raw); err != nil {
			return err
		}
		var cfg struct {
			Enabled  bool  `json:"astra_policy_enabled"`
			Epoch    int64 `json:"_astra_policy_epoch"`
			Failure  int64 `json:"astra_failure_group_id"`
			Recovery int64 `json:"astra_recovery_group_id"`
			Minutes  int   `json:"astra_recheck_minutes"`
		}
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return err
		}
		if !cfg.Enabled || cfg.Epoch != o.Epoch {
			return nil
		}
		if err := db.lockPolicyAccount(ctx, tx, o.AccountID); err != nil {
			return err
		}
		now := time.Now()
		stamp := now.Unix()
		membership, err := policyMembership(ctx, tx, o.AccountID)
		if err != nil {
			return err
		}
		s := AstraPolicyState{AccountID: o.AccountID, OriginalGroupIDs: []int64{}}
		err = tx.QueryRowContext(ctx, `SELECT state_json FROM codex_astra_policy WHERE account_id=$1`, o.AccountID).Scan(&raw)
		if err == nil {
			if err = decodeAstraState(raw, &s); err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		s.AccountID = o.AccountID
		if o.BatchSequence <= s.LastBatchSequence {
			return nil
		}
		s.LastBatchSequence = o.BatchSequence
		if s.Epoch != o.Epoch {
			s.ConsecutiveFailures = 0
			s.Epoch = o.Epoch
		}
		s.LastBatchID, s.LastOutcome, s.LastOutcomeAt, s.Error = o.BatchID, o.Outcome, stamp, ""
		s.LatestBatch = o.LatestBatch.Clone()
		owned := s.Demoted && membership.Version == s.OwnedVersion && slices.Equal(membership.GroupIDs, []int64{s.FailureGroupID})
		expected := membership.Version == o.Expected.Version && slices.Equal(membership.GroupIDs, o.Expected.GroupIDs)
		if s.Demoted && !owned {
			s.Demoted = false
			s.NextRecoveryAt = 0
			s.ConsecutiveFailures = 0
			s.Error = "manual_membership_changed"
		}
		if !s.Demoted && s.OwnedVersion != membership.Version {
			s.ConsecutiveFailures = 0
			s.OwnedVersion = membership.Version
		}
		if o.Outcome == "new_292" {
			s.ConsecutiveFailures = 0
			s.Last292At = stamp
			if o.Confirmed {
				s.LastConfirmed292At = stamp
			}
		}
		target := int64(0)
		if !expected {
			s.ConsecutiveFailures = 0
			s.Error = "manual_membership_changed"
		} else if o.Outcome == "ordinary_miss" && !s.Demoted {
			s.ConsecutiveFailures++
			if s.ConsecutiveFailures >= 2 {
				target = cfg.Failure
			}
		}
		if owned && expected && o.Outcome == "new_292" && o.Confirmed && o.ObtainedAtNano > s.DemotedAtNano && o.IssuedUnix >= s.DemotedAt && o.IssuedUnix <= stamp+60 && o.IssuedUnix+3600 > stamp {
			target = cfg.Recovery
		}
		if target != 0 {
			validation := error(nil)
			if cfg.Failure == cfg.Recovery {
				validation = errors.New("policy groups must be distinct")
			} else {
				for _, gid := range []int64{cfg.Failure, cfg.Recovery} {
					if e := validateAstraGroup(ctx, tx, db.isSQLite(), gid); e != nil {
						validation = e
						break
					}
				}
			}
			if validation != nil {
				s.Error = validation.Error()
			} else {
				if _, err := tx.ExecContext(ctx, `DELETE FROM account_group_members WHERE account_id=$1`, o.AccountID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO account_group_members(account_id,group_id) VALUES ($1,$2)`, o.AccountID, target); err != nil {
					return err
				}
				membership.Version++
				if s.Demoted {
					s.Demoted = false
					s.NextRecoveryAt = 0
					s.ConsecutiveFailures = 0
				} else {
					s.OriginalGroupIDs = append([]int64{}, membership.GroupIDs...)
					s.FailureGroupID = target
					s.Demoted = true
					s.DemotedAt = stamp
					s.DemotedAtNano = now.UnixNano()
					s.OwnedVersion = membership.Version
				}
				changed = true
			}
		}
		if s.Demoted {
			s.NextRecoveryAt = stamp + int64(max(cfg.Minutes, 1))*60
		}
		encoded, err := encodeAstraState(s)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO codex_astra_policy(account_id,state_json,membership_version) VALUES($1,$2,$3) ON CONFLICT(account_id) DO UPDATE SET state_json=excluded.state_json,membership_version=excluded.membership_version`, o.AccountID, string(encoded), membership.Version)
		return err
	})
	if err != nil {
		changed = false
	}
	return changed, err
}
