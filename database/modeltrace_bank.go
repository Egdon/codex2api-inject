package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ModelTraceBankOverride is an operator-installed ModelTrace bank. It lives in
// its own table so the ~1 MB blob never rides along with system_settings reads.
type ModelTraceBankOverride struct {
	Revision    string
	BuiltAt     string
	BankJSON    []byte
	InstalledAt time.Time
}

var (
	modelTraceBankInitMu sync.Mutex
	modelTraceBankReady  = make(map[*DB]bool)
)

func (db *DB) ensureModelTraceBankSchema(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return fmt.Errorf("数据库不可用")
	}
	modelTraceBankInitMu.Lock()
	defer modelTraceBankInitMu.Unlock()
	if modelTraceBankReady[db] {
		return nil
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS modeltrace_bank_override (
		singleton_id INTEGER PRIMARY KEY CHECK (singleton_id = 1),
		revision VARCHAR(64) NOT NULL,
		built_at VARCHAR(64) NOT NULL DEFAULT '',
		bank_json TEXT NOT NULL,
		installed_at TIMESTAMP NOT NULL
	)`); err != nil {
		return err
	}
	modelTraceBankReady[db] = true
	return nil
}

// GetModelTraceBankRevision returns the installed override revision without
// loading the bank body; empty means no override is installed.
func (db *DB) GetModelTraceBankRevision(ctx context.Context) (string, error) {
	if err := db.ensureModelTraceBankSchema(ctx); err != nil {
		return "", err
	}
	var revision string
	err := db.conn.QueryRowContext(ctx, `SELECT revision FROM modeltrace_bank_override WHERE singleton_id = 1`).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return revision, err
}

// GetModelTraceBankOverride returns the installed override, or nil when none.
func (db *DB) GetModelTraceBankOverride(ctx context.Context) (*ModelTraceBankOverride, error) {
	if err := db.ensureModelTraceBankSchema(ctx); err != nil {
		return nil, err
	}
	var override ModelTraceBankOverride
	var body string
	err := db.conn.QueryRowContext(ctx, `SELECT revision, built_at, bank_json, installed_at
		FROM modeltrace_bank_override WHERE singleton_id = 1`).Scan(
		&override.Revision, &override.BuiltAt, &body, &override.InstalledAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	override.BankJSON = []byte(body)
	return &override, nil
}

func (db *DB) SaveModelTraceBankOverride(ctx context.Context, override ModelTraceBankOverride) error {
	if err := db.ensureModelTraceBankSchema(ctx); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx, `INSERT INTO modeltrace_bank_override (singleton_id, revision, built_at, bank_json, installed_at)
		VALUES (1, $1, $2, $3, $4)
		ON CONFLICT (singleton_id) DO UPDATE SET
			revision = EXCLUDED.revision,
			built_at = EXCLUDED.built_at,
			bank_json = EXCLUDED.bank_json,
			installed_at = EXCLUDED.installed_at`,
		override.Revision, override.BuiltAt, string(override.BankJSON), override.InstalledAt.UTC())
	return err
}

func (db *DB) DeleteModelTraceBankOverride(ctx context.Context) error {
	if err := db.ensureModelTraceBankSchema(ctx); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx, `DELETE FROM modeltrace_bank_override WHERE singleton_id = 1`)
	return err
}
