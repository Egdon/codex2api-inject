package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
)

type astraPriorityFixture struct {
	t                               *testing.T
	db                              *DB
	id, original, failure, recovery int64
	cfg                             astraPolicyConfig
}

func newAstraPriorityFixture(t *testing.T, db *DB) *astraPriorityFixture {
	t.Helper()
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "astra-priority", map[string]any{"scheduler_priority": 7, "access_token": "fixture-only"}, "")
	if err != nil {
		t.Fatal(err)
	}
	f := &astraPriorityFixture{t: t, db: db, id: id}
	groups := []*int64{&f.original, &f.failure, &f.recovery}
	for i, target := range groups {
		*target, err = db.CreateAccountGroup(ctx, fmt.Sprintf("astra-%d-%d-%d", id, time.Now().UnixNano(), i), "", "", 0, 0, sql.NullInt64{})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = db.SetAccountGroups(ctx, id, []int64{f.original}); err != nil {
		t.Fatal(err)
	}
	f.cfg, err = decodeAstraPolicyConfig(`{"astra_policy_enabled":true,"astra_priority_policy_enabled":true,"_astra_policy_epoch":1}`)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.Failure, f.cfg.Recovery = f.failure, f.recovery
	f.save()
	return f
}
func (f *astraPriorityFixture) save() {
	f.t.Helper()
	raw, err := json.Marshal(f.cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	if err = f.db.SaveTurnStateConfig(context.Background(), string(raw)); err != nil {
		f.t.Fatal(err)
	}
}
func (f *astraPriorityFixture) outcome(kind string, confirmed bool) AstraPolicyOutcome {
	f.t.Helper()
	e, err := f.db.AstraPolicyExpectation(context.Background(), f.id)
	if err != nil {
		f.t.Fatal(err)
	}
	return AstraPolicyOutcome{AccountID: f.id, BatchID: fmt.Sprint(e.Sequence), BatchSequence: e.Sequence, Expected: e, Epoch: f.cfg.Epoch, Outcome: kind, Confirmed: confirmed, ObtainedAtNano: time.Now().UnixNano(), IssuedUnix: time.Now().Unix()}
}
func (f *astraPriorityFixture) apply(o AstraPolicyOutcome) bool {
	f.t.Helper()
	changed, err := f.db.ApplyAstraPolicyOutcome(context.Background(), o)
	if err != nil {
		f.t.Fatal(err)
	}
	return changed
}
func (f *astraPriorityFixture) miss() { f.t.Helper(); f.apply(f.outcome("ordinary_miss", false)) }
func (f *astraPriorityFixture) state() AstraPolicyState {
	f.t.Helper()
	s, err := f.db.AstraPolicySnapshot(context.Background(), f.id)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}
func (f *astraPriorityFixture) expectValues(group int64, priority int) {
	f.t.Helper()
	e, err := f.db.AstraPolicyExpectation(context.Background(), f.id)
	if err != nil || !slices.Equal(e.GroupIDs, []int64{group}) || e.Priority != priority {
		f.t.Fatalf("properties %+v, err %v; want group %d priority %d", e, err, group, priority)
	}
}
func (f *astraPriorityFixture) manual(priority bool) {
	f.t.Helper()
	ctx := context.Background()
	e, err := f.db.AstraPolicyExpectation(ctx, f.id)
	if err != nil {
		f.t.Fatal(err)
	}
	if priority {
		err = f.db.UpdateCredentials(ctx, f.id, map[string]any{"scheduler_priority": e.Priority})
	} else {
		err = f.db.SetAccountGroups(ctx, f.id, e.GroupIDs)
	}
	if err != nil {
		f.t.Fatal(err)
	}
}

func TestAstraPolicyPriority(t *testing.T) { runAstraPriorityCases(t, newGrokStateTestDB(t)) }

// Kept as a dedicated entrypoint for PostgreSQL compatibility CI. Never connects
// unless the explicitly isolated integration DSN is supplied by the test runner.
func TestPostgresAstraPolicyPriority(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("requires isolated CODEX2API_TEST_POSTGRES_DSN")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runAstraPriorityCases(t, db)
}

func runAstraPriorityCases(t *testing.T, db *DB) {
	t.Run("scheduler_priority_decoding", func(t *testing.T) {
		f := newAstraPriorityFixture(t, db)
		for _, tc := range []struct {
			value any
			want  int
		}{
			{"999999999999999999999999999999999", 0},
			{float64(1000000), 100},
			{1.5, 0},
			{"-12", -12},
			{nil, 0},
		} {
			if err := db.UpdateCredentials(context.Background(), f.id, map[string]any{"scheduler_priority": tc.value}); err != nil {
				t.Fatal(err)
			}
			f.expectValues(f.original, tc.want)
		}
	})
	for _, thresholds := range [][2]int{{2, 5}, {5, 2}} {
		t.Run(fmt.Sprintf("thresholds_%d_%d", thresholds[0], thresholds[1]), func(t *testing.T) {
			f := newAstraPriorityFixture(t, db)
			f.cfg.GroupBatches, f.cfg.PriorityBatches = thresholds[0], thresholds[1]
			f.save()
			before, err := db.GetAccountByID(context.Background(), f.id)
			if err != nil {
				t.Fatal(err)
			}
			for n := 1; n <= 7; n++ {
				f.miss()
				s := f.state()
				if s.ConsecutiveFailures != n || s.Demoted != (n >= thresholds[0]) || s.PriorityDemoted != (n >= thresholds[1]) {
					t.Fatalf("batch %d: %+v", n, s)
				}
			}
			f.expectValues(f.failure, -1)
			s := f.state()
			f.miss()
			if next := f.state(); next.OwnedVersion != s.OwnedVersion || next.PriorityOwnedVersion != s.PriorityOwnedVersion {
				t.Fatal("action fired twice")
			}
			f.apply(f.outcome("new_292", false))
			if s = f.state(); s.ConsecutiveFailures != 0 || !s.Demoted || !s.PriorityDemoted {
				t.Fatalf("unconfirmed reset %+v", s)
			}
			if !f.apply(f.outcome("new_292", true)) {
				t.Fatal("recovery did not change values")
			}
			f.expectValues(f.recovery, 0)
			if s = f.state(); s.GroupTriggered || s.PriorityTriggered || s.Demoted || s.PriorityDemoted {
				t.Fatalf("recovery latches %+v", s)
			}
			after, err := db.GetAccountByID(context.Background(), f.id)
			if err != nil || after.CredentialGeneration != before.CredentialGeneration || after.GetCredential("access_token") != "fixture-only" {
				t.Fatalf("policy changed identity: %v", err)
			}
		})
	}
	t.Run("priority_only_explicit_zero", func(t *testing.T) {
		f := newAstraPriorityFixture(t, db)
		zero := 0
		f.cfg.Enabled, f.cfg.FailurePriority, f.cfg.RecoveryPriority = false, &zero, 3
		f.cfg.Failure, f.cfg.Recovery = 0, 0
		f.save()
		f.miss()
		f.expectValues(f.original, 0)
		if s := f.state(); s.Demoted || !s.PriorityTriggered {
			t.Fatalf("priority-only %+v", s)
		}
		f.apply(f.outcome("new_292", true))
		f.expectValues(f.original, 3)
	})
	t.Run("canonical_priority_assignment", func(t *testing.T) {
		f := newAstraPriorityFixture(t, db)
		zero := 0
		f.cfg.Enabled, f.cfg.FailurePriority = false, &zero
		f.save()
		if err := db.UpdateCredentials(context.Background(), f.id, map[string]any{"scheduler_priority": nil}); err != nil {
			t.Fatal(err)
		}
		if !f.apply(f.outcome("ordinary_miss", false)) {
			t.Fatal("canonical assignment did not request refresh")
		}
		row, err := db.GetAccountByID(context.Background(), f.id)
		if err != nil {
			t.Fatal(err)
		}
		if priority, valid := row.GetCredentialInt64("scheduler_priority"); !valid || priority != 0 {
			t.Fatal("explicit zero was not persisted")
		}
		version := f.state().PriorityOwnedVersion
		if f.apply(f.outcome("ordinary_miss", false)) || f.state().PriorityOwnedVersion != version {
			t.Fatal("canonical assignment repeated within episode")
		}
	})
	t.Run("invalid_group_independent_priority", func(t *testing.T) {
		f := newAstraPriorityFixture(t, db)
		f.cfg.GroupBatches, f.cfg.Failure = 1, 0
		f.save()
		f.miss()
		f.expectValues(f.original, -1)
		if s := f.state(); s.Error == "" || s.GroupTriggered || !s.PriorityTriggered {
			t.Fatalf("invalid group %+v", s)
		}
	})
	for _, priority := range []bool{false, true} {
		t.Run(fmt.Sprintf("manual_same_value_priority_%t", priority), func(t *testing.T) {
			f := newAstraPriorityFixture(t, db)
			f.cfg.GroupBatches, f.cfg.PriorityBatches = 2, 2
			f.save()
			// Same-value initial writes do not suppress a future episode.
			f.manual(priority)
			if s := f.state(); s.GroupSuppressed || s.PrioritySuppressed {
				t.Fatal("initial manual write suppressed")
			}
			f.miss()
			late := f.outcome("new_292", true)
			f.manual(priority)
			f.apply(late)
			s := f.state()
			if s.ConsecutiveFailures != 0 || (priority && !s.PrioritySuppressed) || (!priority && !s.GroupSuppressed) {
				t.Fatalf("late success cleared suppression %+v", s)
			}
			f.miss()
			f.miss()
			if priority {
				f.expectValues(f.failure, 7)
			} else {
				f.expectValues(f.original, -1)
			}
			s = f.state()
			if s.ConsecutiveFailures != 2 || (priority && (!s.GroupTriggered || s.PriorityTriggered)) || (!priority && (!s.PriorityTriggered || s.GroupTriggered)) {
				t.Fatalf("independent manual %+v", s)
			}
			f.apply(f.outcome("new_292", true))
			if s = f.state(); s.GroupSuppressed || s.PrioritySuppressed {
				t.Fatalf("fresh success did not rearm %+v", s)
			}
			f.miss()
			f.miss()
			f.expectValues(f.failure, -1)
			// Same-value manual assignment after BOTH actions fired loses only
			// corresponding ownership and does not restore that property.
			f.manual(priority)
			f.apply(f.outcome("new_292", true))
			if priority {
				f.expectValues(f.recovery, -1)
			} else {
				f.expectValues(f.failure, 0)
			}
		})
		t.Run(fmt.Sprintf("manual_during_first_batch_%t", priority), func(t *testing.T) {
			f := newAstraPriorityFixture(t, db)
			f.cfg.GroupBatches, f.cfg.PriorityBatches = 2, 2
			f.save()
			late := f.outcome("ordinary_miss", false)
			f.manual(priority)
			f.apply(late)
			f.miss()
			if priority {
				f.expectValues(f.failure, 7)
			} else {
				f.expectValues(f.original, -1)
			}
		})
	}
	t.Run("epoch_preserves_owned_and_suppressed", func(t *testing.T) {
		f := newAstraPriorityFixture(t, db)
		f.cfg.GroupBatches, f.cfg.PriorityBatches = 2, 5
		f.save()
		f.miss()
		f.miss()
		owned := f.state().OwnedVersion
		old := f.outcome("ordinary_miss", false)
		f.cfg.Epoch++
		f.cfg.Failure = f.original
		f.cfg.RecoveryPriority = 9
		f.save()
		if f.apply(old) {
			t.Fatal("old epoch changed properties")
		}
		for n := 1; n <= 5; n++ {
			f.miss()
			if s := f.state(); s.ConsecutiveFailures != n || !s.GroupTriggered || s.OwnedVersion != owned {
				t.Fatalf("epoch lost ownership %+v", s)
			}
		}
		f.expectValues(f.failure, -1)
		f.manual(false)
		f.cfg.Epoch++
		f.save()
		f.miss()
		if s := f.state(); !s.GroupSuppressed || !s.GroupTriggered || !s.PriorityDemoted || s.ConsecutiveFailures != 1 {
			t.Fatalf("epoch erased state %+v", s)
		}
		f.apply(f.outcome("new_292", true))
		f.expectValues(f.failure, 9)
	})
	t.Run("both_disabled_no_work", func(t *testing.T) {
		f := newAstraPriorityFixture(t, db)
		f.miss()
		before := f.state()
		f.cfg.Enabled, f.cfg.PriorityEnabled = false, false
		f.save()
		if f.apply(f.outcome("new_292", true)) {
			t.Fatal("disabled action changed properties")
		}
		if after := f.state(); after.LastBatchSequence != before.LastBatchSequence || after.ConsecutiveFailures != before.ConsecutiveFailures {
			t.Fatalf("disabled state changed %+v", after)
		}
	})
	t.Run("legacy_group_ownership", func(t *testing.T) {
		f := newAstraPriorityFixture(t, db)
		f.cfg.PriorityEnabled = false
		f.save()
		f.miss()
		f.miss()
		s := f.state()
		raw := fmt.Sprintf(`{"account_id":%d,"demoted":true,"failure_group_id":%d,"owned_version":%d,"demoted_at":%d,"demoted_at_nano":%d,"epoch":1}`, f.id, f.failure, s.OwnedVersion, s.DemotedAt, s.DemotedAtNano)
		if _, err := db.conn.ExecContext(context.Background(), `UPDATE codex_astra_policy SET state_json=$1 WHERE account_id=$2`, raw, f.id); err != nil {
			t.Fatal(err)
		}
		if s = f.state(); !s.GroupTriggered || !s.Demoted || s.PriorityTriggered {
			t.Fatalf("legacy decode %+v", s)
		}
		f.apply(f.outcome("new_292", true))
		f.expectValues(f.recovery, 7)
	})
}

func TestAstraPolicyPriorityRollbackIsAtomic(t *testing.T) {
	db := newGrokStateTestDB(t)
	f := newAstraPriorityFixture(t, db)
	f.cfg.GroupBatches, f.cfg.PriorityBatches = 1, 1
	f.save()
	ctx := context.Background()
	before, err := db.AstraPolicyExpectation(ctx, f.id)
	if err != nil {
		t.Fatal(err)
	}
	// Group replacement happens first. Failure of the priority update must
	// roll back that replacement, both versions, and the outcome receipt.
	if _, err = db.conn.ExecContext(ctx, `CREATE TRIGGER reject_astra_priority BEFORE UPDATE OF credentials ON accounts BEGIN SELECT RAISE(ABORT,'priority rejected'); END`); err != nil {
		t.Fatal(err)
	}
	o := f.outcome("ordinary_miss", false)
	changed, err := db.ApplyAstraPolicyOutcome(ctx, o)
	if err == nil || changed {
		t.Fatalf("rollback result %t %v", changed, err)
	}
	f.expectValues(f.original, 7)
	after, err := db.AstraPolicyExpectation(ctx, f.id)
	if err != nil || after.Version != before.Version || after.PriorityVersion != before.PriorityVersion {
		t.Fatalf("versions escaped rollback %+v %v", after, err)
	}
	if s := f.state(); s.LastBatchSequence != 0 || s.ConsecutiveFailures != 0 || s.GroupTriggered || s.PriorityTriggered {
		t.Fatalf("state escaped rollback %+v", s)
	}
}

func TestAstraPolicyPriorityConfigDefaults(t *testing.T) {
	cfg, err := decodeAstraPolicyConfig(`{}`)
	if err != nil || cfg.Enabled || cfg.PriorityEnabled || cfg.GroupBatches != 2 || cfg.PriorityBatches != 1 || *cfg.FailurePriority != -1 || cfg.RecoveryPriority != 0 {
		t.Fatalf("defaults %+v %v", cfg, err)
	}
	cfg, err = decodeAstraPolicyConfig(`{"astra_failure_priority":0,"astra_group_failure_batches":50,"astra_priority_failure_batches":5}`)
	if err != nil || *cfg.FailurePriority != 0 || cfg.GroupBatches != 50 || cfg.PriorityBatches != 5 {
		t.Fatalf("explicit values %+v %v", cfg, err)
	}
	cfg, err = decodeAstraPolicyConfig(`{"astra_group_failure_batches":-1,"astra_priority_failure_batches":99}`)
	if err != nil || cfg.GroupBatches != 1 || cfg.PriorityBatches != 50 {
		t.Fatalf("clamps %+v %v", cfg, err)
	}
	cfg, err = decodeAstraPolicyConfig(`{"astra_group_failure_batches":0,"astra_priority_failure_batches":0}`)
	if err != nil || cfg.GroupBatches != 2 || cfg.PriorityBatches != 1 {
		t.Fatalf("zero defaults %+v %v", cfg, err)
	}
}

func TestAstraPolicyPrioritySQLiteAdditiveSchema(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	// Recreate the old, empty policy table to exercise the additive migration.
	if _, err := db.conn.ExecContext(ctx, `DROP TABLE codex_astra_policy`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE TABLE codex_astra_policy(account_id BIGINT PRIMARY KEY,state_json TEXT NOT NULL DEFAULT '{}',membership_version BIGINT NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO codex_astra_policy(account_id,state_json,membership_version) VALUES(123,'{"demoted":true}',4)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.ensureAstraPolicySchema(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var version, priorityVersion int64
	if err := db.conn.QueryRowContext(ctx, `SELECT membership_version,priority_version FROM codex_astra_policy WHERE account_id=123`).Scan(&version, &priorityVersion); err != nil || version != 4 || priorityVersion != 0 {
		t.Fatalf("migration %d %d %v", version, priorityVersion, err)
	}
	if s, err := db.AstraPolicySnapshot(ctx, 123); err != nil || !s.GroupTriggered || !s.Demoted {
		t.Fatalf("legacy migration %+v %v", s, err)
	}
}
