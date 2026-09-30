package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestExcelBPSDatabase(t *testing.T) { runExcelBPSDatabaseCases(t, newGrokStateTestDB(t)) }

func TestExcelBPSTriStateMigration(t *testing.T) {
	runExcelBPSTriStateMigrationCases(t, newGrokStateTestDB(t))
}

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
	t.Run("tri_state_migration", func(t *testing.T) { runExcelBPSTriStateMigrationCases(t, db) })
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
				{map[string]any{"openai_excel_bps_opt_out": true}, 3, false},
				{map[string]any{"openai_excel_bps_opt_out": true}, 3, false},
				{map[string]any{"access_token": "refreshed"}, 3, false},
				{map[string]any{"openai_excel_bps_opt_out": false}, 4, false},
				{map[string]any{"openai_excel_bps_opt_out": nil}, 4, false},
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
			if applied, err := db.UpsertHarvestedTurnStateTicket(ctx, ticket, stale.Expected.BPSRevision, stale.Expected.GlobalBPSRevision); err != nil || applied {
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
		if applied, err := db.UpsertHarvestedTurnStateTicket(ctx, ticket, fresh.Expected.BPSRevision, fresh.Expected.GlobalBPSRevision); err != nil || !applied {
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
				applied, err := db.UpsertHarvestedTurnStateTicket(ctx, ticket, 0, 0)
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
	t.Run("upstream_global_revision_and_legacy_independence", func(t *testing.T) {
		if _, err := db.conn.ExecContext(ctx, `DELETE FROM system_settings WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		if enabled, revision, err := db.GetCodexBasispointsState(ctx); err != nil || enabled || revision != 0 {
			t.Fatalf("fresh upstream default: %t %d %v", enabled, revision, err)
		}
		for _, legacy := range []bool{true, false} {
			if err := db.SaveOpenAIExcelBPSEnabled(ctx, legacy); err != nil {
				t.Fatal(err)
			}
			if enabled, revision, err := db.GetCodexBasispointsState(ctx); err != nil || enabled || revision != 0 {
				t.Fatalf("legacy master changed upstream: %t %d %v", enabled, revision, err)
			}
		}
		for _, step := range []struct {
			enabled  bool
			revision int64
		}{{false, 0}, {true, 1}, {true, 1}, {false, 2}, {false, 2}} {
			if err := db.UpdateSystemSettings(ctx, &SystemSettings{CodexBasispointsEnabled: step.enabled}); err != nil {
				t.Fatal(err)
			}
			if enabled, revision, err := db.GetCodexBasispointsState(ctx); err != nil || enabled != step.enabled || revision != step.revision {
				t.Fatalf("global transition: %t %d %v want %+v", enabled, revision, err, step)
			}
		}
	})

	for _, transition := range []string{"global", "opt_out"} {
		t.Run(transition+"_ABA_ticket_and_policy_fence", func(t *testing.T) {
			f := newAstraPriorityFixture(t, db)
			f.miss()
			f.miss()
			before := f.state()
			if !before.Demoted || !before.PriorityDemoted {
				t.Fatal("fixture not demoted")
			}
			stale := f.outcome("new_292", true)
			ticket := TurnStateTicket{AccountID: f.id, Model: "gpt-6-astra", Token: "original"}
			if err := db.UpsertTurnStateTicket(ctx, ticket); err != nil {
				t.Fatal(err)
			}
			for _, value := range []bool{true, false} {
				if transition == "global" {
					if err := db.UpdateSystemSettings(ctx, &SystemSettings{CodexBasispointsEnabled: value}); err != nil {
						t.Fatal(err)
					}
				} else if err := db.UpdateCredentials(ctx, f.id, map[string]any{"openai_excel_bps_opt_out": value}); err != nil {
					t.Fatal(err)
				}
				if current, err := db.HarvestBPSCurrent(ctx, f.id, stale.Expected.BPSRevision, stale.Expected.GlobalBPSRevision); err != nil || current {
					t.Fatalf("stale admission: %t %v", current, err)
				}
				if f.apply(stale) {
					t.Fatal("stale outcome changed policy")
				}
				ticket.Token = "stale"
				if applied, err := db.UpsertHarvestedTurnStateTicket(ctx, ticket, stale.Expected.BPSRevision, stale.Expected.GlobalBPSRevision); err != nil || applied {
					t.Fatalf("stale publication: %t %v", applied, err)
				}
				if !reflect.DeepEqual(before, f.state()) {
					t.Fatal("transition rewrote group/priority policy")
				}
			}
			var token string
			if err := db.conn.QueryRowContext(ctx, `SELECT token FROM codex_turn_state_tickets WHERE account_id=$1 AND model=$2`, f.id, ticket.Model).Scan(&token); err != nil || token != "original" {
				t.Fatalf("ticket changed: %q %v", token, err)
			}
			fresh := f.outcome("new_292", true)
			if !f.apply(fresh) {
				t.Fatal("fresh work could not recover")
			}
			if applied, err := db.UpsertHarvestedTurnStateTicket(ctx, ticket, fresh.Expected.BPSRevision, fresh.Expected.GlobalBPSRevision); err != nil || !applied {
				t.Fatalf("fresh publication: %t %v", applied, err)
			}
		})
	}

	t.Run("publication_waits_for_global_transaction", func(t *testing.T) {
		id, err := db.InsertAccountWithCredentials(ctx, "global-lock", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		expected, err := db.AstraPolicyExpectation(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `UPDATE system_settings SET codex_basispoints_enabled=true,codex_basispoints_revision=codex_basispoints_revision+1 WHERE id=1`); err != nil {
				return err
			}
			started := make(chan struct{})
			go func() {
				close(started)
				applied, err := db.UpsertHarvestedTurnStateTicket(ctx, TurnStateTicket{AccountID: id, Model: "gpt-6-astra", Token: "stale"}, expected.BPSRevision, expected.GlobalBPSRevision)
				if err == nil && applied {
					err = fmt.Errorf("publication escaped settings transaction")
				}
				done <- err
			}()
			<-started
			_, err := tx.ExecContext(ctx, `UPDATE system_settings SET codex_basispoints_enabled=false,codex_basispoints_revision=codex_basispoints_revision+1 WHERE id=1`)
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
			t.Fatal("settings/account lock deadlock")
		}
	})
}

func TestExcelBPSRevisionRollback(t *testing.T) {
	for _, key := range []string{"openai_excel_bps", "openai_excel_bps_opt_out"} {
		t.Run(key, func(t *testing.T) {
			db := newGrokStateTestDB(t)
			ctx := context.Background()
			id, err := db.InsertAccountWithCredentials(ctx, "bps-rollback", nil, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := db.UpdateCredentials(ctx, id, map[string]any{key: true}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.conn.ExecContext(ctx, `CREATE TRIGGER reject_bps_write BEFORE UPDATE OF bps_revision ON codex_astra_policy BEGIN SELECT RAISE(ABORT,'rejected'); END`); err != nil {
				t.Fatal(err)
			}
			// Neither key may take the JSON-only fast path: a failed revision
			// write must roll back its flag and every other credential update.
			if err := db.UpdateCredentials(ctx, id, map[string]any{key: false, "expires_at": "changed"}); err == nil {
				t.Fatal("expected rollback")
			}
			state, err := db.AstraPolicyExpectation(ctx, id)
			if err != nil || state.BPSEnabled != (key == "openai_excel_bps") || state.BPSRevision != 1 {
				t.Fatalf("partial transition: %+v %v", state, err)
			}
			row, err := db.GetAccountByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if !row.GetCredentialBool(key) || row.Credentials["expires_at"] != nil {
				t.Fatal("failed revision write changed credentials")
			}
		})
	}
}

func runExcelBPSTriStateMigrationCases(t *testing.T, db *DB) {
	ctx := context.Background()
	if !db.DrainBackgroundTasks(10 * time.Second) {
		t.Fatal("background migration did not drain")
	}
	for _, tc := range []struct {
		name                           string
		master                         any
		missingRow, noLegacy, upstream bool
		upstreamValue                  bool
	}{
		{name: "legacy_master_true", master: true},
		{name: "legacy_master_false", master: false},
		{name: "legacy_master_null"},
		{name: "legacy_master_missing_row", missingRow: true},
		{name: "new_install_no_fork_marker", noLegacy: true},
		{name: "existing_upstream_false", master: true, upstream: true},
		{name: "existing_upstream_true", master: false, upstream: true, upstreamValue: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, q := range []string{`DELETE FROM system_settings WHERE id=1`, `ALTER TABLE system_settings DROP COLUMN codex_basispoints_enabled`} {
				if _, err := db.conn.ExecContext(ctx, q); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.conn.ExecContext(ctx, `DELETE FROM data_migrations WHERE version=$1`, dataMigrationExcelBPSTriStateV1); err != nil {
				t.Fatal(err)
			}
			if tc.noLegacy {
				if _, err := db.conn.ExecContext(ctx, `ALTER TABLE system_settings DROP COLUMN openai_excel_bps_enabled`); err != nil {
					t.Fatal(err)
				}
			} else if !tc.missingRow {
				if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings(id,openai_excel_bps_enabled) VALUES(1,$1)`, tc.master); err != nil {
					t.Fatal(err)
				}
			}
			if tc.upstream {
				columnType := "BOOLEAN DEFAULT false"
				if db.isSQLite() {
					columnType = "INTEGER DEFAULT 0"
				}
				if _, err := db.conn.ExecContext(ctx, `ALTER TABLE system_settings ADD COLUMN codex_basispoints_enabled `+columnType); err != nil {
					t.Fatal(err)
				}
				if _, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET codex_basispoints_enabled=$1 WHERE id=1`, tc.upstreamValue); err != nil {
					t.Fatal(err)
				}
			}
			type fixture struct {
				id          int64
				credentials map[string]any
				generation  int64
				updated     string
			}
			var fixtures []fixture
			for _, flags := range []map[string]any{
				{"openai_excel_bps": true}, {"openai_excel_bps": false}, {},
				{"openai_excel_bps": true, "openai_excel_bps_opt_out": false},
				{"openai_excel_bps": false, "openai_excel_bps_opt_out": false},
				{"openai_excel_bps": false, "openai_excel_bps_opt_out": true},
			} {
				flags["access_token"] = "enc:v1:fixture-ciphertext"
				flags["refresh_token"] = "fixture-plaintext"
				flags["scheduler_priority"] = float64(7)
				flags["custom"] = map[string]any{"nested": "unchanged"}
				id, err := db.InsertAccountWithCredentials(ctx, "migration-"+tc.name, flags, "")
				if err != nil {
					t.Fatal(err)
				}
				var raw []byte
				f := fixture{id: id}
				if err := db.conn.QueryRowContext(ctx, `SELECT credentials,credential_generation,CAST(updated_at AS TEXT) FROM accounts WHERE id=$1`, id).Scan(&raw, &f.generation, &f.updated); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &f.credentials); err != nil {
					t.Fatal(err)
				}
				fixtures = append(fixtures, f)
			}
			// Commit conversion independently, then simulate a restart before schema
			// completion. The second pre-DDL call must not reinterpret old intent.
			if err := db.migrateLegacyExcelBPS(ctx); err != nil {
				t.Fatal(err)
			}
			if err := db.migrateLegacyExcelBPS(ctx); err != nil {
				t.Fatal(err)
			}
			if err := db.migrate(ctx); err != nil {
				t.Fatal(err)
			}
			for _, f := range fixtures {
				want := f.credentials
				_, hasOptOut := want["openai_excel_bps_opt_out"]
				if flag, exists := want["openai_excel_bps"]; exists && !hasOptOut && !tc.noLegacy && !tc.upstream {
					on := flag == true && tc.master != false
					want["openai_excel_bps"], want["openai_excel_bps_opt_out"] = on, !on
				}
				var raw []byte
				var generation int64
				var updated string
				if err := db.conn.QueryRowContext(ctx, `SELECT credentials,credential_generation,CAST(updated_at AS TEXT) FROM accounts WHERE id=$1`, f.id).Scan(&raw, &generation, &updated); err != nil {
					t.Fatal(err)
				}
				var got map[string]any
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) || generation != f.generation || updated != f.updated {
					t.Fatalf("metadata-only migration changed unrelated state for %d: got=%v want=%v generation=%d/%d timestamp=%s/%s", f.id, got, want, generation, f.generation, updated, f.updated)
				}
			}
			if enabled, _, err := db.GetCodexBasispointsState(ctx); err != nil || enabled != tc.upstreamValue {
				t.Fatalf("upstream global overwritten: %t %v", enabled, err)
			}
			if !tc.noLegacy && !tc.missingRow {
				var legacy sql.NullBool
				if err := db.conn.QueryRowContext(ctx, `SELECT openai_excel_bps_enabled FROM system_settings WHERE id=1`).Scan(&legacy); err != nil {
					t.Fatal(err)
				}
				if tc.master == nil && legacy.Valid || tc.master != nil && (!legacy.Valid || legacy.Bool != tc.master) {
					t.Fatalf("old master overwritten: %+v", legacy)
				}
			}
			// Later administrator writes are authoritative even on another startup.
			if err := db.UpdateCredentials(ctx, fixtures[0].id, map[string]any{"openai_excel_bps": false, "openai_excel_bps_opt_out": false}); err != nil {
				t.Fatal(err)
			}
			if err := db.SaveOpenAIExcelBPSEnabled(ctx, true); err != nil {
				t.Fatal(err)
			}
			if err := db.migrate(ctx); err != nil {
				t.Fatal(err)
			}
			row, err := db.GetAccountByID(ctx, fixtures[0].id)
			if err != nil || row.GetCredentialBool("openai_excel_bps") || row.GetCredentialBool("openai_excel_bps_opt_out") {
				t.Fatalf("rerun replaced administrator intent: %v", err)
			}
		})
	}
	t.Run("atomic_rollback_and_retry", func(t *testing.T) {
		if _, err := db.conn.ExecContext(ctx, `ALTER TABLE system_settings DROP COLUMN codex_basispoints_enabled`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.conn.ExecContext(ctx, `DELETE FROM data_migrations WHERE version=$1`, dataMigrationExcelBPSTriStateV1); err != nil {
			t.Fatal(err)
		}
		if err := db.SaveOpenAIExcelBPSEnabled(ctx, false); err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for range 2 {
			id, err := db.InsertAccountWithCredentials(ctx, "rollback-migration", map[string]any{"openai_excel_bps": true}, "")
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		create := fmt.Sprintf(`CREATE TRIGGER reject_bps_migration BEFORE UPDATE OF credentials ON accounts WHEN NEW.id=%d BEGIN SELECT RAISE(ABORT,'fixture reject'); END`, ids[1])
		drop := `DROP TRIGGER reject_bps_migration`
		if !db.isSQLite() {
			if _, err := db.conn.ExecContext(ctx, `CREATE OR REPLACE FUNCTION reject_bps_migration_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture reject'; END $$`); err != nil {
				t.Fatal(err)
			}
			create = fmt.Sprintf(`CREATE TRIGGER reject_bps_migration BEFORE UPDATE OF credentials ON accounts FOR EACH ROW WHEN (NEW.id=%d) EXECUTE FUNCTION reject_bps_migration_fixture()`, ids[1])
			drop = `DROP TRIGGER reject_bps_migration ON accounts`
			t.Cleanup(func() { _, _ = db.conn.ExecContext(ctx, `DROP FUNCTION IF EXISTS reject_bps_migration_fixture()`) })
		}
		if _, err := db.conn.ExecContext(ctx, create); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = db.conn.ExecContext(ctx, drop) })
		if err := db.migrateLegacyExcelBPS(ctx); err == nil {
			t.Fatal("expected migration failure")
		}
		var markers int
		if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM data_migrations WHERE version=$1`, dataMigrationExcelBPSTriStateV1).Scan(&markers); err != nil || markers != 0 {
			t.Fatalf("failed migration retained marker: %d %v", markers, err)
		}
		for _, id := range ids {
			row, err := db.GetAccountByID(ctx, id)
			if err != nil || !row.GetCredentialBool("openai_excel_bps") {
				t.Fatalf("partial credentials update: %v", err)
			}
			if _, exists := row.Credentials["openai_excel_bps_opt_out"]; exists {
				t.Fatal("partial opt-out update")
			}
		}
		if _, err := db.conn.ExecContext(ctx, drop); err != nil {
			t.Fatal(err)
		}
		if err := db.migrate(ctx); err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			row, err := db.GetAccountByID(ctx, id)
			if err != nil || row.GetCredentialBool("openai_excel_bps") || !row.GetCredentialBool("openai_excel_bps_opt_out") {
				t.Fatalf("retry failed conversion: %v", err)
			}
		}
	})
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
