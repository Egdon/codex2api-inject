package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestExcelBPSDatabase(t *testing.T) { runExcelBPSDatabaseCases(t, newGrokStateTestDB(t)) }

// Explicit integration entrypoint; no provider/model traffic is used.
func TestPostgresExcelBPSDatabase(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("requires isolated CODEX2API_TEST_POSTGRES_DSN")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runExcelBPSDatabaseCases(t, db)
	t.Run("legacy_additive_migration", func(t *testing.T) { runExcelBPSLegacyMigration(t, db) })
}

func runExcelBPSDatabaseCases(t *testing.T, db *DB) {
	ctx := context.Background()
	t.Run("master_setting", func(t *testing.T) {
		if _, err := db.conn.ExecContext(ctx, `DELETE FROM system_settings WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		if enabled, err := db.GetOpenAIExcelBPSEnabled(ctx); err != nil || !enabled {
			t.Fatalf("missing setting: %t %v", enabled, err)
		}
		for _, want := range []bool{false, true, false} {
			if err := db.SaveOpenAIExcelBPSEnabled(ctx, want); err != nil {
				t.Fatal(err)
			}
			if got, err := db.GetOpenAIExcelBPSEnabled(ctx); err != nil || got != want {
				t.Fatalf("setting %t: %t %v", want, got, err)
			}
		}
		if _, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET openai_excel_bps_enabled=NULL WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		if enabled, err := db.GetOpenAIExcelBPSEnabled(ctx); err != nil || !enabled {
			t.Fatalf("NULL setting: %t %v", enabled, err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if enabled, err := db.GetOpenAIExcelBPSEnabled(cancelled); err == nil || enabled {
			t.Fatalf("real error silently enabled BPS: %t %v", enabled, err)
		}
		if enabled, err := (*DB)(nil).GetOpenAIExcelBPSEnabled(ctx); err == nil || enabled {
			t.Fatal("unavailable database silently enabled BPS")
		}
	})

	t.Run("source_logs_filters_and_counters", func(t *testing.T) {
		id, err := db.InsertAccountWithCredentials(ctx, "bps-source-fixture", map[string]any{"openai_excel_bps": true}, "")
		if err != nil {
			t.Fatal(err)
		}
		for i, source := range []string{" BPS ", "codex", "relay", "", "unknown"} {
			if err := db.InsertUsageLog(ctx, &UsageLogInput{AccountID: id, Channel: "codex", UpstreamSource: source, Model: "gpt-6-astra", Endpoint: "/v1/responses", InboundEndpoint: "/v1/responses", StatusCode: 200, TotalTokens: (i + 1) * 10, InputTokens: 10, OutputTokens: 1}); err != nil {
				t.Fatal(err)
			}
		}
		db.FlushUsageLogs()
		// Legacy/relay endpoint evidence must not be inferred from today's BPS flag.
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO usage_logs(account_id,channel,endpoint,model,status_code,total_tokens,upstream_endpoint) VALUES($1,'codex','/v1/responses','gpt-6-astra',500,60,'https://relay.invalid/backend-api/codex/responses')`, id); err != nil {
			t.Fatal(err)
		}
		if err := db.UpdateCredentials(ctx, id, map[string]any{"openai_excel_bps": false}); err != nil {
			t.Fatal(err)
		}
		start, end := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
		all, err := db.GetUsageStats(ctx, start, end, "codex")
		if err != nil {
			t.Fatal(err)
		}
		keys := map[string]bool{}
		for _, tc := range []struct {
			source        string
			count, tokens int64
		}{{"", 6, 210}, {"bps", 1, 10}, {"codex", 1, 20}, {"other", 1, 30}, {"unknown", 3, 150}} {
			filter := UsageLogFilter{Start: start, End: end, AccountID: &id, UpstreamSource: tc.source, Page: 1, PageSize: 50}
			page, err := db.ListUsageLogsByTimeRangePaged(ctx, filter)
			if err != nil || page.Total != tc.count || int64(len(page.Logs)) != tc.count {
				t.Fatalf("source %q list: %+v %v", tc.source, page, err)
			}
			rows, err := db.ListUsageLogsByFilter(ctx, filter)
			if err != nil || int64(len(rows)) != tc.count {
				t.Fatalf("export %q: %d %v", tc.source, len(rows), err)
			}
			for _, row := range rows {
				if tc.source != "" && row.UpstreamSource != NormalizeUpstreamSource(tc.source) {
					t.Fatalf("source changed with account: %+v", row)
				}
			}
			stats, err := db.GetUsageStatsFiltered(ctx, start, end, "codex", filter, true)
			if err != nil {
				t.Fatal(err)
			}
			if stats.TodayRequests != tc.count || stats.TodayTokens != tc.tokens || stats.TotalRequests != all.TotalRequests || stats.TotalTokens != all.TotalTokens {
				t.Fatalf("source %q counters: %+v", tc.source, stats)
			}
			if len(stats.ModelStats) != 1 || len(stats.EndpointStats) != 1 {
				t.Fatalf("source breakdowns missing: %+v", stats)
			}
			if keys[filter.DimensionKey()] {
				t.Fatal("source cache key collision")
			}
			keys[filter.DimensionKey()] = true
			errors, err := db.GetUsageErrorSummary(ctx, filter)
			wantErrors := int64(0)
			if tc.source == "" || tc.source == "unknown" {
				wantErrors = 1
			}
			if err != nil || errors.TotalErrors != wantErrors {
				t.Fatalf("source errors: %+v %v", errors, err)
			}
		}
		for _, list := range []func() ([]*UsageLog, error){func() ([]*UsageLog, error) { return db.ListRecentUsageLogs(ctx, 500) }, func() ([]*UsageLog, error) { return db.ListUsageLogsByTimeRange(ctx, start, end) }} {
			rows, err := list()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range rows {
				if row.AccountID == id && row.UpstreamSource == "bps" {
					found = true
				}
			}
			if !found {
				t.Fatal("unfiltered list lost stored source")
			}
		}
		if !(UsageLogFilter{UpstreamSource: "unknown"}).HasDimensionFilter() {
			t.Fatal("source is not a dimension")
		}
	})

	t.Run("quality_source_snapshot", func(t *testing.T) {
		id, err := db.InsertAccountWithCredentials(ctx, "quality-bps", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		job, err := db.CreateQualityTestJob(ctx, QualityTestJob{AccountID: id, AccountName: "fixture", Model: "gpt-6-astra"})
		if err != nil {
			t.Fatal(err)
		}
		if job.UpstreamSource != "" {
			t.Fatal("fabricated source before request")
		}
		job.UpstreamSource = " BPS "
		if err := db.SaveQualityTestProgress(ctx, *job); err != nil {
			t.Fatal(err)
		}
		job.UpstreamSource = "" // cancellation must not erase an observed source
		job.Status = "completed"
		if err := db.FinishQualityTest(ctx, *job); err != nil {
			t.Fatal(err)
		}
		if err := db.UpdateCredentials(ctx, id, map[string]any{"openai_excel_bps": false}); err != nil {
			t.Fatal(err)
		}
		got, err := db.GetQualityTestJob(ctx, job.ID)
		if err != nil || got.UpstreamSource != "bps" {
			t.Fatalf("snapshot: %+v %v", got, err)
		}
		page, err := db.ListQualityTests(ctx, 1, 50, QualityTestFilter{AccountID: id, UpstreamSource: "bps"})
		if err != nil || page.Total != 1 {
			t.Fatalf("history filter: %+v %v", page, err)
		}
		legacy, err := db.CreateQualityTestJob(ctx, QualityTestJob{AccountID: id, AccountName: "legacy", Model: "gpt-6-astra"})
		if err != nil {
			t.Fatal(err)
		}
		legacy.Status = "completed"
		if err := db.FinishQualityTest(ctx, *legacy); err != nil {
			t.Fatal(err)
		}
		page, err = db.ListQualityTests(ctx, 1, 50, QualityTestFilter{AccountID: id, UpstreamSource: "unknown"})
		if err != nil || page.Total != 1 || page.Jobs[0].UpstreamSource != "" {
			t.Fatalf("unknown history: %+v %v", page, err)
		}
	})

	for _, writer := range astraPriorityWriteCases() {
		t.Run("revision_"+writer.name, func(t *testing.T) {
			id, err := db.InsertAccountWithCredentials(ctx, "bps-writer", map[string]any{"access_token": "old-token"}, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range []struct {
				updates  map[string]any
				revision int64
				enabled  bool
			}{
				{map[string]any{"openai_excel_bps": false}, 0, false},
				{map[string]any{"openai_excel_bps": true}, 1, true},
				{map[string]any{"openai_excel_bps": true}, 1, true},
				{map[string]any{"refresh_token": "rotated"}, 1, true},
				{map[string]any{"openai_excel_bps": false}, 2, false},
				{map[string]any{"openai_excel_bps": nil}, 2, false},
			} {
				if err := writer.write(ctx, db, id, step.updates); err != nil {
					t.Fatal(err)
				}
				got, err := db.AstraPolicyExpectation(ctx, id)
				if err != nil || got.BPSRevision != step.revision || got.BPSEnabled != step.enabled {
					t.Fatalf("revision: %+v %v want %+v", got, err, step)
				}
			}
		})
	}

	t.Run("policy_and_ticket_ABA_fence", func(t *testing.T) {
		f := newAstraPriorityFixture(t, db)
		f.miss()
		f.miss()
		before := f.state()
		if !before.Demoted || !before.PriorityDemoted {
			t.Fatal("fixture not demoted")
		}
		stale := f.outcome("new_292", true)
		ticket := TurnStateTicket{AccountID: f.id, Model: "gpt-6-astra", Token: "before"}
		if err := db.UpsertTurnStateTicket(ctx, ticket); err != nil {
			t.Fatal(err)
		}
		for _, flag := range []bool{true, false} {
			if err := db.UpdateCredentials(ctx, f.id, map[string]any{"openai_excel_bps": flag}); err != nil {
				t.Fatal(err)
			}
			if f.apply(stale) {
				t.Fatal("stale policy applied across BPS transition")
			}
			ticket.Token = "stale"
			if applied, err := db.UpsertHarvestedTurnStateTicket(ctx, ticket, stale.Expected.BPSRevision); err != nil || applied {
				t.Fatalf("stale publication: %t %v", applied, err)
			}
			if after := f.state(); !reflect.DeepEqual(before, after) {
				t.Fatalf("BPS transition rewrote policy state: before=%+v after=%+v", before, after)
			}
		}
		var token string
		if err := db.conn.QueryRowContext(ctx, `SELECT token FROM codex_turn_state_tickets WHERE account_id=$1 AND model=$2`, f.id, ticket.Model).Scan(&token); err != nil || token != "before" {
			t.Fatalf("ticket overwritten: %q %v", token, err)
		}
		fresh := f.outcome("new_292", true)
		if fresh.Expected.BPSRevision != 2 || !f.apply(fresh) {
			t.Fatal("new work after disabling should recover")
		}
		ticket.Token = "fresh"
		if applied, err := db.UpsertHarvestedTurnStateTicket(ctx, ticket, fresh.Expected.BPSRevision); err != nil || !applied {
			t.Fatalf("fresh publication: %t %v", applied, err)
		}
	})

	t.Run("publication_waits_for_account_transaction", func(t *testing.T) {
		id, err := db.InsertAccountWithCredentials(ctx, "bps-lock", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		ticket := TurnStateTicket{AccountID: id, Model: "gpt-5.6-sol", Token: "stale"}
		done := make(chan error, 1)
		err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
			if err := db.lockPolicyAccount(ctx, tx, id); err != nil {
				return err
			}
			started := make(chan struct{})
			go func() {
				close(started)
				applied, err := db.UpsertHarvestedTurnStateTicket(ctx, ticket, 0)
				if err == nil && applied {
					err = fmt.Errorf("publication escaped account transaction")
				}
				done <- err
			}()
			<-started
			if err := db.markBPSPolicyTransition(ctx, tx, id, nil, map[string]any{"openai_excel_bps": true}); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE accounts SET credentials='{"openai_excel_bps":true}' WHERE id=$1`, id); err != nil {
				return err
			}
			// A same-transaction enable/disable returns the flag to false but still
			// advances the generation; the old worker must not overwrite anything.
			if err := db.markBPSPolicyTransition(ctx, tx, id, map[string]any{"openai_excel_bps": true}, nil); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE accounts SET credentials='{}' WHERE id=$1`, id)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("publication deadlock")
		}
	})
}

func TestExcelBPSRevisionRollback(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "bps-rollback", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateCredentials(ctx, id, map[string]any{"openai_excel_bps": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE TRIGGER reject_bps_write BEFORE UPDATE OF bps_revision ON codex_astra_policy BEGIN SELECT RAISE(ABORT,'rejected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateCredentials(ctx, id, map[string]any{"openai_excel_bps": false, "expires_at": "changed"}); err == nil {
		t.Fatal("expected rollback")
	}
	state, err := db.AstraPolicyExpectation(ctx, id)
	if err != nil || !state.BPSEnabled || state.BPSRevision != 1 {
		t.Fatalf("partial transition: %+v %v", state, err)
	}
}

func TestExcelBPSLegacyMigration(t *testing.T) {
	runExcelBPSLegacyMigration(t, newGrokStateTestDB(t))
}

func runExcelBPSLegacyMigration(t *testing.T, db *DB) {
	ctx := context.Background()
	if !db.DrainBackgroundTasks(10 * time.Second) {
		t.Fatal("background migration did not drain")
	}
	// Recreate the pre-feature layout with existing history, then run the real
	// additive migrations. No endpoint/account-based backfill is permitted.
	for _, q := range []string{
		`INSERT INTO usage_logs(account_id,endpoint,model,status_code) VALUES(0,'/v1/responses','legacy',200)`,
		`ALTER TABLE usage_logs DROP COLUMN upstream_source`,
		`ALTER TABLE system_settings DROP COLUMN openai_excel_bps_enabled`,
		`ALTER TABLE quality_test_jobs DROP COLUMN upstream_source`,
		`ALTER TABLE codex_astra_policy DROP COLUMN bps_revision`,
	} {
		if _, err := db.conn.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := db.migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if err := db.ensureQualityTestSchema(ctx); err != nil {
			t.Fatal(err)
		}
		if err := db.ensureAstraPolicySchema(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var source sql.NullString
	if err := db.conn.QueryRowContext(ctx, `SELECT upstream_source FROM usage_logs WHERE model='legacy'`).Scan(&source); err != nil || source.Valid {
		t.Fatalf("historical source fabricated: %+v %v", source, err)
	}
	if enabled, err := db.GetOpenAIExcelBPSEnabled(ctx); err != nil || !enabled {
		t.Fatalf("legacy default: %t %v", enabled, err)
	}
}
