package database

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

type astraPriorityWriteCase struct {
	name  string
	write func(context.Context, *DB, int64, map[string]interface{}) error
}

func astraPriorityWriteCases() []astraPriorityWriteCase {
	return []astraPriorityWriteCase{
		{"single_metadata", func(ctx context.Context, db *DB, id int64, updates map[string]interface{}) error {
			return db.UpdateAccountSchedulerMetadata(ctx, id, OptionalNullInt64{}, OptionalNullInt64{}, OptionalBool{}, OptionalInt64Slice{}, OptionalStringSlice{}, OptionalInt64Slice{}, OptionalString{}, updates)
		}},
		{"bulk_metadata", func(ctx context.Context, db *DB, id int64, updates map[string]interface{}) error {
			_, err := db.BatchUpdateAccountMetadata(ctx, []int64{id, id}, BatchAccountMetadataUpdate{CredentialUpdates: updates})
			return err
		}},
		{"generic_fast", func(ctx context.Context, db *DB, id int64, updates map[string]interface{}) error {
			return db.UpdateCredentials(ctx, id, updates)
		}},
		{"generic_identity_fallback", func(ctx context.Context, db *DB, id int64, updates map[string]interface{}) error {
			updates = cloneCredentialUpdates(updates)
			updates["access_token"] = "old-token"
			return db.UpdateCredentials(ctx, id, updates)
		}},
		{"generic_key_fallback", func(ctx context.Context, db *DB, id int64, updates map[string]interface{}) error {
			updates = cloneCredentialUpdates(updates)
			updates["custom.key"] = "value"
			return db.UpdateCredentials(ctx, id, updates)
		}},
		{"oauth_merge", func(ctx context.Context, db *DB, id int64, updates map[string]interface{}) error {
			return db.UpdateOAuthAccountCredentials(ctx, id, updates, "")
		}},
		{"responses_merge", func(ctx context.Context, db *DB, id int64, updates map[string]interface{}) error {
			return db.UpdateOpenAIResponsesAccount(ctx, id, "priority-fixture", updates, "")
		}},
	}
}

func seedAstraPriorityWriteAccount(t *testing.T, db *DB, id int64) {
	t.Helper()
	if _, err := db.conn.ExecContext(context.Background(), `INSERT INTO accounts(id,name,credentials,credential_generation) VALUES($1,'priority-fixture',$2,7)`, id, `{"scheduler_priority":20,"access_token":"old-token","refresh_token":"old-refresh","upstream_type":"openai_responses","api_key":"key","base_url":"https://example.invalid"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(context.Background(), `INSERT INTO codex_astra_policy(account_id,priority_version,membership_version) VALUES($1,3,5)`, id); err != nil {
		t.Fatal(err)
	}
}

func readAstraPriorityWriteAccount(t *testing.T, db *DB, id int64) (map[string]interface{}, int64, int64, int64) {
	t.Helper()
	var raw interface{}
	var generation, priorityVersion, membershipVersion int64
	if err := db.conn.QueryRowContext(context.Background(), `SELECT a.credentials,a.credential_generation,p.priority_version,p.membership_version FROM accounts a JOIN codex_astra_policy p ON p.account_id=a.id WHERE a.id=$1`, id).Scan(&raw, &generation, &priorityVersion, &membershipVersion); err != nil {
		t.Fatal(err)
	}
	return decodeCredentials(raw), generation, priorityVersion, membershipVersion
}

func TestAstraManualPriorityWritesIncludeSameValueAndNull(t *testing.T) {
	for _, writer := range astraPriorityWriteCases() {
		t.Run(writer.name, func(t *testing.T) {
			db := newGrokStateTestDB(t)
			ctx := context.Background()
			seedAstraPriorityWriteAccount(t, db, 901)
			// Presence, not a value comparison, establishes manual ownership.
			for i, value := range []interface{}{float64(20), float64(20), nil, nil} {
				if err := writer.write(ctx, db, 901, map[string]interface{}{"scheduler_priority": value}); err != nil {
					t.Fatal(err)
				}
				credentials, generation, version, membership := readAstraPriorityWriteAccount(t, db, 901)
				if priority, exists := credentials["scheduler_priority"]; !exists || !reflect.DeepEqual(priority, value) {
					t.Fatalf("priority = %#v, present = %t; want %#v", priority, exists, value)
				}
				if version != int64(4+i) || membership != 5 {
					t.Fatalf("versions priority=%d membership=%d; want %d/5", version, membership, 4+i)
				}
				if generation != 7 {
					t.Fatalf("configuration-only write changed credential generation to %d", generation)
				}
			}
		})
	}
}

func TestAstraUnrelatedCredentialWritesRetainPriorityVersion(t *testing.T) {
	for _, writer := range astraPriorityWriteCases() {
		t.Run(writer.name, func(t *testing.T) {
			db := newGrokStateTestDB(t)
			seedAstraPriorityWriteAccount(t, db, 901)
			// The stored map contains scheduler_priority, but the input does not.
			if err := writer.write(context.Background(), db, 901, map[string]interface{}{"refresh_token": "new-refresh", "expires_at": "2099-01-01T00:00:00Z"}); err != nil {
				t.Fatal(err)
			}
			credentials, _, version, membership := readAstraPriorityWriteAccount(t, db, 901)
			if version != 3 || membership != 5 || credentials["scheduler_priority"] != float64(20) {
				t.Fatalf("token merge invalidated priority ownership: priority=%v versions=%d/%d", credentials["scheduler_priority"], version, membership)
			}
			if credentials["refresh_token"] != "new-refresh" {
				t.Fatal("token update was not merged")
			}
		})
	}
}

func TestAstraBulkPriorityWritesEachActiveAccountOnce(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	for _, id := range []int64{901, 902, 903} {
		seedAstraPriorityWriteAccount(t, db, id)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET status='deleted' WHERE id=903`); err != nil {
		t.Fatal(err)
	}
	ids, err := db.BatchUpdateAccountMetadata(ctx, []int64{902, 903, 901, 902, 999}, BatchAccountMetadataUpdate{CredentialUpdates: map[string]interface{}{"scheduler_priority": 20}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []int64{901, 902}) {
		t.Fatalf("updated IDs = %v; want deterministic active IDs", ids)
	}
	for _, id := range []int64{901, 902, 903} {
		_, generation, version, membership := readAstraPriorityWriteAccount(t, db, id)
		want := int64(4)
		if id == 903 {
			want = 3
		}
		if version != want || generation != 7 || membership != 5 {
			t.Fatalf("account %d: version=%d generation=%d membership=%d", id, version, generation, membership)
		}
	}
}

func TestAstraForceDeleteCurrentGroupSuppressesOnlyGroupAction(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	f := newAstraPriorityFixture(t, db)
	f.cfg.GroupBatches, f.cfg.PriorityBatches = 2, 2
	f.save()
	f.miss()
	before := f.state()
	if before.ConsecutiveFailures != 1 || before.Demoted || before.PriorityDemoted {
		t.Fatalf("expected one miss below both thresholds: %+v", before)
	}
	_, _, priorityVersion, membershipVersion := readAstraPriorityWriteAccount(t, db, f.id)
	// Delete the current group, not either configured policy destination.
	if err := db.DeleteAccountGroup(ctx, f.original, true); err != nil {
		t.Fatal(err)
	}
	after := f.state()
	if !after.GroupSuppressed || after.PrioritySuppressed || after.ConsecutiveFailures != 1 {
		t.Fatalf("group deletion must suppress only groups and preserve streak: %+v", after)
	}
	_, _, newPriorityVersion, newMembershipVersion := readAstraPriorityWriteAccount(t, db, f.id)
	if newPriorityVersion != priorityVersion || newMembershipVersion != membershipVersion+1 {
		t.Fatalf("group deletion changed wrong versions: priority %d -> %d, groups %d -> %d", priorityVersion, newPriorityVersion, membershipVersion, newMembershipVersion)
	}
	f.miss()
	after = f.state()
	if after.ConsecutiveFailures != 2 || after.Demoted || !after.GroupSuppressed || !after.PriorityDemoted || after.PrioritySuppressed {
		t.Fatalf("next miss must leave groups alone while priority acts independently: %+v", after)
	}
	expected, err := db.AstraPolicyExpectation(ctx, f.id)
	if err != nil {
		t.Fatal(err)
	}
	if len(expected.GroupIDs) != 0 || expected.Priority != *f.cfg.FailurePriority {
		t.Fatalf("force-deleted group overwritten or priority action skipped: %+v", expected)
	}
}

func TestAstraDeleteGroupSkipsOrphansAndMarksDeletedAccounts(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	seedAstraPriorityWriteAccount(t, db, 901)
	for _, query := range []string{
		`UPDATE accounts SET status='deleted' WHERE id=901`,
		`INSERT INTO account_groups(id,name) VALUES(901,'deleted-members')`,
		`INSERT INTO account_group_members(account_id,group_id) VALUES(999,901),(901,901)`,
	} {
		if _, err := db.conn.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	// No active members: the ordinary deletion path also removes memberships.
	if err := db.DeleteAccountGroup(ctx, 901); err != nil {
		t.Fatal(err)
	}
	_, generation, priorityVersion, membershipVersion := readAstraPriorityWriteAccount(t, db, 901)
	if generation != 7 || priorityVersion != 3 || membershipVersion != 6 {
		t.Fatalf("deleted account versions: generation=%d priority=%d membership=%d", generation, priorityVersion, membershipVersion)
	}
	var count int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_group_members WHERE group_id=901`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("memberships remain: %d, %v", count, err)
	}
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM codex_astra_policy WHERE account_id=999`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("orphan created policy state: %d, %v", count, err)
	}
}

func TestAstraManualPriorityWriteRollbackIsAtomic(t *testing.T) {
	for _, writer := range astraPriorityWriteCases() {
		t.Run(writer.name, func(t *testing.T) {
			db := newGrokStateTestDB(t)
			ctx := context.Background()
			seedAstraPriorityWriteAccount(t, db, 901)
			before, _, _, _ := readAstraPriorityWriteAccount(t, db, 901)
			// Force the ownership write to fail after (or before) the credentials
			// write; neither document nor versions may escape the transaction.
			if _, err := db.conn.ExecContext(ctx, `CREATE TRIGGER reject_priority_write BEFORE UPDATE OF priority_version ON codex_astra_policy BEGIN SELECT RAISE(ABORT, 'priority write rejected'); END`); err != nil {
				t.Fatal(err)
			}
			if err := writer.write(ctx, db, 901, map[string]interface{}{"scheduler_priority": 99, "expires_at": "changed"}); err == nil {
				t.Fatal("expected rejected ownership update")
			}
			after, generation, version, membership := readAstraPriorityWriteAccount(t, db, 901)
			beforeJSON, _ := json.Marshal(before)
			afterJSON, _ := json.Marshal(after)
			if string(beforeJSON) != string(afterJSON) || generation != 7 || version != 3 || membership != 5 {
				t.Fatalf("partial write escaped rollback: credentials=%s generation=%d versions=%d/%d", afterJSON, generation, version, membership)
			}
		})
	}
}
