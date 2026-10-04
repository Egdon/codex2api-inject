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
	PriorityDemoted          bool                `json:"priority_demoted"`
	GroupTriggered           bool                `json:"group_triggered"`
	PriorityTriggered        bool                `json:"priority_triggered"`
	GroupSuppressed          bool                `json:"group_suppressed"`
	PrioritySuppressed       bool                `json:"priority_suppressed"`
	FailurePriority          int                 `json:"failure_priority"`
	PriorityDemotedAt        int64               `json:"priority_demoted_at"`
	GroupSuppressedAt        int64               `json:"group_suppressed_at"`
	PrioritySuppressedAt     int64               `json:"priority_suppressed_at"`
	PriorityOwnedVersion     int64               `json:"-"`
	PriorityDemotedAtNano    int64               `json:"-"`
	GroupSuppressedAtNano    int64               `json:"-"`
	PrioritySuppressedAtNano int64               `json:"-"`
	LatestBatch              *AstraBatchEvidence `json:"latest_batch,omitempty"`
	AccountID                int64               `json:"account_id"`
	Enabled                  bool                `json:"enabled"`
	ConsecutiveFailures      int                 `json:"consecutive_failures"`
	Demoted                  bool                `json:"demoted"`
	FailureGroupID           int64               `json:"failure_group_id"`
	OriginalGroupIDs         []int64             `json:"original_group_ids"`
	DemotedAt                int64               `json:"demoted_at"`
	LastBatchID              string              `json:"last_batch_id"`
	LastOutcome              string              `json:"last_outcome"`
	LastOutcomeAt            int64               `json:"last_outcome_at"`
	Last292At                int64               `json:"last_292_at"`
	LastConfirmed292At       int64               `json:"last_confirmed_292_at"`
	NextRecoveryAt           int64               `json:"next_recovery_at"`
	Error                    string              `json:"error"`
	Epoch                    int64               `json:"-"`
	OwnedVersion             int64               `json:"-"`
	LastBatchSequence        int64               `json:"-"`
	DemotedAtNano            int64               `json:"-"`
}

type astraPolicyStored struct {
	AstraPolicyState
	Epoch                    int64 `json:"epoch"`
	OwnedVersion             int64 `json:"owned_version"`
	LastBatchSequence        int64 `json:"last_batch_sequence"`
	DemotedAtNano            int64 `json:"demoted_at_nano"`
	PriorityOwnedVersion     int64 `json:"priority_owned_version"`
	PriorityDemotedAtNano    int64 `json:"priority_demoted_at_nano"`
	GroupSuppressedAtNano    int64 `json:"group_suppressed_at_nano"`
	PrioritySuppressedAtNano int64 `json:"priority_suppressed_at_nano"`
}

func encodeAstraState(s AstraPolicyState) ([]byte, error) {
	return json.Marshal(astraPolicyStored{AstraPolicyState: s, Epoch: s.Epoch, OwnedVersion: s.OwnedVersion, LastBatchSequence: s.LastBatchSequence, DemotedAtNano: s.DemotedAtNano,
		PriorityOwnedVersion: s.PriorityOwnedVersion, PriorityDemotedAtNano: s.PriorityDemotedAtNano, GroupSuppressedAtNano: s.GroupSuppressedAtNano, PrioritySuppressedAtNano: s.PrioritySuppressedAtNano})
}
func decodeAstraState(raw string, s *AstraPolicyState) error {
	var stored astraPolicyStored
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return err
	}
	*s = stored.AstraPolicyState
	s.LastBatchSequence = stored.LastBatchSequence
	s.Epoch, s.OwnedVersion, s.DemotedAtNano = stored.Epoch, stored.OwnedVersion, stored.DemotedAtNano
	s.PriorityOwnedVersion, s.PriorityDemotedAtNano = stored.PriorityOwnedVersion, stored.PriorityDemotedAtNano
	s.GroupSuppressedAtNano, s.PrioritySuppressedAtNano = stored.GroupSuppressedAtNano, stored.PrioritySuppressedAtNano
	// Old records only carried the group-owned flag. Preserve that action.
	s.GroupTriggered = s.GroupTriggered || s.Demoted
	s.PriorityTriggered = s.PriorityTriggered || s.PriorityDemoted
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
	if db.isSQLite() {
		return db.ensureSQLiteColumn(ctx, "codex_astra_policy", "priority_version", "BIGINT NOT NULL DEFAULT 0")
	}
	_, err := db.conn.ExecContext(ctx, `ALTER TABLE codex_astra_policy ADD COLUMN IF NOT EXISTS priority_version BIGINT NOT NULL DEFAULT 0`)
	return err
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

// markManualPolicyGroups invalidates only group ownership, even for same-value
// assignments. Call inside the membership write transaction, before its writes.
func (db *DB) markManualPolicyGroups(ctx context.Context, tx *sql.Tx, ids []int64) error {
	ids = append([]int64(nil), ids...)
	slices.Sort(ids)
	for _, id := range slices.Compact(ids) {
		if err := db.markManualPolicyProperty(ctx, tx, id, false); err != nil {
			return err
		}
	}
	return nil
}

type AstraPolicyExpectation struct {
	GroupIDs        []int64
	Version         int64
	Sequence        int64
	Priority        int
	PriorityVersion int64
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
	err = tx.QueryRowContext(ctx, `SELECT membership_version,priority_version FROM codex_astra_policy WHERE account_id=$1`, id).Scan(&out.Version, &out.PriorityVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	out.Priority, err = policyPriority(ctx, tx, id)
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

// ApplyAstraPolicyOutcome checks settings before locking the account and commits
// both independent actions atomically. The caller refreshes groups AND priority
// after a true result; no precomputed runtime value is safe to publish.
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
		cfg, err := decodeAstraPolicyConfig(raw)
		if err != nil {
			return err
		}
		if (!cfg.Enabled && !cfg.PriorityEnabled) || cfg.Epoch != o.Epoch {
			return nil
		}
		if err := db.lockPolicyAccount(ctx, tx, o.AccountID); err != nil {
			return err
		}
		current, err := policyMembership(ctx, tx, o.AccountID)
		if err != nil {
			return err
		}
		s, err := loadAstraPolicyState(ctx, tx, o.AccountID)
		if err != nil {
			return err
		}
		if o.BatchSequence <= s.LastBatchSequence {
			return nil
		}
		if s.Epoch != o.Epoch {
			s.ConsecutiveFailures = 0
			s.Epoch = o.Epoch
		}
		s.LastBatchSequence = o.BatchSequence
		now := time.Now()
		s.LastBatchID, s.LastOutcome, s.LastOutcomeAt, s.Error = o.BatchID, o.Outcome, now.Unix(), ""
		s.LatestBatch = o.LatestBatch.Clone()
		changed, err = db.applyAstraPolicyActions(ctx, tx, &s, &current, cfg, o, now)
		if err != nil {
			return err
		}
		return saveAstraPolicyState(ctx, tx, s, current.Version, current.PriorityVersion)
	})
	if err != nil {
		changed = false
	}
	return changed, err
}
