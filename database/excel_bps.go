package database

import (
	"context"
	"database/sql"
	"encoding/json"
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

// migrateLegacyExcelBPS runs before schema defaults can obscure provenance.
// The marker and metadata conversion commit together, so a failed schema upgrade
// can restart without mistaking freshly added upstream columns for prior intent.
func (db *DB) migrateLegacyExcelBPS(ctx context.Context) error {
	if err := db.ensureDataMigrationsTable(ctx); err != nil {
		return err
	}
	return db.runDataMigrationOnce(ctx, dataMigrationExcelBPSTriStateV1, func(ctx context.Context, tx *sql.Tx) error {
		columnsQuery := `SELECT name FROM pragma_table_info('system_settings')`
		if !db.isSQLite() {
			columnsQuery = `SELECT column_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='system_settings'`
		}
		rows, err := tx.QueryContext(ctx, columnsQuery)
		if err != nil {
			return err
		}
		var legacy, upstream bool
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			legacy = legacy || name == "openai_excel_bps_enabled"
			upstream = upstream || name == "codex_basispoints_enabled"
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !legacy || upstream {
			return err // fresh/pure upstream installs and explicit upstream settings
		}
		var master sql.NullBool
		query := `SELECT openai_excel_bps_enabled FROM system_settings WHERE id=1`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		if err := tx.QueryRowContext(ctx, query).Scan(&master); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		masterEnabled := !master.Valid || master.Bool
		query = `SELECT id,credentials FROM accounts ORDER BY id`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		rows, err = tx.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		type change struct {
			id      int64
			enabled bool
		}
		var changes []change
		for rows.Next() {
			var id int64
			var raw []byte
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return err
			}
			// Never decrypt or re-encrypt tokens: inspect only configuration keys,
			// then patch those keys in SQL while preserving all other JSON values.
			var credentials map[string]any
			if len(strings.TrimSpace(string(raw))) == 0 {
				continue // NULL/empty legacy credentials have no BPS intent
			}
			if err := json.Unmarshal(raw, &credentials); err != nil {
				rows.Close()
				return err
			}
			if _, exists := credentials["openai_excel_bps_opt_out"]; exists {
				continue
			}
			if _, exists := credentials["openai_excel_bps"]; !exists {
				continue // absent remains inherit, not explicit off
			}
			enabled := masterEnabled && (&AccountRow{Credentials: credentials}).GetCredentialBool("openai_excel_bps")
			changes = append(changes, change{id, enabled})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, c := range changes {
			query = `UPDATE accounts SET credentials=json_set(credentials,'$.openai_excel_bps',json($1),'$.openai_excel_bps_opt_out',json($2)) WHERE id=$3`
			if !db.isSQLite() {
				query = `UPDATE accounts SET credentials=jsonb_set(jsonb_set(credentials,'{openai_excel_bps}',$1::jsonb),'{openai_excel_bps_opt_out}',$2::jsonb) WHERE id=$3`
			}
			on, off := "false", "true"
			if c.enabled {
				on, off = "true", "false"
			}
			if _, err := tx.ExecContext(ctx, query, on, off, c.id); err != nil {
				return err
			}
		}
		return nil
	})
}

// GetOpenAIExcelBPSEnabled reads the legacy fork master for migration tooling.
// Runtime configuration exclusively uses SystemSettings.CodexBasispointsEnabled.
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
	old := &AccountRow{Credentials: before}
	next := &AccountRow{Credentials: after}
	if old.GetCredentialBool("openai_excel_bps") == next.GetCredentialBool("openai_excel_bps") &&
		old.GetCredentialBool("openai_excel_bps_opt_out") == next.GetCredentialBool("openai_excel_bps_opt_out") {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO codex_astra_policy(account_id,bps_revision) VALUES($1,1)
		ON CONFLICT(account_id) DO UPDATE SET bps_revision=codex_astra_policy.bps_revision+1`, id)
	return err
}

// GetCodexBasispointsState returns the persisted upstream global and its fence.
// Used only by background harvest admission/publication, never business routing.
func (db *DB) GetCodexBasispointsState(ctx context.Context) (enabled bool, revision int64, err error) {
	if db == nil || db.conn == nil {
		return false, 0, errors.New("database is not initialized")
	}
	err = db.conn.QueryRowContext(ctx, `SELECT COALESCE(codex_basispoints_enabled,false),codex_basispoints_revision FROM system_settings WHERE id=1`).Scan(&enabled, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

// Lock settings before accounts, matching policy publication and settings writes.
// Materialize the default row so even the first settings INSERT is serialized.
func (db *DB) lockHarvestSettings(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO system_settings(id) VALUES(1) ON CONFLICT(id) DO NOTHING`); err != nil {
		return err
	}
	q := `SELECT id FROM system_settings WHERE id=1`
	if !db.isSQLite() {
		q += ` FOR SHARE`
	}
	var id int
	return tx.QueryRowContext(ctx, q).Scan(&id)
}

// harvestBPSState captures configured intent and both revisions in one snapshot.
// Writers first lock settings, then account; health/model pauses are irrelevant.
func harvestBPSState(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64) (revision, globalRevision int64, enabled bool, err error) {
	var raw any
	var global bool
	err = q.QueryRowContext(ctx, `SELECT a.credentials,COALESCE(p.bps_revision,0),
		COALESCE(s.codex_basispoints_enabled,false),COALESCE(s.codex_basispoints_revision,0) FROM accounts a
		LEFT JOIN codex_astra_policy p ON p.account_id=a.id LEFT JOIN system_settings s ON s.id=1 WHERE a.id=$1`, id).Scan(&raw, &revision, &global, &globalRevision)
	if err == nil {
		account := &AccountRow{Credentials: decodeCredentials(raw)}
		enabled = account.GetCredentialBool("openai_excel_bps") || (!account.GetCredentialBool("openai_excel_bps_opt_out") && global)
	}
	return
}

// HarvestBPSCurrent is a pre-probe check. Publication repeats it under transaction
// locks; neither a same-value write nor token refresh invalidates admitted work.
func (db *DB) HarvestBPSCurrent(ctx context.Context, id, expectedRevision, expectedGlobalRevision int64) (bool, error) {
	revision, globalRevision, enabled, err := harvestBPSState(ctx, db.conn, id)
	return err == nil && !enabled && revision == expectedRevision && globalRevision == expectedGlobalRevision, err
}
