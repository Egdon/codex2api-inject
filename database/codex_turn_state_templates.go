package database

import (
	"context"
	"database/sql"
)

const CodexTurnStateRenewalMaxAttempts = 10

// CodexTurnStateTemplate is an upstream-minted opaque value. Never expose Value in admin JSON.
type CodexTurnStateTemplate struct {
	AccountID int64
	Model     string
	Value     string `json:"-"`
	IssuedAt  int64
	Strikes   int
	UpdatedAt int64
}

func (db *DB) ensureCodexTurnStateTemplateSchema(ctx context.Context) error {
	_, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS codex_turn_state_templates (
 account_id BIGINT NOT NULL, model TEXT NOT NULL, value TEXT NOT NULL,
 issued_at BIGINT NOT NULL, strikes INTEGER NOT NULL DEFAULT 0,
 updated_at BIGINT NOT NULL, PRIMARY KEY(account_id,model))`)
	if err != nil {
		return err
	}
	// Keep attempts across restarts without changing existing template rows.
	_, err = db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS codex_turn_state_renewals (
 account_id BIGINT NOT NULL, model TEXT NOT NULL, issued_at BIGINT NOT NULL,
 attempts INTEGER NOT NULL, next_attempt_at BIGINT NOT NULL,
 PRIMARY KEY(account_id,model))`)
	if err != nil {
		return err
	}
	// Store only URL hashes: retries avoid previously attempted exits without
	// copying proxy credentials into renewal history.
	_, err = db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS codex_turn_state_renewal_routes (
 account_id BIGINT NOT NULL, model TEXT NOT NULL, issued_at BIGINT NOT NULL,
 attempt INTEGER NOT NULL, proxy_id BIGINT NOT NULL, proxy_hash TEXT NOT NULL,
 PRIMARY KEY(account_id,model,issued_at,attempt))`)
	if err != nil {
		return err
	}
	return db.ensureCodexTurnStateHistorySchema(ctx)
}

func (db *DB) SaveCodexTurnStateTemplate(ctx context.Context, row CodexTurnStateTemplate) error {
	_, err := db.conn.ExecContext(ctx, `INSERT INTO codex_turn_state_templates
 (account_id,model,value,issued_at,strikes,updated_at) VALUES ($1,$2,$3,$4,$5,$6)
 ON CONFLICT(account_id,model) DO UPDATE SET value=excluded.value,issued_at=excluded.issued_at,
 strikes=excluded.strikes,updated_at=excluded.updated_at
 WHERE excluded.issued_at>=codex_turn_state_templates.issued_at`, row.AccountID, row.Model, row.Value, row.IssuedAt, row.Strikes, row.UpdatedAt)
	return err
}

func (db *DB) GetCodexTurnStateTemplate(ctx context.Context, accountID int64, model string) (CodexTurnStateTemplate, bool, error) {
	var row CodexTurnStateTemplate
	err := db.conn.QueryRowContext(ctx, `SELECT account_id,model,value,issued_at,strikes,updated_at FROM codex_turn_state_templates WHERE account_id=$1 AND model=$2`, accountID, model).Scan(&row.AccountID, &row.Model, &row.Value, &row.IssuedAt, &row.Strikes, &row.UpdatedAt)
	if err == sql.ErrNoRows {
		return row, false, nil
	}
	return row, err == nil, err
}

func (db *DB) ListCodexTurnStateTemplates(ctx context.Context, accountID int64) ([]CodexTurnStateTemplate, error) {
	return db.listCodexTurnStateTemplates(ctx, `SELECT account_id,model,value,issued_at,strikes,updated_at FROM codex_turn_state_templates WHERE account_id=$1 ORDER BY model`, accountID)
}

func (db *DB) ListCodexTurnStateRenewalCandidates(ctx context.Context, now int64) ([]CodexTurnStateTemplate, error) {
	return db.listCodexTurnStateTemplates(ctx, `SELECT t.account_id,t.model,t.value,t.issued_at,t.strikes,t.updated_at
 FROM codex_turn_state_templates t LEFT JOIN codex_turn_state_renewals r ON t.account_id=r.account_id AND t.model=r.model
 WHERE r.account_id IS NULL OR t.issued_at>r.issued_at OR
 (t.issued_at=r.issued_at AND r.attempts<$2 AND r.next_attempt_at<=$1)
 ORDER BY t.issued_at,t.account_id,t.model`, now, CodexTurnStateRenewalMaxAttempts)
}

func (db *DB) listCodexTurnStateTemplates(ctx context.Context, query string, args ...any) ([]CodexTurnStateTemplate, error) {
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []CodexTurnStateTemplate{}
	for rows.Next() {
		var row CodexTurnStateTemplate
		if err := rows.Scan(&row.AccountID, &row.Model, &row.Value, &row.IssuedAt, &row.Strikes, &row.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// ClaimCodexTurnStateRenewal atomically reserves one of ten attempts for this
// upstream issuance. Ordinary re-saves of the same token do not reset the budget.
func (db *DB) ClaimCodexTurnStateRenewal(ctx context.Context, row CodexTurnStateTemplate, now, leaseUntil int64) (int, error) {
	var attempt int
	err := db.conn.QueryRowContext(ctx, `INSERT INTO codex_turn_state_renewals
 (account_id,model,issued_at,attempts,next_attempt_at)
 SELECT $1,$2,$3,1,$5 WHERE EXISTS (SELECT 1 FROM codex_turn_state_templates
 WHERE account_id=$1 AND model=$2 AND issued_at=$3)
 ON CONFLICT(account_id,model) DO UPDATE SET issued_at=excluded.issued_at,
 attempts=CASE WHEN excluded.issued_at>codex_turn_state_renewals.issued_at THEN 1 ELSE codex_turn_state_renewals.attempts+1 END,
 next_attempt_at=excluded.next_attempt_at
 WHERE excluded.issued_at>codex_turn_state_renewals.issued_at OR
 (excluded.issued_at=codex_turn_state_renewals.issued_at AND codex_turn_state_renewals.attempts<$6 AND codex_turn_state_renewals.next_attempt_at<=$4)
 RETURNING attempts`, row.AccountID, row.Model, row.IssuedAt, now, leaseUntil, CodexTurnStateRenewalMaxAttempts).Scan(&attempt)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return attempt, err
}

func (db *DB) FinishCodexTurnStateRenewal(ctx context.Context, row CodexTurnStateTemplate, attempt int, nextAttempt int64) error {
	_, err := db.conn.ExecContext(ctx, `UPDATE codex_turn_state_renewals SET next_attempt_at=$5
 WHERE account_id=$1 AND model=$2 AND issued_at=$3 AND attempts=$4`, row.AccountID, row.Model, row.IssuedAt, attempt, nextAttempt)
	return err
}

func (db *DB) PruneCodexTurnStateRenewals(ctx context.Context) error {
	_, err := db.conn.ExecContext(ctx, `DELETE FROM codex_turn_state_renewals WHERE NOT EXISTS
 (SELECT 1 FROM codex_turn_state_templates t WHERE t.account_id=codex_turn_state_renewals.account_id AND t.model=codex_turn_state_renewals.model)`)
	if err != nil {
		return err
	}
	_, err = db.conn.ExecContext(ctx, `DELETE FROM codex_turn_state_renewal_routes WHERE NOT EXISTS
 (SELECT 1 FROM codex_turn_state_renewals r WHERE r.account_id=codex_turn_state_renewal_routes.account_id
 AND r.model=codex_turn_state_renewal_routes.model AND r.issued_at=codex_turn_state_renewal_routes.issued_at)`)
	return err
}

func (db *DB) DeleteCodexTurnStateTemplate(ctx context.Context, accountID int64, model string) error {
	_, err := db.conn.ExecContext(ctx, `DELETE FROM codex_turn_state_templates WHERE account_id=$1 AND ($2='' OR model=$2)`, accountID, model)
	return err
}

func (db *DB) PruneCodexTurnStateTemplates(ctx context.Context, oldestIssued int64, maxEntries int) error {
	_, err := db.conn.ExecContext(ctx, `DELETE FROM codex_turn_state_templates WHERE issued_at<=$1`, oldestIssued)
	if err != nil {
		return err
	}
	limit := "ALL"
	if db.isSQLite() {
		limit = "-1"
	}
	_, err = db.conn.ExecContext(ctx, `DELETE FROM codex_turn_state_templates WHERE (account_id,model) IN
 (SELECT account_id,model FROM codex_turn_state_templates ORDER BY issued_at DESC,account_id,model LIMIT `+limit+` OFFSET $1)`, maxEntries)
	return err
}

// CodexTurnStateRenewalProxyHashes returns exits already tried for this issuance.
func (db *DB) CodexTurnStateRenewalProxyHashes(ctx context.Context, row CodexTurnStateTemplate) (map[string]bool, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT proxy_hash FROM codex_turn_state_renewal_routes
 WHERE account_id=$1 AND model=$2 AND issued_at=$3 ORDER BY attempt`, row.AccountID, row.Model, row.IssuedAt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	used := make(map[string]bool)
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		used[hash] = true
	}
	return used, rows.Err()
}

func (db *DB) RecordCodexTurnStateRenewalProxy(ctx context.Context, row CodexTurnStateTemplate, attempt int, proxyID int64, hash string) error {
	_, err := db.conn.ExecContext(ctx, `INSERT INTO codex_turn_state_renewal_routes
 (account_id,model,issued_at,attempt,proxy_id,proxy_hash) VALUES ($1,$2,$3,$4,$5,$6)
 ON CONFLICT(account_id,model,issued_at,attempt) DO NOTHING`, row.AccountID, row.Model, row.IssuedAt, attempt, proxyID, hash)
	return err
}
