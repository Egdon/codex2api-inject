package database

import (
	"context"
	"database/sql"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestSQLiteBPSRetirementDatabase(t *testing.T) {
	runBPSRetirementDatabaseCases(t, newGrokStateTestDB(t))
}

// Explicit integration entrypoint; no provider/model traffic is used.
func TestPostgresBPSRetirementDatabase(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("requires isolated CODEX2API_TEST_POSTGRES_DSN")
	}
	t.Run("legacy_settings_restart", func(t *testing.T) {
		runSystemSettingsIgnoresRetiredColumnsAfterRestart(t, "postgres", dsn)
	})
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runBPSRetirementDatabaseCases(t, db)
}

func TestNormalizeHistoricalUpstreamSource(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{" BPS ", "bps"}, {"CODEX", "codex"}, {"other", "other"},
		{"relay", "other"}, {"", ""}, {" unknown ", ""},
	} {
		if got := NormalizeUpstreamSource(tc.input); got != tc.want {
			t.Errorf("NormalizeUpstreamSource(%q) = %q; want %q", tc.input, got, tc.want)
		}
	}
}

func migrateBPSRetirementFixture(t *testing.T, db *DB) {
	t.Helper()
	if !db.DrainBackgroundTasks(10 * time.Second) {
		t.Fatal("background migration did not drain")
	}
	ctx := context.Background()
	for range 2 {
		if err := db.migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if err := db.ensureQualityTestSchema(ctx); err != nil {
			t.Fatal(err)
		}
		if err := db.ensureTurnStateSchema(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func runBPSRetirementDatabaseCases(t *testing.T, db *DB) {
	ctx := context.Background()
	t.Run("source_logs_filters_and_counters", func(t *testing.T) {
		id, err := db.InsertAccountWithCredentials(ctx, "historical-source-fixture", map[string]any{"openai_excel_bps": true}, "")
		if err != nil {
			t.Fatal(err)
		}
		for i, source := range []string{" BPS ", "codex", "relay", "", "unknown"} {
			if err := db.InsertUsageLog(ctx, &UsageLogInput{AccountID: id, Channel: "codex", UpstreamSource: source, Model: "gpt-6-astra", Endpoint: "/v1/responses", InboundEndpoint: "/v1/responses", StatusCode: 200, TotalTokens: (i + 1) * 10, InputTokens: 10, OutputTokens: 1}); err != nil {
				t.Fatal(err)
			}
		}
		db.FlushUsageLogs()
		// An endpoint or a retired account flag must not manufacture historical evidence.
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO usage_logs(account_id,channel,endpoint,model,status_code,total_tokens,upstream_endpoint) VALUES($1,'codex','/v1/responses','gpt-6-astra',500,60,'https://relay.invalid/backend-api/codex/responses')`, id); err != nil {
			t.Fatal(err)
		}
		if err := db.UpdateCredentials(ctx, id, map[string]any{"openai_excel_bps": false}); err != nil {
			t.Fatal(err)
		}
		migrateBPSRetirementFixture(t, db)
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

	t.Run("quality_source_snapshot_and_timeout", func(t *testing.T) {
		id, err := db.InsertAccountWithCredentials(ctx, "quality-history", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		job, err := db.CreateQualityTestJob(ctx, QualityTestJob{AccountID: id, AccountName: "fixture", Model: "gpt-6-astra", TimeoutMS: (25 * time.Minute).Milliseconds()})
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
		job.UpstreamSource = "" // Cancellation must not erase an observed source.
		job.Status = "completed"
		if err := db.FinishQualityTest(ctx, *job); err != nil {
			t.Fatal(err)
		}
		if err := db.UpdateCredentials(ctx, id, map[string]any{"openai_excel_bps": false}); err != nil {
			t.Fatal(err)
		}
		legacy, err := db.CreateQualityTestJob(ctx, QualityTestJob{AccountID: id, AccountName: "legacy", Model: "gpt-6-astra"})
		if err != nil {
			t.Fatal(err)
		}
		legacy.Status = "completed"
		if err := db.FinishQualityTest(ctx, *legacy); err != nil {
			t.Fatal(err)
		}
		migrateBPSRetirementFixture(t, db)
		got, err := db.GetQualityTestJob(ctx, job.ID)
		if err != nil || got.UpstreamSource != "bps" || got.TimeoutMS != (25*time.Minute).Milliseconds() {
			t.Fatalf("snapshot/timeout: %+v %v", got, err)
		}
		page, err := db.ListQualityTests(ctx, 1, 50, QualityTestFilter{AccountID: id, UpstreamSource: "bps"})
		if err != nil || page.Total != 1 || page.Jobs[0].ID != job.ID {
			t.Fatalf("history filter: %+v %v", page, err)
		}
		page, err = db.ListQualityTests(ctx, 1, 50, QualityTestFilter{AccountID: id, UpstreamSource: "unknown"})
		if err != nil || page.Total != 1 || page.Jobs[0].ID != legacy.ID || page.Jobs[0].UpstreamSource != "" {
			t.Fatalf("unknown history: %+v %v", page, err)
		}
	})

	for _, writer := range astraPriorityWriteCases() {
		t.Run("inert_credentials_"+writer.name, func(t *testing.T) {
			f := newAstraPriorityFixture(t, db)
			f.miss()
			f.miss()
			before := f.state()
			if !before.Demoted || !before.PriorityDemoted {
				t.Fatal("fixture not demoted")
			}
			expected, err := db.AstraPolicyExpectation(ctx, f.id)
			if err != nil {
				t.Fatal(err)
			}
			for _, flags := range []map[string]any{
				{"openai_excel_bps": true}, {"openai_excel_bps_opt_out": true},
				{"openai_excel_bps": false, "openai_excel_bps_opt_out": false},
				{"openai_excel_bps": nil, "openai_excel_bps_opt_out": nil},
			} {
				if err := writer.write(ctx, db, f.id, flags); err != nil {
					t.Fatal(err)
				}
				if after := f.state(); !reflect.DeepEqual(before, after) {
					t.Fatalf("retired metadata rewrote policy: before=%+v after=%+v", before, after)
				}
				got, err := db.AstraPolicyExpectation(ctx, f.id)
				got.Sequence = expected.Sequence // Admission timestamps are not revisions.
				if err != nil || !reflect.DeepEqual(got, expected) {
					t.Fatalf("retired metadata invalidated surviving ownership: %+v %v", got, err)
				}
				var revision sql.NullInt64
				// Fresh installs do not create a BPS revision column or need one.
				if err := db.conn.QueryRowContext(ctx, `SELECT credential_generation FROM accounts WHERE id=$1`, f.id).Scan(&revision); err != nil || revision.Int64 != 1 {
					t.Fatalf("retired metadata changed identity generation: %+v %v", revision, err)
				}
			}
		})
	}

	t.Run("retired_flags_do_not_block_tickets_or_policy", func(t *testing.T) {
		f := newAstraPriorityFixture(t, db)
		if err := db.UpdateCredentials(ctx, f.id, map[string]any{"openai_excel_bps": true, "openai_excel_bps_opt_out": true}); err != nil {
			t.Fatal(err)
		}
		f.miss()
		f.miss()
		if state := f.state(); !state.Demoted || !state.PriorityDemoted {
			t.Fatalf("retired flags still block policy: %+v", state)
		}
		ticket := TurnStateTicket{AccountID: f.id, Model: "gpt-6-astra", Token: "synthetic-ticket"}
		if err := db.UpsertTurnStateTicket(ctx, ticket); err != nil {
			t.Fatal(err)
		}
		var token string
		if err := db.conn.QueryRowContext(ctx, `SELECT token FROM codex_turn_state_tickets WHERE account_id=$1 AND model=$2`, f.id, ticket.Model).Scan(&token); err != nil || token != ticket.Token {
			t.Fatalf("ticket still isolated by retired flags: %q %v", token, err)
		}
		outcome := f.outcome("new_292", true)
		stale := outcome
		stale.Epoch++
		if f.apply(stale) {
			t.Fatal("stale config epoch applied")
		}
		if !f.apply(outcome) {
			t.Fatal("current recovery blocked by retired flags")
		}
		before := f.state()
		if f.apply(outcome) || !reflect.DeepEqual(before, f.state()) {
			t.Fatal("duplicate batch bypassed sequence fence")
		}
	})
}
