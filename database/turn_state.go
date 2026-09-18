package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TurnStateTicket is one harvested X-Codex-Turn-State for (account, model).
type TurnStateTicket struct {
	AccountID      int64
	Model          string
	Token          string
	IssuedUnix     int64
	Length         int
	Blocks         int
	ConfirmWarning bool
	Exhausted      bool
	Attempts       int
	LastError      string
	LastHarvestAt  time.Time
	CooldownUntil  int64
	UpdatedAt      time.Time
}

func (db *DB) ensureTurnStateSchema(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return errors.New("database unavailable")
	}
	if db.isSQLite() {
		if err := db.ensureSQLiteColumn(ctx, "system_settings", "turn_state_config", "TEXT DEFAULT '{}'"); err != nil {
			return err
		}
	} else {
		if _, err := db.conn.ExecContext(ctx, `ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS turn_state_config TEXT DEFAULT '{}'`); err != nil {
			return fmt.Errorf("add system_settings.turn_state_config: %w", err)
		}
	}

	timeType := "TIMESTAMPTZ"
	if db.isSQLite() {
		timeType = "TIMESTAMP"
	}
	_, err := db.conn.ExecContext(ctx, fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS codex_turn_state_tickets (
				account_id BIGINT NOT NULL,
				model TEXT NOT NULL,
				token TEXT NOT NULL DEFAULT '',
				issued_unix BIGINT NOT NULL DEFAULT 0,
				length INT NOT NULL DEFAULT 0,
				blocks INT NOT NULL DEFAULT 0,
				confirm_warning INT NOT NULL DEFAULT 0,
				exhausted INT NOT NULL DEFAULT 0,
				attempts INT NOT NULL DEFAULT 0,
				last_error TEXT NOT NULL DEFAULT '',
				last_harvest_at %s,
				cooldown_until BIGINT NOT NULL DEFAULT 0,
				updated_at %s NOT NULL,
				PRIMARY KEY (account_id, model)
			)`, timeType, timeType))
	if err != nil {
		return fmt.Errorf("create codex_turn_state_tickets: %w", err)
	}
	if db.isSQLite() {
		if err := db.ensureSQLiteColumn(ctx, "codex_turn_state_tickets", "exhausted", "INT NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		if err := db.ensureSQLiteColumn(ctx, "codex_turn_state_tickets", "cooldown_until", "BIGINT NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	} else {
		if _, err := db.conn.ExecContext(ctx, `ALTER TABLE codex_turn_state_tickets ADD COLUMN IF NOT EXISTS exhausted INT NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add exhausted: %w", err)
		}
		if _, err := db.conn.ExecContext(ctx, `ALTER TABLE codex_turn_state_tickets ADD COLUMN IF NOT EXISTS cooldown_until BIGINT NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add cooldown_until: %w", err)
		}
	}
	return db.ensureAstraPolicySchema(ctx)
}

func (db *DB) LoadTurnStateConfig(ctx context.Context) (string, error) {
	var raw string
	err := db.conn.QueryRowContext(ctx, `
		SELECT COALESCE(turn_state_config, '{}')
		FROM system_settings
		WHERE id = 1
	`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "{}", nil
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(raw) == "" {
		return "{}", nil
	}
	return raw, nil
}

func (db *DB) SaveTurnStateConfig(ctx context.Context, raw string) error {
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		if _, err := db.conn.ExecContext(ctx, `
			INSERT INTO system_settings (id) VALUES (1)
			ON CONFLICT (id) DO NOTHING
		`); err != nil {
			return err
		}
		_, err := db.conn.ExecContext(ctx, `
			UPDATE system_settings
			SET turn_state_config = $1
			WHERE id = 1
		`, raw)
		return err
	})
}

func (db *DB) ListTurnStateTickets(ctx context.Context) ([]TurnStateTicket, error) {
	rows, err := db.conn.QueryContext(ctx, `
			SELECT account_id, model, token, issued_unix, length, blocks, confirm_warning,
			       exhausted, attempts, last_error, last_harvest_at, cooldown_until, updated_at
			FROM codex_turn_state_tickets
		`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TurnStateTicket, 0, 32)
	for rows.Next() {
		var t TurnStateTicket
		var warning, exhausted int
		var harvest sql.NullTime
		if err := rows.Scan(&t.AccountID, &t.Model, &t.Token, &t.IssuedUnix, &t.Length, &t.Blocks,
			&warning, &exhausted, &t.Attempts, &t.LastError, &harvest, &t.CooldownUntil, &t.UpdatedAt); err != nil {
			return nil, err
		}
		t.ConfirmWarning = warning != 0
		t.Exhausted = exhausted != 0
		if harvest.Valid {
			t.LastHarvestAt = harvest.Time
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (db *DB) UpsertTurnStateTicket(ctx context.Context, t TurnStateTicket) error {
	t.Model = strings.TrimSpace(t.Model)
	if t.AccountID <= 0 || t.Model == "" {
		return fmt.Errorf("invalid ticket key")
	}
	now := time.Now().UTC()
	warning := 0
	if t.ConfirmWarning {
		warning = 1
	}
	exhausted := 0
	if t.Exhausted {
		exhausted = 1
	}
	var harvest any
	if !t.LastHarvestAt.IsZero() {
		harvest = db.timeArg(t.LastHarvestAt.UTC())
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, `
				INSERT INTO codex_turn_state_tickets (
					account_id, model, token, issued_unix, length, blocks, confirm_warning,
					exhausted, attempts, last_error, last_harvest_at, cooldown_until, updated_at
				) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
				ON CONFLICT (account_id, model) DO UPDATE SET
					token = EXCLUDED.token,
					issued_unix = EXCLUDED.issued_unix,
					length = EXCLUDED.length,
					blocks = EXCLUDED.blocks,
					confirm_warning = EXCLUDED.confirm_warning,
					exhausted = EXCLUDED.exhausted,
					attempts = EXCLUDED.attempts,
					last_error = EXCLUDED.last_error,
					last_harvest_at = EXCLUDED.last_harvest_at,
					cooldown_until = EXCLUDED.cooldown_until,
					updated_at = EXCLUDED.updated_at
			`, t.AccountID, t.Model, t.Token, t.IssuedUnix, t.Length, t.Blocks, warning,
			exhausted, t.Attempts, strings.TrimSpace(t.LastError), harvest, t.CooldownUntil, db.timeArg(now))
		return err
	})
}

func (db *DB) DeleteTurnStateTicket(ctx context.Context, accountID int64, model string) error {
	model = strings.TrimSpace(model)
	if accountID <= 0 || model == "" {
		return fmt.Errorf("invalid ticket key")
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, `
			DELETE FROM codex_turn_state_tickets WHERE account_id = $1 AND model = $2
		`, accountID, model)
		return err
	})
}
