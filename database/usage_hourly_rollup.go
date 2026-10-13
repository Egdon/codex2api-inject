package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"time"
)

// usage_log_hourly 按小时预聚合 usage_logs(issue #778)。慢盘 SQLite 上,30 天的自助用量页
// 与长区间的用量页要把区间内每一行明细读出来;汇总表把同一小时、同一组维度的请求压成一行,
// 长区间统计改读整点小时的汇总,只有首尾不足一小时的零头仍扫明细。
//
// 维度覆盖自助页与管理端区间卡片、分项排行用到的全部分组与筛选列;499 与内部请求也入表,
// 由查询方按各自口径排除。汇总是明细的精确镜像:批量写日志时在同一事务里按刚插入的行 id
// 累加,清空日志时一并清空。升级前的存量历史由后台按 id 分块回填,回填完成前查询照旧扫明细。
// 绕过写入队列的插入(手工 SQL、滚动升级时仍在跑的旧版本实例)由两道检查兜底:查询前比对
// MAX(id) 发现没进汇总的新行就改扫明细并通知后台修复;后台再定时核对最近几个整点小时的行数。
const (
	// 改动聚合口径、新增聚合列,或数据迁移改写了 usage_logs 里汇总用到的列时提升版本号:
	// 启动时补齐缺的聚合列,清空汇总并按新口径重新回填。维度(主键)变了要换表名。
	usageHourlyRollupVersion = 1

	usageHourlyMaintainIDChunk       = 500
	usageHourlyBackfillChunkSQLite   = 2000
	usageHourlyBackfillChunkPostgres = 20000
	// 每块之间让出写锁,慢盘上回填期间日志落库不至于长时间排队。
	usageHourlyBackfillPause      = 50 * time.Millisecond
	usageHourlyBackfillRetryDelay = time.Minute
	usageHourlyVerifyInterval     = 20 * time.Minute
	usageHourlyVerifyHours        = 3
	// 修复时涉及的小时数超过这个值就整表重建(后台分块回填),不在一个事务里逐小时重算。
	usageHourlyRepairMaxHours = 72
)

const usageHourlyKeyColumns = `bucket, api_key_id, api_key_name, api_key_masked, channel, model, effective_model,
	inbound_endpoint, endpoint, status_code, stream, internal`

// usageHourlyAggregates 是汇总列与对应的明细聚合表达式。duration_ms_sum 同时服务两种口径:
// 管理端 AVG(duration_ms) = duration_ms_sum / duration_rows,自助页 AVG(NULLIF(duration_ms, 0))
// = duration_ms_sum / duration_samples(耗时与首字都不会是负数,> 0 与 <> 0 等价)。
var usageHourlyAggregates = []struct{ column, expr string }{
	{"requests", "COUNT(*)"},
	{"total_tokens", "COALESCE(SUM(u.total_tokens), 0)"},
	{"prompt_tokens", "COALESCE(SUM(u.prompt_tokens), 0)"},
	{"completion_tokens", "COALESCE(SUM(u.completion_tokens), 0)"},
	{"input_tokens", "COALESCE(SUM(u.input_tokens), 0)"},
	{"output_tokens", "COALESCE(SUM(u.output_tokens), 0)"},
	{"cached_tokens", "COALESCE(SUM(u.cached_tokens), 0)"},
	{"account_billed", "COALESCE(SUM(u.account_billed), 0)"},
	{"user_billed", "COALESCE(SUM(u.user_billed), 0)"},
	{"duration_ms_sum", "COALESCE(SUM(u.duration_ms), 0)"},
	{"duration_rows", "COUNT(u.duration_ms)"},
	{"duration_samples", "COALESCE(SUM(CASE WHEN u.duration_ms > 0 THEN 1 ELSE 0 END), 0)"},
	{"first_token_ms_sum", "COALESCE(SUM(CASE WHEN u.first_token_ms > 0 THEN u.first_token_ms ELSE 0 END), 0)"},
	{"first_token_samples", "COALESCE(SUM(CASE WHEN u.first_token_ms > 0 THEN 1 ELSE 0 END), 0)"},
	{"cache_hit_requests", "COALESCE(SUM(CASE WHEN u.cached_tokens > 0 THEN 1 ELSE 0 END), 0)"},
	{"fast_requests", "COALESCE(SUM(CASE WHEN LOWER(COALESCE(NULLIF(u.billing_service_tier, ''), u.service_tier, '')) IN ('fast', 'priority', 'ultrafast') THEN 1 ELSE 0 END), 0)"},
	{"reasoning_requests", "COALESCE(SUM(CASE WHEN u.reasoning_tokens > 0 OR NULLIF(u.reasoning_effort, '') IS NOT NULL THEN 1 ELSE 0 END), 0)"},
	{"image_requests", "COALESCE(SUM(CASE WHEN LOWER(COALESCE(NULLIF(u.inbound_endpoint, ''), u.endpoint, '')) LIKE '%/images/%' OR LOWER(COALESCE(u.model, '')) LIKE 'gpt-image-%' OR u.image_count > 0 THEN 1 ELSE 0 END), 0)"},
	// attempt_index 是 1-based,> 1 才是重试出来的请求(与管理端分项口径一致)。
	{"retry_requests", "COALESCE(SUM(CASE WHEN u.attempt_index > 1 THEN 1 ELSE 0 END), 0)"},
	// 以下只统计错误行(与请求日志"仅错误"筛选同一判定),供错误摘要与错误筛选计数。
	{"error_rows", "COALESCE(SUM(CASE WHEN " + usageLogErrorPredicate + " THEN 1 ELSE 0 END), 0)"},
	{"error_duration_ms_sum", "COALESCE(SUM(CASE WHEN " + usageLogErrorPredicate + " THEN u.duration_ms ELSE 0 END), 0)"},
	{"error_duration_rows", "COALESCE(SUM(CASE WHEN (" + usageLogErrorPredicate + ") AND u.duration_ms IS NOT NULL THEN 1 ELSE 0 END), 0)"},
	{"error_timeouts", "COALESCE(SUM(CASE WHEN (" + usageLogErrorPredicate + ") AND (" + usageLogTimeoutPredicate + ") THEN 1 ELSE 0 END), 0)"},
	{"error_retry_attempts", "COALESCE(SUM(CASE WHEN (" + usageLogErrorPredicate + ") AND COALESCE(u.is_retry_attempt, false) THEN 1 ELSE 0 END), 0)"},
}

const (
	usageLogErrorPredicate   = `u.status_code >= 400 OR COALESCE(u.error_message, '') <> '' OR COALESCE(u.upstream_error_kind, '') <> ''`
	usageLogTimeoutPredicate = `LOWER(COALESCE(u.upstream_error_kind, '')) LIKE '%timeout%'
			OR LOWER(COALESCE(u.error_message, '')) LIKE '%timeout%'
			OR LOWER(COALESCE(u.error_message, '')) LIKE '%deadline%'`
)

func (db *DB) ensureUsageHourlySchema(ctx context.Context) error {
	intType, floatType, boolType, timeType := "BIGINT", "DOUBLE PRECISION", "BOOLEAN NOT NULL DEFAULT FALSE", "TIMESTAMPTZ"
	if db.isSQLite() {
		intType, floatType, boolType, timeType = "INTEGER", "REAL", "INTEGER NOT NULL DEFAULT 0", "TIMESTAMP"
	}
	aggregateColumnDef := func(column string) string {
		if column == "account_billed" || column == "user_billed" {
			return floatType + " NOT NULL DEFAULT 0"
		}
		return intType + " NOT NULL DEFAULT 0"
	}
	var columns strings.Builder
	for _, agg := range usageHourlyAggregates {
		fmt.Fprintf(&columns, "\t\t%s %s,\n", agg.column, aggregateColumnDef(agg.column))
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS usage_log_hourly (
		bucket ` + timeType + ` NOT NULL,
		api_key_id ` + intType + ` NOT NULL DEFAULT 0,
		api_key_name TEXT NOT NULL DEFAULT '',
		api_key_masked TEXT NOT NULL DEFAULT '',
		channel TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		effective_model TEXT NOT NULL DEFAULT '',
		inbound_endpoint TEXT NOT NULL DEFAULT '',
		endpoint TEXT NOT NULL DEFAULT '',
		status_code INTEGER NOT NULL DEFAULT 0,
		stream ` + boolType + `,
		internal INTEGER NOT NULL DEFAULT 0,
` + columns.String() + `		min_created_at ` + timeType + `,
		PRIMARY KEY (` + usageHourlyKeyColumns + `)
	)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_log_hourly_api_key_bucket ON usage_log_hourly (api_key_id, bucket)`,
		`CREATE TABLE IF NOT EXISTS usage_log_hourly_state (
		id INTEGER PRIMARY KEY,
		version INTEGER NOT NULL DEFAULT 0,
		ready INTEGER NOT NULL DEFAULT 0,
		backfill_to_id BIGINT NOT NULL DEFAULT 0,
		backfill_cursor BIGINT NOT NULL DEFAULT 0,
		last_log_id BIGINT NOT NULL DEFAULT 0,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`,
	}
	for _, statement := range statements {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	// 后续版本新增的聚合列在这里补上,配合提升版本号重新回填。
	if db.isSQLite() {
		existing, err := db.sqliteTableColumns(ctx, "usage_log_hourly")
		if err != nil {
			return err
		}
		for _, agg := range usageHourlyAggregates {
			if _, ok := existing[agg.column]; ok {
				continue
			}
			if _, err := db.conn.ExecContext(ctx, "ALTER TABLE usage_log_hourly ADD COLUMN "+agg.column+" "+aggregateColumnDef(agg.column)); err != nil {
				return err
			}
		}
		return nil
	}
	for _, agg := range usageHourlyAggregates {
		if _, err := db.conn.ExecContext(ctx, "ALTER TABLE usage_log_hourly ADD COLUMN IF NOT EXISTS "+agg.column+" "+aggregateColumnDef(agg.column)); err != nil {
			return err
		}
	}
	return nil
}

// initUsageHourlyRollup 在日志写入协程启动前确定回填边界:版本不符(含首次升级)就清空汇总,
// 把当前 MAX(id) 记为回填上界,此后的新日志由写入事务维护。返回值表示是否还有存量待回填。
func (db *DB) initUsageHourlyRollup(ctx context.Context) (bool, error) {
	needsBackfill := false
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if !db.isSQLite() {
			// 等在途的写入事务提交,让 MAX(id) 与"由写入事务负责的 id"之间没有缝隙。
			if _, err := tx.ExecContext(ctx, `LOCK TABLE usage_log_hourly IN EXCLUSIVE MODE`); err != nil {
				return err
			}
		}
		var version, ready int
		err := tx.QueryRowContext(ctx, `SELECT version, ready FROM usage_log_hourly_state WHERE id = 1`).Scan(&version, &ready)
		if err == nil && version == usageHourlyRollupVersion {
			needsBackfill = ready != 1
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		pending, err := db.resetUsageHourlyRollupTx(ctx, tx)
		needsBackfill = pending
		return err
	})
	return needsBackfill, err
}

// resetUsageHourlyRollupTx 清空汇总并从头登记回填。调用方须已持有汇总表的写锁。
func (db *DB) resetUsageHourlyRollupTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_log_hourly`); err != nil {
		return false, err
	}
	var maxID int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM usage_logs`).Scan(&maxID); err != nil {
		return false, err
	}
	ready := 0
	if maxID == 0 {
		ready = 1
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_log_hourly_state (id, version, ready, backfill_to_id, backfill_cursor, last_log_id, updated_at)
		VALUES (1, $1, $2, $3, 0, $3, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET version = excluded.version, ready = excluded.ready,
			backfill_to_id = excluded.backfill_to_id, backfill_cursor = 0,
			last_log_id = excluded.last_log_id, updated_at = CURRENT_TIMESTAMP`,
		usageHourlyRollupVersion, ready, maxID)
	return ready == 0, err
}

// clearUsageHourlyRollupTx 随清空日志一起清空汇总;没有存量需要回填,直接就绪。
func (db *DB) clearUsageHourlyRollupTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_log_hourly`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE usage_log_hourly_state SET version = $1, ready = 1,
		backfill_to_id = 0, backfill_cursor = 0, last_log_id = 0, updated_at = CURRENT_TIMESTAMP WHERE id = 1`,
		usageHourlyRollupVersion)
	return err
}

func (db *DB) usageHourlyBucketExpr(column string) string {
	if db.isSQLite() {
		return "strftime('%Y-%m-%d %H:00:00', " + column + ")"
	}
	// 与会话时区无关:按 UTC 整点截断,和 Go 侧 Truncate(time.Hour) 对齐。
	return "(date_trunc('hour', " + column + " AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')"
}

// usageHourlyUpsertSQL 把满足 where 的明细行按小时与维度聚合后累加进汇总表。
// 增量维护、存量回填与按小时重算共用这一条聚合定义,保证口径一致。
// ORDER BY 让并发事务按同一顺序锁汇总行,避免 PostgreSQL 死锁。
func (db *DB) usageHourlyUpsertSQL(where string) string {
	groupBy := "1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12"
	columns := []string{usageHourlyKeyColumns}
	selects := []string{
		db.usageHourlyBucketExpr("u.created_at"),
		"COALESCE(u.api_key_id, 0)",
		"COALESCE(u.api_key_name, '')",
		"COALESCE(u.api_key_masked, '')",
		"COALESCE(u.channel, '')",
		"COALESCE(u.model, '')",
		"COALESCE(u.effective_model, '')",
		"COALESCE(u.inbound_endpoint, '')",
		"COALESCE(u.endpoint, '')",
		"COALESCE(u.status_code, 0)",
		"COALESCE(u.stream, false)",
		"CASE WHEN TRIM(COALESCE(u.internal_reason, '')) <> '' THEN 1 ELSE 0 END",
	}
	updates := make([]string, 0, len(usageHourlyAggregates)+1)
	for _, agg := range usageHourlyAggregates {
		columns = append(columns, agg.column)
		selects = append(selects, agg.expr)
		updates = append(updates, fmt.Sprintf("%[1]s = usage_log_hourly.%[1]s + excluded.%[1]s", agg.column))
	}
	columns = append(columns, "min_created_at")
	selects = append(selects, "MIN(u.created_at)")
	if db.isSQLite() {
		updates = append(updates, `min_created_at = MIN(COALESCE(usage_log_hourly.min_created_at, excluded.min_created_at),
			COALESCE(excluded.min_created_at, usage_log_hourly.min_created_at))`)
	} else {
		updates = append(updates, "min_created_at = LEAST(usage_log_hourly.min_created_at, excluded.min_created_at)")
	}
	return `INSERT INTO usage_log_hourly (` + strings.Join(columns, ", ") + `)
		SELECT ` + strings.Join(selects, ",\n\t\t\t") + `
		FROM usage_logs u
		WHERE ` + where + `
		GROUP BY ` + groupBy + `
		ORDER BY ` + groupBy + `
		ON CONFLICT (` + usageHourlyKeyColumns + `) DO UPDATE SET ` + strings.Join(updates, ",\n\t\t\t")
}

func (db *DB) usageHourlyMaxExpr(a, b string) string {
	if db.isSQLite() {
		return "MAX(" + a + ", " + b + ")"
	}
	return "GREATEST(" + a + ", " + b + ")"
}

// applyUsageHourlyRollupWithExec 在写日志的事务里把刚插入的行累加进汇总。
// 只认回填上界之后的 id:更早的行由回填负责,两边不会重复计数。
func (db *DB) applyUsageHourlyRollupWithExec(ctx context.Context, execer sqlExecer, ids []int64) error {
	if execer == nil || len(ids) == 0 {
		return nil
	}
	maxID := slices.Max(ids)
	for start := 0; start < len(ids); start += usageHourlyMaintainIDChunk {
		chunk := ids[start:min(start+usageHourlyMaintainIDChunk, len(ids))]
		placeholders := dbPlaceholders(false, 1, len(chunk))
		where := "u.id IN (" + strings.Join(placeholders, ", ") + `)
			AND u.id > COALESCE((SELECT backfill_to_id FROM usage_log_hourly_state WHERE id = 1), 0)`
		if _, err := execer.ExecContext(ctx, db.usageHourlyUpsertSQL(where), argsFromInt64s(chunk)...); err != nil {
			return err
		}
	}
	// 取本批自己的最大 id 而不是表的 MAX(id):后者会把同时混进来、没进汇总的行一并"认领"。
	_, err := execer.ExecContext(ctx, `UPDATE usage_log_hourly_state SET last_log_id = `+
		db.usageHourlyMaxExpr("last_log_id", "$1")+` WHERE id = 1`, maxID)
	return err
}

// lockUsageHourlyForRebuildTx 在重算前挡住并发的增量累加。PostgreSQL 锁表等在途写入提交;
// SQLite 先写一笔拿到写锁(WAL 下先读后写的延迟事务可能升级失败)。
func (db *DB) lockUsageHourlyForRebuildTx(ctx context.Context, tx *sql.Tx) error {
	if db.isSQLite() {
		_, err := tx.ExecContext(ctx, `UPDATE usage_log_hourly_state SET updated_at = CURRENT_TIMESTAMP WHERE id = 1`)
		return err
	}
	_, err := tx.ExecContext(ctx, `LOCK TABLE usage_log_hourly IN SHARE ROW EXCLUSIVE MODE`)
	return err
}

// rebuildUsageHourlyHoursTx 按明细重算给定整点小时的汇总。
func (db *DB) rebuildUsageHourlyHoursTx(ctx context.Context, tx *sql.Tx, hours []time.Time) error {
	for _, hour := range hours {
		hour = hour.UTC().Truncate(time.Hour)
		if _, err := tx.ExecContext(ctx, `DELETE FROM usage_log_hourly WHERE bucket = $1`, db.timeArg(hour)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, db.usageHourlyUpsertSQL(`u.created_at >= $1 AND u.created_at < $2`),
			db.timeArg(hour), db.timeArg(hour.Add(time.Hour))); err != nil {
			return err
		}
	}
	return nil
}

// RebuildUsageHourlyRollup 按明细同步重建整张汇总表。给直接改写 usage_logs 的测试与诊断用;
// 正常运行不需要调用。
func (db *DB) RebuildUsageHourlyRollup(ctx context.Context) error {
	if _, err := db.initUsageHourlyRollupReset(ctx); err != nil {
		return err
	}
	for {
		done, err := db.usageHourlyBackfillStep(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

func (db *DB) initUsageHourlyRollupReset(ctx context.Context) (bool, error) {
	pending := false
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if err := db.lockUsageHourlyForRebuildTx(ctx, tx); err != nil {
			return err
		}
		var err error
		pending, err = db.resetUsageHourlyRollupTx(ctx, tx)
		return err
	})
	return pending, err
}

var errUsageHourlyBackfillRaced = errors.New("usage hourly backfill cursor moved concurrently")

// usageHourlyBackfillStep 回填下一块存量 id。先累加汇总、再以游标比较交换推进,
// 加锁顺序与写日志事务一致(汇总行在前、状态行在后);另一个实例抢先推进时整块回滚重来。
func (db *DB) usageHourlyBackfillStep(ctx context.Context) (bool, error) {
	chunk := int64(usageHourlyBackfillChunkPostgres)
	if db.isSQLite() {
		chunk = usageHourlyBackfillChunkSQLite
	}
	done := false
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if db.isSQLite() {
			if _, err := tx.ExecContext(ctx, `UPDATE usage_log_hourly_state SET updated_at = CURRENT_TIMESTAMP WHERE id = 1`); err != nil {
				return err
			}
		}
		var version, ready int
		var to, cursor int64
		if err := tx.QueryRowContext(ctx, `SELECT version, ready, backfill_to_id, backfill_cursor
			FROM usage_log_hourly_state WHERE id = 1`).Scan(&version, &ready, &to, &cursor); err != nil {
			return err
		}
		if version != usageHourlyRollupVersion || ready == 1 {
			done = true
			return nil
		}
		hi := min(cursor+chunk, to)
		if hi > cursor {
			if _, err := tx.ExecContext(ctx, db.usageHourlyUpsertSQL(`u.id > $1 AND u.id <= $2`), cursor, hi); err != nil {
				return err
			}
		}
		nextReady := 0
		if hi >= to {
			nextReady = 1
		}
		res, err := tx.ExecContext(ctx, `UPDATE usage_log_hourly_state SET backfill_cursor = $1, ready = $2, updated_at = CURRENT_TIMESTAMP
			WHERE id = 1 AND version = $3 AND backfill_cursor = $4 AND ready = 0`, hi, nextReady, usageHourlyRollupVersion, cursor)
		if err != nil {
			return err
		}
		if affected, err := res.RowsAffected(); err == nil && affected == 0 {
			return errUsageHourlyBackfillRaced
		}
		done = nextReady == 1
		return nil
	})
	if errors.Is(err, errUsageHourlyBackfillRaced) {
		return false, nil
	}
	return done, err
}

func (db *DB) runUsageHourlyBackfill(ctx context.Context) {
	started := time.Now()
	chunks := 0
	for {
		done, err := db.usageHourlyBackfillStep(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("回填用量小时汇总失败，%s 后重试: %v", usageHourlyBackfillRetryDelay, err)
			if !sleepContext(ctx, usageHourlyBackfillRetryDelay) {
				return
			}
			continue
		}
		chunks++
		if done {
			log.Printf("用量小时汇总回填完成：%d 块，用时 %s", chunks, time.Since(started).Round(time.Millisecond))
			return
		}
		if !sleepContext(ctx, usageHourlyBackfillPause) {
			return
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// startUsageHourlyMaintainer 跑回填、按需修复与定时核对。挂在日志写入协程同一组生命周期上,
// Close 关闭 logStop 时立即取消,不占用后台任务的优雅退出窗口。
func (db *DB) startUsageHourlyMaintainer(backfill bool) {
	db.logWg.Add(1)
	go func() {
		defer db.logWg.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-db.logStop:
				cancel()
			case <-ctx.Done():
			}
		}()
		if backfill {
			db.runUsageHourlyBackfill(ctx)
		}
		ticker := time.NewTicker(usageHourlyVerifyInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-db.usageHourlyRepairNotify:
				db.repairUsageHourly(ctx)
			case <-ticker.C:
				db.verifyRecentUsageHourly(ctx)
			}
		}
	}()
}

func (db *DB) requestUsageHourlyRepair() {
	select {
	case db.usageHourlyRepairNotify <- struct{}{}:
	default:
	}
}

// usageHourlyRollupUsable 判断查询能否读汇总:回填已完成,且汇总已覆盖明细里最新的行。
// 状态与 MAX(id) 在同一条语句里读,PostgreSQL 下是同一个快照。
func (db *DB) usageHourlyRollupUsable(ctx context.Context) bool {
	var version, ready int
	var lastLogID, maxID int64
	err := db.conn.QueryRowContext(ctx, `SELECT s.version, s.ready, s.last_log_id,
		(SELECT COALESCE(MAX(id), 0) FROM usage_logs)
		FROM usage_log_hourly_state s WHERE s.id = 1`).Scan(&version, &ready, &lastLogID, &maxID)
	if err != nil || version != usageHourlyRollupVersion || ready != 1 {
		return false
	}
	if maxID != lastLogID {
		db.requestUsageHourlyRepair()
		return false
	}
	return true
}

// repairUsageHourly 补齐绕过写入队列插入的行:重算这些行所在的小时。MAX(id) 比记录的
// 还小说明有行被删掉,汇总无从对账,整表重建。
func (db *DB) repairUsageHourly(ctx context.Context) {
	reset := false
	repaired := 0
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if err := db.lockUsageHourlyForRebuildTx(ctx, tx); err != nil {
			return err
		}
		var ready int
		var lastLogID, maxID int64
		if err := tx.QueryRowContext(ctx, `SELECT s.ready, s.last_log_id, (SELECT COALESCE(MAX(id), 0) FROM usage_logs)
			FROM usage_log_hourly_state s WHERE s.id = 1`).Scan(&ready, &lastLogID, &maxID); err != nil {
			return err
		}
		if ready != 1 || maxID == lastLogID {
			return nil
		}
		if maxID > lastLogID {
			hours, err := db.usageHourlyBucketsAfterID(ctx, tx, lastLogID)
			if err != nil {
				return err
			}
			if len(hours) <= usageHourlyRepairMaxHours {
				if err := db.rebuildUsageHourlyHoursTx(ctx, tx, hours); err != nil {
					return err
				}
				repaired = len(hours)
				_, err := tx.ExecContext(ctx, `UPDATE usage_log_hourly_state SET last_log_id = $1, updated_at = CURRENT_TIMESTAMP WHERE id = 1`, maxID)
				return err
			}
		}
		reset = true
		_, err := db.resetUsageHourlyRollupTx(ctx, tx)
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("修复用量小时汇总失败: %v", err)
		}
		return
	}
	if reset {
		log.Printf("用量小时汇总与明细对不上，已清空并在后台重新回填")
		db.runUsageHourlyBackfill(ctx)
	} else if repaired > 0 {
		log.Printf("用量小时汇总已补齐写入队列之外插入的明细：重算 %d 个小时", repaired)
	}
}

func (db *DB) usageHourlyBucketsAfterID(ctx context.Context, tx *sql.Tx, afterID int64) ([]time.Time, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT `+db.usageHourlyBucketExpr("u.created_at")+`
		FROM usage_logs u WHERE u.id > $1`, afterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hours []time.Time
	for rows.Next() {
		var raw interface{}
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		hour, err := parseDBTimeValue(raw)
		if err != nil {
			return nil, err
		}
		if !hour.IsZero() {
			hours = append(hours, hour)
		}
	}
	return hours, rows.Err()
}

// verifyRecentUsageHourly 核对最近几个已结束小时的行数,对不上就重算。覆盖查询前 MAX(id)
// 比对发现不了的情况:旧版本实例写入的行 id 夹在新实例已登记的 id 之间。
func (db *DB) verifyRecentUsageHourly(ctx context.Context) {
	var ready int
	if err := db.conn.QueryRowContext(ctx, `SELECT ready FROM usage_log_hourly_state WHERE id = 1`).Scan(&ready); err != nil || ready != 1 {
		return
	}
	current := time.Now().UTC().Truncate(time.Hour)
	var stale []time.Time
	for i := 1; i <= usageHourlyVerifyHours; i++ {
		hour := current.Add(-time.Duration(i) * time.Hour)
		var logged, rolled int64
		if err := db.conn.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM usage_logs WHERE created_at >= $1 AND created_at < $2),
			(SELECT CAST(COALESCE(SUM(requests), 0) AS BIGINT) FROM usage_log_hourly WHERE bucket = $1)`,
			db.timeArg(hour), db.timeArg(hour.Add(time.Hour))).Scan(&logged, &rolled); err != nil {
			return
		}
		if logged != rolled {
			stale = append(stale, hour)
		}
	}
	if len(stale) == 0 {
		return
	}
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if err := db.lockUsageHourlyForRebuildTx(ctx, tx); err != nil {
			return err
		}
		return db.rebuildUsageHourlyHoursTx(ctx, tx, stale)
	})
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("核对用量小时汇总失败: %v", err)
		}
		return
	}
	log.Printf("用量小时汇总核对发现 %d 个小时与明细行数不一致，已按明细重算", len(stale))
}

// usageTimeSpan 是半开区间 [start, end);start 零值表示不设下界,end 零值表示不设上界。
type usageTimeSpan struct {
	start, end time.Time
}

// usageHourlyPlan 描述一次区间查询如何拆分:汇总表负责 [from, to) 内除 skip 以外的整点小时,
// raw 里的区间回明细表扫描。useRollup 为 false 时整个区间都扫明细。
type usageHourlyPlan struct {
	useRollup bool
	from, to  time.Time // from 零值表示不设下界
	skip      []time.Time
	raw       []usageTimeSpan
}

// planUsageHourly 把 [start, end) 拆成整点小时与零头。cuts 是查询内部的分段边界(窗口起点、
// "一分钟前"等):落在某个小时中间的边界把那个小时整个交给明细,汇总里的每一行就一定
// 只属于一个分段。end 为零值时尾部零头从当前小时起不设上界。
func planUsageHourly(start, end, now time.Time, cuts ...time.Time) usageHourlyPlan {
	upper := end
	if upper.IsZero() {
		upper = now
	}
	to := upper.UTC().Truncate(time.Hour)
	var from time.Time
	if !start.IsZero() {
		from = start.UTC().Truncate(time.Hour)
		if from.Before(start) {
			from = from.Add(time.Hour)
		}
	}
	if !from.IsZero() && !from.Before(to) {
		return usageHourlyPlan{raw: []usageTimeSpan{{start, end}}}
	}
	plan := usageHourlyPlan{useRollup: true, from: from, to: to}
	if !start.IsZero() && start.Before(from) {
		plan.raw = append(plan.raw, usageTimeSpan{start, from})
	}
	if end.IsZero() || to.Before(end) {
		plan.raw = append(plan.raw, usageTimeSpan{to, end})
	}
	for _, cut := range cuts {
		if cut.IsZero() {
			continue
		}
		hour := cut.UTC().Truncate(time.Hour)
		if hour.Equal(cut) || (!from.IsZero() && hour.Before(from)) || !hour.Before(to) {
			continue
		}
		if slices.ContainsFunc(plan.skip, hour.Equal) {
			continue
		}
		plan.skip = append(plan.skip, hour)
		plan.raw = append(plan.raw, usageTimeSpan{hour, hour.Add(time.Hour)})
	}
	plan.raw = mergeUsageTimeSpans(plan.raw)
	return plan
}

func mergeUsageTimeSpans(spans []usageTimeSpan) []usageTimeSpan {
	if len(spans) < 2 {
		return spans
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start.IsZero() != spans[j].start.IsZero() {
			return spans[i].start.IsZero()
		}
		return spans[i].start.Before(spans[j].start)
	})
	merged := []usageTimeSpan{spans[0]}
	for _, span := range spans[1:] {
		last := &merged[len(merged)-1]
		if last.end.IsZero() {
			continue
		}
		if !span.start.After(last.end) {
			if span.end.IsZero() || span.end.After(last.end) {
				last.end = span.end
			}
			continue
		}
		merged = append(merged, span)
	}
	return merged
}

// usageSpanWhere 生成 column 落在 span 内的条件,参数编号接在 args 之后。
func (db *DB) usageSpanWhere(column string, span usageTimeSpan, args []interface{}) (string, []interface{}) {
	parts := make([]string, 0, 2)
	if !span.start.IsZero() {
		args = append(args, db.timeArg(span.start))
		parts = append(parts, fmt.Sprintf("%s >= $%d", column, len(args)))
	}
	if !span.end.IsZero() {
		args = append(args, db.timeArg(span.end))
		parts = append(parts, fmt.Sprintf("%s < $%d", column, len(args)))
	}
	if len(parts) == 0 {
		return "1 = 1", args
	}
	return strings.Join(parts, " AND "), args
}

// usageHourlyBucketWhere 生成汇总表在 plan 下负责的小时条件。
func (db *DB) usageHourlyBucketWhere(column string, plan usageHourlyPlan, args []interface{}) (string, []interface{}) {
	where, args := db.usageSpanWhere(column, usageTimeSpan{plan.from, plan.to}, args)
	if len(plan.skip) > 0 {
		placeholders := make([]string, len(plan.skip))
		for i, hour := range plan.skip {
			args = append(args, db.timeArg(hour))
			placeholders[i] = fmt.Sprintf("$%d", len(args))
		}
		where += " AND " + column + " NOT IN (" + strings.Join(placeholders, ", ") + ")"
	}
	return where, args
}

// planUsageHourlyQuery 在汇总可用时返回拆分方案,否则整个区间都扫明细。
func (db *DB) planUsageHourlyQuery(ctx context.Context, start, end, now time.Time, cuts ...time.Time) usageHourlyPlan {
	plan := planUsageHourly(start, end, now, cuts...)
	if plan.useRollup && !db.usageHourlyRollupUsable(ctx) {
		return usageHourlyPlan{raw: []usageTimeSpan{{start, end}}}
	}
	return plan
}

// sumBigint 包一层 CAST:PostgreSQL 对 BIGINT 求和得到 NUMERIC,直接扫进 int64 不可靠。
func sumBigint(expr string) string {
	return "CAST(COALESCE(SUM(" + expr + "), 0) AS BIGINT)"
}
