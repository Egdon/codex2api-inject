package database

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestAstraPolicyBatchOwnershipEpochAndRecovery(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	for _, q := range []string{`INSERT INTO accounts(id,name) VALUES(901,'astra-fixture')`, `INSERT INTO account_groups(id,name,channel) VALUES(901,'original','codex'),(902,'failure','codex'),(903,'recovery','codex')`} {
		if _, err := db.conn.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetAccountGroups(ctx, 901, []int64{901}); err != nil {
		t.Fatal(err)
	}
	settings := func(enabled bool, epoch int64) {
		t.Helper()
		if err := db.SaveTurnStateConfig(ctx, fmt.Sprintf(`{"astra_policy_enabled":%t,"_astra_policy_epoch":%d,"astra_failure_group_id":902,"astra_recovery_group_id":903,"astra_recheck_minutes":30}`, enabled, epoch)); err != nil {
			t.Fatal(err)
		}
	}
	settings(true, 1)
	apply := func(kind string, confirmed bool) AstraPolicyOutcome {
		t.Helper()
		expected, err := db.AstraPolicyExpectation(ctx, 901)
		if err != nil {
			t.Fatal(err)
		}
		o := AstraPolicyOutcome{AccountID: 901, BatchID: fmt.Sprint(expected.Sequence), BatchSequence: expected.Sequence, Epoch: 1, Expected: expected, Outcome: kind, Confirmed: confirmed, IssuedUnix: time.Now().Unix(), ObtainedAtNano: time.Now().UnixNano()}
		if _, err = db.ApplyAstraPolicyOutcome(ctx, o); err != nil {
			t.Fatal(err)
		}
		return o
	}
	snapshot := func() AstraPolicyState {
		t.Helper()
		s, err := db.AstraPolicySnapshot(ctx, 901)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := apply("ordinary_miss", false)
	if _, err := db.ApplyAstraPolicyOutcome(ctx, first); err != nil {
		t.Fatal(err)
	}
	apply("inconclusive", false)
	if s := snapshot(); s.ConsecutiveFailures != 1 || s.Demoted {
		t.Fatalf("duplicate/inconclusive %+v", s)
	}
	apply("ordinary_miss", false)
	if s := snapshot(); !s.Demoted || len(s.OriginalGroupIDs) != 1 || s.OriginalGroupIDs[0] != 901 {
		t.Fatalf("demotion %+v", s)
	}
	apply("new_292", false)
	if !snapshot().Demoted {
		t.Fatal("unconfirmed recovery")
	}
	apply("new_292", true)
	if snapshot().Demoted {
		t.Fatal("confirmed recovery failed")
	}
	groups, err := db.GetAccountGroupIDs(ctx, 901)
	if err != nil || len(groups) != 1 || groups[0] != 903 {
		t.Fatalf("groups %v %v", groups, err)
	}
	apply("ordinary_miss", false)
	apply("ordinary_miss", false)
	// Same-value human assignment relinquishes automatic ownership.
	if err := db.SetAccountGroups(ctx, 901, []int64{902}); err != nil {
		t.Fatal(err)
	}
	apply("new_292", true)
	groups, _ = db.GetAccountGroupIDs(ctx, 901)
	if len(groups) != 1 || groups[0] != 902 {
		t.Fatalf("manual assignment overwritten: %v", groups)
	}
	expected, _ := db.AstraPolicyExpectation(ctx, 901)
	stale := AstraPolicyOutcome{AccountID: 901, BatchID: "stale", BatchSequence: expected.Sequence, Epoch: 1, Expected: expected, Outcome: "ordinary_miss"}
	before := snapshot().LastBatchID
	settings(false, 1)
	if _, err := db.ApplyAstraPolicyOutcome(ctx, stale); err != nil {
		t.Fatal(err)
	}
	settings(true, 2)
	if _, err := db.ApplyAstraPolicyOutcome(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if snapshot().LastBatchID != before {
		t.Fatal("disabled/old epoch outcome applied")
	}
}
