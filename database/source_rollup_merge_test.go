package database

import (
	"context"
	"testing"
	"time"
)

func TestSQLiteSourceFilterWithHourlyRollup(t *testing.T) {
	runSourceFilterWithHourlyRollup(t, newGrokStateTestDB(t))
}

func runSourceFilterWithHourlyRollup(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "source-rollup-fixture", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"bps", "codex", "other", ""} {
		if err := db.InsertUsageLog(ctx, &UsageLogInput{AccountID: id, Model: "source-rollup-fixture", Channel: "codex", UpstreamSource: source, StatusCode: 200, TotalTokens: 11, InputTokens: 7, OutputTokens: 4}); err != nil {
			t.Fatal(err)
		}
	}
	db.FlushUsageLogs()
	when := time.Now().UTC().Truncate(time.Hour).Add(-4 * time.Hour)
	var timestamp any = when
	if db.isSQLite() {
		timestamp = sqliteTimeParam(when)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE usage_logs SET created_at=$1 WHERE account_id=$2`, timestamp, id); err != nil {
		t.Fatal(err)
	}
	if err := db.RebuildUsageHourlyRollup(ctx); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"bps", "codex", "other", "unknown"} {
		f := UsageLogFilter{Start: when.Add(-time.Hour), End: when.Add(2 * time.Hour), Model: "source-rollup-fixture", UpstreamSource: source, Page: 1, PageSize: 20}
		if usageStatsRollupServable(f) {
			t.Fatal("source is not a dimension in hourly aggregate")
		}
		page, err := db.ListUsageLogsByTimeRangePaged(ctx, f)
		if err != nil || page.Total != 1 || len(page.Logs) != 1 || page.Logs[0].UpstreamSource != NormalizeUpstreamSource(source) {
			t.Fatalf("source page %q: %+v %v", source, page, err)
		}
		stats, err := db.GetUsageStatsFiltered(ctx, f.Start, f.End, "codex", f, true)
		if err != nil || stats.TodayRequests != 1 || stats.TodayTokens != 11 {
			t.Fatalf("source aggregate %q: %+v %v", source, stats, err)
		}
	}
}
