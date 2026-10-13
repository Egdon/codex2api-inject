package database

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPlanUsageHourlySplitsPartialHours(t *testing.T) {
	now := time.Date(2026, 10, 10, 15, 42, 30, 0, time.UTC)
	start := time.Date(2026, 10, 9, 8, 20, 0, 0, time.UTC)
	cut := time.Date(2026, 10, 9, 22, 5, 0, 0, time.UTC)
	aligned := time.Date(2026, 10, 10, 3, 0, 0, 0, time.UTC)
	plan := planUsageHourly(start, time.Time{}, now, cut, aligned, now.Add(-time.Minute))
	if !plan.useRollup {
		t.Fatal("expected rollup for a multi-hour range")
	}
	if want := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC); !plan.from.Equal(want) {
		t.Fatalf("from = %s, want %s", plan.from, want)
	}
	if want := time.Date(2026, 10, 10, 15, 0, 0, 0, time.UTC); !plan.to.Equal(want) {
		t.Fatalf("to = %s, want %s", plan.to, want)
	}
	if len(plan.skip) != 1 || !plan.skip[0].Equal(time.Date(2026, 10, 9, 22, 0, 0, 0, time.UTC)) {
		t.Fatalf("skip = %v, want only the hour containing the unaligned cut", plan.skip)
	}
	want := []usageTimeSpan{
		{start, plan.from},
		{time.Date(2026, 10, 9, 22, 0, 0, 0, time.UTC), time.Date(2026, 10, 9, 23, 0, 0, 0, time.UTC)},
		{plan.to, time.Time{}},
	}
	if len(plan.raw) != len(want) {
		t.Fatalf("raw = %v, want %v", plan.raw, want)
	}
	for i := range want {
		if !plan.raw[i].start.Equal(want[i].start) || !plan.raw[i].end.Equal(want[i].end) {
			t.Fatalf("raw[%d] = %v, want %v", i, plan.raw[i], want[i])
		}
	}

	short := planUsageHourly(now.Add(-50*time.Minute), now, now)
	if short.useRollup || len(short.raw) != 1 {
		t.Fatalf("sub-hour range should stay on raw logs: %+v", short)
	}
	all := planUsageHourly(time.Time{}, now, now)
	if !all.useRollup || !all.from.IsZero() || len(all.raw) != 1 || !all.raw[0].end.Equal(now) {
		t.Fatalf("unbounded start should read every rollup hour before the tail: %+v", all)
	}
}

type hourlyTestLog struct {
	at                        time.Time
	apiKeyID                  int64
	keyName, keyMasked        string
	channel                   string
	model, effective          string
	inbound, endpoint         string
	status                    int
	stream                    bool
	internal                  string
	prompt, completion        int
	input, output, cached     int
	reasoning                 int
	effort, tier              string
	duration, firstToken      int
	imageCount, attempt       int
	accountBilled, userBilled float64
	errorMessage, errorKind   string
	retryAttempt              bool
}

func insertHourlyTestLogs(t *testing.T, db *DB, logs []hourlyTestLog) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, l := range logs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage_logs (
			api_key_id, api_key_name, api_key_masked, channel, model, effective_model, inbound_endpoint, endpoint,
			status_code, stream, internal_reason, prompt_tokens, completion_tokens, total_tokens, input_tokens, output_tokens,
			cached_tokens, reasoning_tokens, reasoning_effort, billing_service_tier, duration_ms, first_token_ms,
			image_count, attempt_index, account_billed, user_billed, error_message, upstream_error_kind, is_retry_attempt, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30)`,
			l.apiKeyID, l.keyName, l.keyMasked, l.channel, l.model, l.effective, l.inbound, l.endpoint,
			l.status, l.stream, l.internal, l.prompt, l.completion, l.prompt+l.completion, l.input, l.output,
			l.cached, l.reasoning, l.effort, l.tier, l.duration, l.firstToken,
			l.imageCount, l.attempt, l.accountBilled, l.userBilled, l.errorMessage, l.errorKind, l.retryAttempt, db.timeArg(l.at)); err != nil {
			t.Fatalf("insert usage log: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func generateHourlyTestLogs(now time.Time, keyIDs []int64, count int) []hourlyTestLog {
	rng := rand.New(rand.NewSource(778))
	pick := func(values ...string) string { return values[rng.Intn(len(values))] }
	logs := make([]hourlyTestLog, 0, count+8)
	for i := 0; i < count; i++ {
		// 落在 [now-45d, now-2min],整秒;同时故意制造一些整点与窗口边界附近的记录。
		at := now.Add(-time.Duration(2*60+rng.Int63n(45*24*3600)) * time.Second)
		switch i % 50 {
		case 0:
			at = at.Truncate(time.Hour)
		case 1:
			at = now.Add(-5*time.Hour + time.Duration(rng.Intn(120)-60)*time.Second)
		case 2:
			at = now.Add(-7*24*time.Hour + time.Duration(rng.Intn(120)-60)*time.Second)
		case 3:
			at = now.Add(-time.Duration(2+rng.Intn(40)) * time.Minute)
		}
		keyIndex := rng.Intn(len(keyIDs) + 1)
		var keyID int64
		keyName, keyMasked := "", ""
		if keyIndex < len(keyIDs) {
			keyID = keyIDs[keyIndex]
			keyName = pick(fmt.Sprintf("key-%d", keyID), "")
			keyMasked = fmt.Sprintf("sk-***%d", keyID)
		}
		status := []int{200, 200, 200, 200, 400, 429, 499, 500, 502}[rng.Intn(9)]
		internal := ""
		if rng.Intn(25) == 0 {
			internal = "connection_test"
		}
		prompt, completion := rng.Intn(5000), rng.Intn(2000)
		logs = append(logs, hourlyTestLog{
			at: at, apiKeyID: keyID, keyName: keyName, keyMasked: keyMasked,
			channel:   pick("codex", "codex", "grok", "claude", ""),
			model:     pick("gpt-5.5", "gpt-5.6-luna", "gpt-image-2", "grok-4.7", ""),
			effective: pick("", "gpt-5.5", "gpt-5.6-sol"),
			inbound:   pick("/v1/responses", "/v1/chat/completions", "/v1/messages", "/v1/images/generations", ""),
			endpoint:  pick("/v1/responses", "/backend-api/codex/responses", ""),
			status:    status,
			stream:    rng.Intn(2) == 0,
			internal:  internal,
			prompt:    prompt, completion: completion,
			input: prompt, output: completion, cached: rng.Intn(3) * rng.Intn(1000),
			reasoning: rng.Intn(2) * rng.Intn(800), effort: pick("", "", "high", "xhigh"),
			tier:     pick("", "", "priority", "fast", "default"),
			duration: rng.Intn(3) * rng.Intn(9000), firstToken: rng.Intn(3) * rng.Intn(3000),
			imageCount: rng.Intn(10) / 9, attempt: 1 + rng.Intn(4)/3,
			accountBilled: rng.Float64() * 0.37, userBilled: rng.Float64() * 0.73,
			// 成功状态也可能带错误信息(流中断等),这类行算进"仅错误"。
			errorMessage: pick("", "", "", "", "context deadline exceeded", "upstream reset"),
			errorKind:    pick("", "", "", "", "timeout", "server_error"),
			retryAttempt: rng.Intn(6) == 0,
		})
	}
	return logs
}

// approxEqualJSON 比较两个结构的 JSON 形态:浮点按相对误差 1e-9 比较(汇总与明细的求和顺序不同)。
func approxEqualJSON(t *testing.T, label string, want, got interface{}) {
	t.Helper()
	var wantTree, gotTree interface{}
	for _, item := range []struct {
		src interface{}
		dst *interface{}
	}{{want, &wantTree}, {got, &gotTree}} {
		raw, err := json.Marshal(item.src)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, item.dst); err != nil {
			t.Fatal(err)
		}
	}
	if path, ok := approxEqualTree(wantTree, gotTree, "$"); !ok {
		wantJSON, _ := json.Marshal(want)
		gotJSON, _ := json.Marshal(got)
		t.Fatalf("%s: mismatch at %s\nwant %s\n got %s", label, path, wantJSON, gotJSON)
	}
}

func approxEqualTree(a, b interface{}, path string) (string, bool) {
	switch av := a.(type) {
	case map[string]interface{}:
		bv, ok := b.(map[string]interface{})
		if !ok || len(av) != len(bv) {
			return path, false
		}
		for k, v := range av {
			if p, ok := approxEqualTree(v, bv[k], path+"."+k); !ok {
				return p, false
			}
		}
		return "", true
	case []interface{}:
		bv, ok := b.([]interface{})
		if !ok || len(av) != len(bv) {
			return path, false
		}
		for i := range av {
			if p, ok := approxEqualTree(av[i], bv[i], fmt.Sprintf("%s[%d]", path, i)); !ok {
				return p, false
			}
		}
		return "", true
	case float64:
		bv, ok := b.(float64)
		if !ok {
			return path, false
		}
		scale := math.Max(1, math.Max(math.Abs(av), math.Abs(bv)))
		return path, math.Abs(av-bv) <= 1e-9*scale
	default:
		return path, a == b
	}
}

func setUsageHourlyReady(t *testing.T, db *DB, ready bool) {
	t.Helper()
	value := 0
	if ready {
		value = 1
	}
	if _, err := db.conn.ExecContext(context.Background(), `UPDATE usage_log_hourly_state SET ready = $1 WHERE id = 1`, value); err != nil {
		t.Fatal(err)
	}
}

func TestUsageHourlyRollupMatchesRawLogsSQLite(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "hourly.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runUsageHourlyEquivalence(t, db)
}

func TestUsageHourlyRollupMatchesRawLogsPostgres(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ClearUsageLogs(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 共用测试库:跑完清掉本测试写入的日志,不影响同包其它 PostgreSQL 用例。
	defer db.ClearUsageLogs(context.Background())
	runUsageHourlyEquivalence(t, db)
}

func runUsageHourlyEquivalence(t *testing.T, db *DB) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	var keyIDs []int64
	for i := 0; i < 3; i++ {
		id, err := db.InsertAPIKeyWithOptions(ctx, APIKeyInput{Name: fmt.Sprintf("hourly-%d", i), Key: fmt.Sprintf("sk-hourly-%d-%d", i, now.UnixNano())})
		if err != nil {
			t.Fatal(err)
		}
		keyIDs = append(keyIDs, id)
	}
	defer func() {
		for _, id := range keyIDs {
			_ = db.DeleteAPIKey(ctx, id)
		}
	}()
	// 手动重置让 5h/7d 窗口从重置时刻起算,也作为一个不在整点上的分段边界。
	if _, err := db.conn.ExecContext(ctx, `UPDATE api_keys SET last_reset_at = $1 WHERE id = $2`,
		db.timeArg(now.Add(-3*time.Hour-17*time.Minute)), keyIDs[1]); err != nil {
		t.Fatal(err)
	}
	insertHourlyTestLogs(t, db, generateHourlyTestLogs(now, keyIDs, 3000))
	if err := db.RebuildUsageHourlyRollup(ctx); err != nil {
		t.Fatal(err)
	}
	if !db.usageHourlyRollupUsable(ctx) {
		t.Fatal("rollup should be usable after a rebuild")
	}
	if plan := db.planUsageHourlyQuery(ctx, now.Add(-30*24*time.Hour), now, now); !plan.useRollup {
		t.Fatal("a 30-day range should read the rollup")
	}
	if plan := db.usageLogFilterRollupPlan(ctx, UsageLogFilter{Start: now.Add(-30 * 24 * time.Hour), End: now, ErrorOnly: true}); !plan.useRollup {
		t.Fatal("error-only log counts over 30 days should read the rollup")
	}
	if plan := db.usageLogFilterRollupPlan(ctx, UsageLogFilter{Start: now.Add(-30 * 24 * time.Hour), End: now, ErrorKind: "timeout"}); plan.useRollup {
		t.Fatal("error kind is not a rollup dimension")
	}

	type statsCase struct {
		name       string
		start, end time.Time
		channel    string
		dim        UsageLogFilter
	}
	keyID := keyIDs[0]
	accountID := int64(0)
	streamOnly := true
	statsCases := []statsCase{
		{name: "today"},
		{name: "30d", start: now.Add(-30 * 24 * time.Hour), end: now},
		{name: "90d", start: now.Add(-90 * 24 * time.Hour), end: now},
		{name: "unaligned", start: now.Add(-7*24*time.Hour + 17*time.Minute), end: now.Add(-3*time.Hour + 5*time.Minute)},
		{name: "aligned", start: now.Truncate(time.Hour).Add(-26 * time.Hour), end: now.Truncate(time.Hour).Add(-time.Hour)},
		{name: "sub-hour", start: now.Add(-50 * time.Minute), end: now},
		{name: "channel", start: now.Add(-30 * 24 * time.Hour), end: now, channel: "codex"},
		{name: "model", start: now.Add(-30 * 24 * time.Hour), end: now, dim: UsageLogFilter{Model: "gpt-5.5"}},
		{name: "endpoint", start: now.Add(-30 * 24 * time.Hour), end: now, dim: UsageLogFilter{Endpoint: "/v1/responses"}},
		{name: "api-key", start: now.Add(-30 * 24 * time.Hour), end: now, channel: "grok", dim: UsageLogFilter{APIKeyID: &keyID, Model: "gpt-5.6-sol"}},
		{name: "raw-only-dims", start: now.Add(-30 * 24 * time.Hour), end: now, dim: UsageLogFilter{AccountID: &accountID, StreamOnly: &streamOnly}},
	}
	type selfCase struct {
		name       string
		keyID      int64
		start, end time.Time
	}
	var selfCases []selfCase
	for _, id := range keyIDs {
		selfCases = append(selfCases,
			selfCase{"today", id, StartOfDay(now.Local()), now},
			selfCase{"7d", id, now.Add(-7 * 24 * time.Hour), now},
			selfCase{"30d", id, now.Add(-30 * 24 * time.Hour), now},
			selfCase{"all", id, time.Time{}, now},
			selfCase{"past", id, now.Add(-20 * 24 * time.Hour), now.Add(-2*24*time.Hour - 13*time.Minute)},
		)
	}
	errorOnly := true
	logFilters := []UsageLogFilter{
		{},
		{Channel: "codex"},
		{Model: "gpt-5.5", Endpoint: "/v1/responses"},
		{APIKeyID: &keyID, StatusCode: 429},
		{StatusFamily: "5xx", IncludeCanceled: true},
		{ErrorOnly: true},
		{ErrorOnly: true, IncludeCanceled: true, Channel: "grok"},
		{ErrorKind: "timeout"},
		{StreamOnly: &errorOnly},
	}
	logRanges := [][2]time.Time{
		{now.Add(-30 * 24 * time.Hour), now},
		{now.Add(-7*24*time.Hour + 17*time.Minute), now.Add(-3*time.Hour + 5*time.Minute)},
		{now.Truncate(time.Hour).Add(-26 * time.Hour), now.Truncate(time.Hour).Add(-time.Hour)},
	}
	filters := []APIKeySelfLogFilter{
		{Model: "gpt-5.5"}, {Endpoint: "/v1/responses"}, {Endpoint: "unknown"}, {Channel: "claude"},
		{Status: "success"}, {Status: "error"}, {Status: "4xx"}, {Status: "5xx"}, {Status: "429"},
		{Stream: "stream"}, {Stream: "sync", Model: "gpt-5.6-sol", Status: "error"},
	}

	collect := func() (map[string]interface{}, error) {
		out := map[string]interface{}{}
		for _, c := range statsCases {
			stats, err := db.GetUsageStatsFiltered(ctx, c.start, c.end, c.channel, c.dim, true)
			if err != nil {
				return nil, fmt.Errorf("stats %s: %w", c.name, err)
			}
			stats.RPM, stats.TPM = 0, 0 // 依赖真实"一分钟前",两次调用之间可能跨秒
			out["stats/"+c.name] = stats
		}
		for ri, r := range logRanges {
			for fi, filter := range logFilters {
				filter.Start, filter.End = r[0], r[1]
				filter.Page, filter.PageSize = 1, 1
				page, err := db.ListUsageLogsByTimeRangePaged(ctx, filter)
				if err != nil {
					return nil, fmt.Errorf("logs %d/%d: %w", ri, fi, err)
				}
				out[fmt.Sprintf("logs-total/%d/%d", ri, fi)] = page.Total
				summary, err := db.GetUsageErrorSummary(ctx, filter)
				if err != nil {
					return nil, fmt.Errorf("error summary %d/%d: %w", ri, fi, err)
				}
				out[fmt.Sprintf("error-summary/%d/%d", ri, fi)] = summary
			}
		}
		for _, c := range selfCases {
			report := &APIKeySelfUsageReport{}
			if err := db.fillAPIKeySelfAggregates(ctx, report, c.keyID, c.start, c.end, now); err != nil {
				return nil, fmt.Errorf("self %s/%d: %w", c.name, c.keyID, err)
			}
			out[fmt.Sprintf("self/%d/%s", c.keyID, c.name)] = report
			for i, filter := range filters {
				count, err := db.countAPIKeySelfLogs(ctx, c.keyID, c.start, c.end, filter)
				if err != nil {
					return nil, fmt.Errorf("count %s/%d/%d: %w", c.name, c.keyID, i, err)
				}
				out[fmt.Sprintf("count/%d/%s/%d", c.keyID, c.name, i)] = count
			}
		}
		return out, nil
	}

	setUsageHourlyReady(t, db, false)
	want, err := collect()
	if err != nil {
		t.Fatal(err)
	}
	setUsageHourlyReady(t, db, true)
	got, err := collect()
	if err != nil {
		t.Fatal(err)
	}
	for name := range want {
		approxEqualJSON(t, name, want[name], got[name])
	}
	if stats := want["stats/30d"].(*UsageStats); stats.TodayRequests == 0 || len(stats.ModelStats) == 0 {
		t.Fatalf("test data should produce non-empty 30d stats: %+v", stats)
	}
}

// 正常写入路径(批量落库)在同一事务里维护汇总:每个小时的请求数与明细一致,
// last_log_id 跟上 MAX(id),查询可以直接读汇总。
func TestUsageHourlyRollupMaintainedByLogFlush(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "hourly-flush.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runUsageHourlyFlushMaintenance(t, db)
}

func TestUsageHourlyRollupMaintainedByLogFlushPostgres(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ClearUsageLogs(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer db.ClearUsageLogs(context.Background())
	runUsageHourlyFlushMaintenance(t, db)
}

func runUsageHourlyFlushMaintenance(t *testing.T, db *DB) {
	ctx := context.Background()
	// 两次落库命中同一批汇总行,第二次走 ON CONFLICT 累加。
	for round := 0; round < 2; round++ {
		for i := 0; i < 25; i++ {
			if err := db.InsertUsageLog(ctx, &UsageLogInput{
				Endpoint: "/v1/responses", Model: "gpt-5.5", StatusCode: []int{200, 429, 499}[i%3],
				PromptTokens: 100 + i, CompletionTokens: 10, TotalTokens: 110 + i, DurationMs: 50 * i,
				APIKeyID: int64(1 + i%2), Channel: "codex", Stream: i%2 == 0,
			}); err != nil {
				t.Fatal(err)
			}
		}
		db.FlushUsageLogs()
	}
	assertUsageHourlyMatchesLogs(t, db)
	var groups, requests int64
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*), `+sumBigint("requests")+` FROM usage_log_hourly`).Scan(&groups, &requests); err != nil {
		t.Fatal(err)
	}
	if requests != 50 || groups >= 50 {
		t.Fatalf("rollup has %d groups / %d requests, want 50 requests merged into fewer groups", groups, requests)
	}
	if !db.usageHourlyRollupUsable(ctx) {
		t.Fatal("flush-maintained rollup should be usable")
	}

	if err := db.ClearUsageLogs(ctx); err != nil {
		t.Fatal(err)
	}
	var rows int64
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_log_hourly`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("clearing logs left %d rollup rows", rows)
	}
	if !db.usageHourlyRollupUsable(ctx) {
		t.Fatal("rollup should stay usable after clearing logs")
	}
}

func assertUsageHourlyMatchesLogs(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	query := func(q string) map[string][2]float64 {
		rows, err := db.conn.QueryContext(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string][2]float64{}
		for rows.Next() {
			var raw interface{}
			var requests int64
			var billed float64
			if err := rows.Scan(&raw, &requests, &billed); err != nil {
				t.Fatal(err)
			}
			hour, err := parseDBTimeValue(raw)
			if err != nil {
				t.Fatal(err)
			}
			out[hour.UTC().Format(time.RFC3339)] = [2]float64{float64(requests), billed}
		}
		return out
	}
	logs := query(`SELECT ` + db.usageHourlyBucketExpr("created_at") + `, COUNT(*), COALESCE(SUM(user_billed), 0) FROM usage_logs GROUP BY 1`)
	rolled := query(`SELECT bucket, ` + sumBigint("requests") + `, COALESCE(SUM(user_billed), 0) FROM usage_log_hourly GROUP BY bucket`)
	approxEqualJSON(t, "hourly totals", logs, rolled)
}

// 升级场景:库里已有历史日志但没有汇总,启动时登记回填边界,后台分块回填后与明细一致;
// 回填期间查询照旧扫明细。
func TestUsageHourlyRollupBackfillsExistingHistory(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "hourly-backfill.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	insertHourlyTestLogs(t, db, generateHourlyTestLogs(now, []int64{1, 2}, usageHourlyBackfillChunkSQLite*2+77))
	if _, err := db.conn.ExecContext(ctx, `DELETE FROM usage_log_hourly_state`); err != nil {
		t.Fatal(err)
	}
	pending, err := db.initUsageHourlyRollup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("existing history should need a backfill")
	}
	if db.usageHourlyRollupUsable(ctx) {
		t.Fatal("rollup must not be read before the backfill completes")
	}
	steps := 0
	for {
		done, err := db.usageHourlyBackfillStep(ctx)
		if err != nil {
			t.Fatal(err)
		}
		steps++
		if done {
			break
		}
	}
	if steps != 3 {
		t.Fatalf("backfill took %d steps, want 3 chunks", steps)
	}
	assertUsageHourlyMatchesLogs(t, db)
	if !db.usageHourlyRollupUsable(ctx) {
		t.Fatal("rollup should be usable after the backfill")
	}
}

// 绕过写入队列插入的行:查询前比对 MAX(id) 发现后改扫明细,修复后重新可用;
// 定时核对按小时行数发现被改坏的汇总并重算。
func TestUsageHourlyRollupRepairsOutOfBandRows(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "hourly-repair.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	insertHourlyTestLogs(t, db, []hourlyTestLog{{at: now.Add(-50 * time.Hour), apiKeyID: 1, status: 200, userBilled: 1}})
	if db.usageHourlyRollupUsable(ctx) {
		t.Fatal("an out-of-band insert must disable rollup reads until repaired")
	}
	db.repairUsageHourly(ctx)
	if !db.usageHourlyRollupUsable(ctx) {
		t.Fatal("repair should bring the rollup back in sync")
	}
	assertUsageHourlyMatchesLogs(t, db)

	recent := now.Truncate(time.Hour).Add(-2 * time.Hour).Add(10 * time.Minute)
	insertHourlyTestLogs(t, db, []hourlyTestLog{{at: recent, apiKeyID: 2, status: 200, userBilled: 2}})
	db.repairUsageHourly(ctx)
	// 模拟没进汇总的行:直接改小汇总的请求数,MAX(id) 比对发现不了。
	if _, err := db.conn.ExecContext(ctx, `UPDATE usage_log_hourly SET requests = requests + 5 WHERE bucket = $1`,
		db.timeArg(recent.Truncate(time.Hour))); err != nil {
		t.Fatal(err)
	}
	db.verifyRecentUsageHourly(ctx)
	assertUsageHourlyMatchesLogs(t, db)
}

// 删除了已登记的行(MAX(id) 倒退)时汇总无从对账,整表重建。
func TestUsageHourlyRollupRebuildsAfterDeletedRows(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "hourly-deleted.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	insertHourlyTestLogs(t, db, generateHourlyTestLogs(now, []int64{1}, 40))
	if err := db.RebuildUsageHourlyRollup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `DELETE FROM usage_logs WHERE id > (SELECT MAX(id) - 5 FROM usage_logs)`); err != nil {
		t.Fatal(err)
	}
	if db.usageHourlyRollupUsable(ctx) {
		t.Fatal("deleted rows must disable rollup reads")
	}
	db.repairUsageHourly(ctx)
	assertUsageHourlyMatchesLogs(t, db)
	if !db.usageHourlyRollupUsable(ctx) {
		t.Fatal("rollup should be usable after the rebuild")
	}
}
