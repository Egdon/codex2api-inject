package database

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSQLiteSystemSettingsIgnoresRetiredColumnsAfterRestart(t *testing.T) {
	runSystemSettingsIgnoresRetiredColumnsAfterRestart(t, "sqlite", filepath.Join(t.TempDir(), "legacy-settings.db"))
}

func runSystemSettingsIgnoresRetiredColumnsAfterRestart(t *testing.T, driver, dsn string) {
	t.Helper()
	ctx := context.Background()
	db, err := New(driver, dsn)
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if db != nil {
			_ = db.Close()
		}
	})
	restart := func() {
		t.Helper()
		if err := db.Close(); err != nil {
			t.Fatalf("close legacy database: %v", err)
		}
		db, err = New(driver, dsn)
		if err != nil {
			t.Fatalf("reopen legacy database: %v", err)
		}
	}
	columnCount := func(table, column string) int {
		t.Helper()
		query := `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 AND column_name=$2`
		if db.isSQLite() {
			query = `SELECT COUNT(*) FROM pragma_table_info($1) WHERE name=$2`
		}
		var count int
		if err := db.conn.QueryRowContext(ctx, query, table, column).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	// New installations omit every retired control. Old installations retain
	// the columns and JSON keys as inert data, without a conversion or cleanup.
	legacyColumns := []struct{ name, ddl string }{
		{"codex_basispoints_enabled", "BOOLEAN DEFAULT false"},
		{"codex_basispoints_models", "TEXT DEFAULT ''"},
		{"codex_basispoints_403_pause_disabled", "BOOLEAN DEFAULT false"},
		{"codex_basispoints_403_probe_interval_minutes", "INTEGER DEFAULT 1"},
		{"codex_basispoints_429_cooldown_seconds", "INTEGER DEFAULT 5"},
		{"codex_basispoints_cache_creation_as_input", "BOOLEAN DEFAULT false"},
		{"codex_basispoints_revision", "BIGINT NOT NULL DEFAULT 0"},
	}
	for _, column := range append(append([]struct{ name, ddl string }{}, legacyColumns...), struct{ name, ddl string }{"openai_excel_bps_enabled", "BOOLEAN DEFAULT true"}) {
		if columnCount("system_settings", column.name) != 0 {
			t.Fatalf("fresh database contains retired column %s", column.name)
		}
	}
	if columnCount("codex_astra_policy", "bps_revision") != 0 {
		t.Fatal("fresh database contains retired account revision")
	}
	for _, query := range []string{
		`ALTER TABLE system_settings ADD COLUMN openai_excel_bps_enabled BOOLEAN DEFAULT true`,
		`ALTER TABLE codex_astra_policy ADD COLUMN bps_revision BIGINT NOT NULL DEFAULT 0`,
	} {
		if _, err := db.conn.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	// Exercise SQL defaults independently of Go's zero-value bools.
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id) VALUES (1)
		ON CONFLICT (id) DO UPDATE SET
		codex_unified_client_identity_enabled=EXCLUDED.codex_unified_client_identity_enabled,
		show_upstream_model_mismatch=EXCLUDED.show_upstream_model_mismatch`); err != nil {
		t.Fatal(err)
	}
	settings, err := db.GetSystemSettings(ctx)
	if err != nil || settings == nil || settings.CodexUnifiedClientIdentityEnabled || !settings.ShowUpstreamModelMismatch {
		t.Fatalf("new upstream setting defaults = %+v, %v", settings, err)
	}
	settings.SiteName, settings.MaxConcurrency = "Before upgrade", 4
	settings.CodexUnifiedClientIdentityEnabled, settings.ShowUpstreamModelMismatch = true, false
	settings.PromptFilterCustomPatterns, settings.PromptFilterReviewAPIKey = `[{"id":"fork-fixture"}]`, "fork-fixture-key"
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	// These fork-only narrow settings must survive full settings upserts as well
	// as additive migrations for the two upstream controls.
	for kind, build := range map[string]string{
		"desktop-mac": "mac-fork-build", "desktop-windows": "windows-fork-build", "vscode": "vscode-fork-build",
	} {
		if err := db.UpdateCodexSyncedAppBuild(ctx, kind, build); err != nil {
			t.Fatal(err)
		}
	}
	checkSettings := func(unified, mismatch bool) *SystemSettings {
		t.Helper()
		got, err := db.GetSystemSettings(ctx)
		if err != nil || got == nil {
			t.Fatalf("read merged settings: %+v, %v", got, err)
		}
		if got.CodexUnifiedClientIdentityEnabled != unified || got.ShowUpstreamModelMismatch != mismatch {
			t.Fatalf("merged toggles = (%v, %v), want (%v, %v)", got.CodexUnifiedClientIdentityEnabled, got.ShowUpstreamModelMismatch, unified, mismatch)
		}
		if got.CodexSyncedDesktopMacBuild != "mac-fork-build" || got.CodexSyncedDesktopWindowsBuild != "windows-fork-build" || got.CodexSyncedVSCodeBuild != "vscode-fork-build" {
			t.Fatal("upstream settings update or migration changed fork app builds")
		}
		if got.PromptFilterCustomPatterns != `[{"id":"fork-fixture"}]` || got.PromptFilterReviewAPIKey != "fork-fixture-key" {
			t.Fatal("new SQL parameters disturbed preserve flags")
		}
		return got
	}
	checkSettings(true, false)
	restart()
	checkSettings(true, false)
	if _, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET openai_excel_bps_enabled=false WHERE id=1`); err != nil {
		t.Fatal(err)
	}

	type accountSnapshot struct {
		id, generation int64
		credentials    string
	}
	readAccount := func(id int64) accountSnapshot {
		t.Helper()
		snapshot := accountSnapshot{id: id}
		if err := db.conn.QueryRowContext(ctx, `SELECT CAST(credentials AS TEXT),credential_generation FROM accounts WHERE id=$1`, id).Scan(&snapshot.credentials, &snapshot.generation); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	var accounts []accountSnapshot
	for _, flags := range []map[string]any{
		{"openai_excel_bps": true}, {"openai_excel_bps": false}, {},
		{"openai_excel_bps": true, "openai_excel_bps_opt_out": false},
		{"openai_excel_bps": false, "openai_excel_bps_opt_out": true},
	} {
		flags["custom"] = map[string]any{"nested": "unchanged"}
		id, err := db.InsertAccountWithCredentials(ctx, "legacy-metadata", flags, "")
		if err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, readAccount(id))
	}
	f := newAstraPriorityFixture(t, db)
	f.miss()
	f.miss()
	policy := f.state()
	if !policy.Demoted || !policy.PriorityDemoted {
		t.Fatal("fixture not demoted")
	}
	accounts = append(accounts, readAccount(f.id))
	expected, err := db.AstraPolicyExpectation(ctx, f.id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE codex_astra_policy SET bps_revision=23 WHERE account_id=$1`, f.id); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertTurnStateTicket(ctx, TurnStateTicket{AccountID: f.id, Model: "gpt-6-astra", Token: "preserved-fixture"}); err != nil {
		t.Fatal(err)
	}

	historyID := accounts[0].id
	start, end := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	jobIDs := map[string]int64{}
	for _, source := range []string{"bps", "codex", "other", ""} {
		if err := db.InsertUsageLog(ctx, &UsageLogInput{AccountID: historyID, Channel: "codex", UpstreamSource: source, Model: "gpt-6-astra", Endpoint: "/v1/responses", StatusCode: 200, TotalTokens: 11}); err != nil {
			t.Fatal(err)
		}
		job, err := db.CreateQualityTestJob(ctx, QualityTestJob{AccountID: historyID, AccountName: "history", Model: "gpt-6-astra", UpstreamSource: source})
		if err != nil {
			t.Fatal(err)
		}
		job.Status, job.Output = "completed", "preserved historical output"
		if err := db.FinishQualityTest(ctx, *job); err != nil {
			t.Fatal(err)
		}
		jobIDs[source] = job.ID
	}
	db.FlushUsageLogs()
	turnStateConfig, err := db.LoadTurnStateConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checkPreserved := func() {
		t.Helper()
		if got, err := db.LoadTurnStateConfig(ctx); err != nil || got != turnStateConfig {
			t.Fatalf("upstream settings merge changed fork Astra config: %q, %v", got, err)
		}
		for _, before := range accounts {
			if after := readAccount(before.id); after != before {
				t.Fatalf("retirement rewrote legacy metadata for account %d", before.id)
			}
		}
		after, err := db.AstraPolicySnapshot(ctx, f.id)
		if err != nil || !reflect.DeepEqual(after, policy) {
			t.Fatalf("retirement changed policy audit state: %+v %v", after, err)
		}
		current, err := db.AstraPolicyExpectation(ctx, f.id)
		current.Sequence = expected.Sequence
		if err != nil || !reflect.DeepEqual(current, expected) {
			t.Fatalf("retirement restored groups/priorities or changed ownership: %+v %v", current, err)
		}
		var revision int64
		if err := db.conn.QueryRowContext(ctx, `SELECT bps_revision FROM codex_astra_policy WHERE account_id=$1`, f.id).Scan(&revision); err != nil || revision != 23 {
			t.Fatalf("retired revision mutated: %d %v", revision, err)
		}
		var token string
		if err := db.conn.QueryRowContext(ctx, `SELECT token FROM codex_turn_state_tickets WHERE account_id=$1 AND model='gpt-6-astra'`, f.id).Scan(&token); err != nil || token != "preserved-fixture" {
			t.Fatalf("retirement changed saved ticket: %q %v", token, err)
		}
		var migrations int
		if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM data_migrations WHERE version='20260929_fork_excel_bps_tristate_v1'`).Scan(&migrations); err != nil || migrations != 0 {
			t.Fatalf("obsolete control migration ran: %d %v", migrations, err)
		}
		for source, jobID := range jobIDs {
			filterSource := source
			if source == "" {
				filterSource = "unknown"
			}
			filter := UsageLogFilter{Start: start, End: end, AccountID: &historyID, UpstreamSource: filterSource, Page: 1, PageSize: 20}
			rows, err := db.ListUsageLogsByFilter(ctx, filter)
			if err != nil || len(rows) != 1 || rows[0].UpstreamSource != source || rows[0].TotalTokens != 11 {
				t.Fatalf("historical usage source %q changed: %+v %v", source, rows, err)
			}
			page, err := db.ListQualityTests(ctx, 1, 20, QualityTestFilter{AccountID: historyID, UpstreamSource: filterSource})
			if err != nil || page.Total != 1 || page.Jobs[0].ID != jobID || page.Jobs[0].UpstreamSource != source {
				t.Fatalf("historical quality source %q changed: %+v %v", source, page, err)
			}
			job, err := db.GetQualityTestJob(ctx, jobID)
			if err != nil || job.Output != "preserved historical output" {
				t.Fatalf("historical output lost: %+v %v", job, err)
			}
		}
	}

	// Simulate the pre-v3.0.7 fork schema with real settings, Astra state and
	// history already present. Reopening must only add defaulted columns.
	if !db.DrainBackgroundTasks(10 * time.Second) {
		t.Fatal("background migration did not drain before schema downgrade")
	}
	for _, column := range []string{"codex_unified_client_identity_enabled", "show_upstream_model_mismatch"} {
		if _, err := db.conn.ExecContext(ctx, `ALTER TABLE system_settings DROP COLUMN `+column); err != nil {
			t.Fatal(err)
		}
	}
	// A fork-only schema with its master disabled must NOT undergo the old
	// tri-state conversion (which would rewrite account flags and opt-outs).
	restart()
	checkSettings(false, true)
	checkPreserved()
	// Nullable legacy rows use the same read defaults as absent columns.
	if _, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET codex_unified_client_identity_enabled=NULL, show_upstream_model_mismatch=NULL WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	checkSettings(false, true)
	for _, column := range legacyColumns {
		if columnCount("system_settings", column.name) != 0 {
			t.Fatalf("restart recreated retired column %s", column.name)
		}
		if _, err := db.conn.ExecContext(ctx, `ALTER TABLE system_settings ADD COLUMN `+column.name+` `+column.ddl); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET
		codex_basispoints_enabled=true, codex_basispoints_models='retired-model',
		codex_basispoints_403_pause_disabled=true, codex_basispoints_403_probe_interval_minutes=60,
		codex_basispoints_429_cooldown_seconds=90, codex_basispoints_cache_creation_as_input=true,
		codex_basispoints_revision=17 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	restart()
	settings = checkSettings(false, true)
	if settings.SiteName != "Before upgrade" || settings.MaxConcurrency != 4 {
		t.Fatalf("read after upgrade = %+v", settings)
	}
	settings.SiteName, settings.MaxConcurrency = "After upgrade", 8
	for _, toggles := range []struct{ unified, mismatch bool }{{true, false}, {true, true}, {false, false}, {false, true}} {
		settings.CodexUnifiedClientIdentityEnabled = toggles.unified
		settings.ShowUpstreamModelMismatch = toggles.mismatch
		settings.PreservePromptFilterCustomPatterns, settings.PreservePromptFilterReviewAPIKey = true, true
		settings.PromptFilterCustomPatterns, settings.PromptFilterReviewAPIKey = "[]", ""
		// Stale whole-row snapshots cannot overwrite the fork's narrow fields.
		settings.CodexSyncedDesktopMacBuild, settings.CodexSyncedDesktopWindowsBuild, settings.CodexSyncedVSCodeBuild = "stale", "stale", "stale"
		if err := db.UpdateSystemSettings(ctx, settings); err != nil {
			t.Fatalf("save after upgrade: %v", err)
		}
		checkSettings(toggles.unified, toggles.mismatch)
		restart()
		checkSettings(toggles.unified, toggles.mismatch)
		checkPreserved()
	}
	settings = checkSettings(false, true)
	if settings.SiteName != "After upgrade" || settings.MaxConcurrency != 8 {
		t.Fatalf("read saved settings after restart = %+v", settings)
	}
	var enabled, pauseDisabled, cacheAsInput, forkMaster bool
	var probeMinutes, cooldownSeconds, revision int
	var models string
	if err := db.conn.QueryRowContext(ctx, `SELECT codex_basispoints_enabled,codex_basispoints_models,
		codex_basispoints_403_pause_disabled,codex_basispoints_403_probe_interval_minutes,
		codex_basispoints_429_cooldown_seconds,codex_basispoints_cache_creation_as_input,
		codex_basispoints_revision,openai_excel_bps_enabled FROM system_settings WHERE id=1`).Scan(
		&enabled, &models, &pauseDisabled, &probeMinutes, &cooldownSeconds, &cacheAsInput, &revision, &forkMaster); err != nil {
		t.Fatalf("read preserved legacy columns: %v", err)
	}
	if !enabled || models != "retired-model" || !pauseDisabled || probeMinutes != 60 || cooldownSeconds != 90 || !cacheAsInput || revision != 17 || forkMaster {
		t.Fatal("settings save or restart mutated retired values")
	}
}
