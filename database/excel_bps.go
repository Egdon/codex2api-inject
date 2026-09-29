package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// NormalizeUpstreamSource describes the observed request path, never today's
// account configuration. Empty/unknown remains unknown; unrecognized sources
// are other, not native Codex.
func NormalizeUpstreamSource(source string) string {
	switch source = strings.ToLower(strings.TrimSpace(source)); source {
	case "", "unknown":
		return ""
	case "bps", "codex", "other":
		return source
	default:
		return "other"
	}
}

func nullableUpstreamSource(source string) any {
	if source = NormalizeUpstreamSource(source); source != "" {
		return source
	}
	return nil
}

func (db *DB) GetOpenAIExcelBPSEnabled(ctx context.Context) (bool, error) {
	if db == nil || db.conn == nil {
		return false, errors.New("database is not initialized")
	}
	var enabled sql.NullBool
	err := db.conn.QueryRowContext(ctx, `SELECT openai_excel_bps_enabled FROM system_settings WHERE id=1`).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !enabled.Valid || enabled.Bool, nil
}

func (db *DB) SaveOpenAIExcelBPSEnabled(ctx context.Context, enabled bool) error {
	if db == nil || db.conn == nil {
		return errors.New("database is not initialized")
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings(id,openai_excel_bps_enabled) VALUES(1,$1)
			ON CONFLICT(id) DO UPDATE SET openai_excel_bps_enabled=EXCLUDED.openai_excel_bps_enabled`, enabled)
		return err
	})
}

// markBPSPolicyTransition is called while the account row is locked, before
// changing credentials. Token refresh and same-value writes do not invalidate
// work. No group/priority ownership or audit history is rewritten.
func (db *DB) markBPSPolicyTransition(ctx context.Context, tx *sql.Tx, id int64, before, after map[string]interface{}) error {
	old := (&AccountRow{Credentials: before}).GetCredentialBool("openai_excel_bps")
	next := (&AccountRow{Credentials: after}).GetCredentialBool("openai_excel_bps")
	if old == next {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO codex_astra_policy(account_id,bps_revision) VALUES($1,1)
		ON CONFLICT(account_id) DO UPDATE SET bps_revision=codex_astra_policy.bps_revision+1`, id)
	return err
}

// harvestBPSState uses one snapshot; callers that write must first lock the
// account. Missing per-account flags are deliberately false, unlike the master.
func harvestBPSState(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64) (revision int64, enabled bool, err error) {
	var raw any
	err = q.QueryRowContext(ctx, `SELECT a.credentials,COALESCE(p.bps_revision,0) FROM accounts a
		LEFT JOIN codex_astra_policy p ON p.account_id=a.id WHERE a.id=$1`, id).Scan(&raw, &revision)
	if err == nil {
		enabled = (&AccountRow{Credentials: decodeCredentials(raw)}).GetCredentialBool("openai_excel_bps")
	}
	return
}

// HarvestBPSCurrent is a cheap pre-probe check. Publication repeats it under a
// transaction lock; a read here alone is not sufficient to authorize a write.
func (db *DB) HarvestBPSCurrent(ctx context.Context, id, expectedRevision int64) (bool, error) {
	revision, enabled, err := harvestBPSState(ctx, db.conn, id)
	return err == nil && !enabled && revision == expectedRevision, err
}
