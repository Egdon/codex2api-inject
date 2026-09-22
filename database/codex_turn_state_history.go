package database

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Renewal history stores immutable display snapshots, never tokens or proxy credentials.
type CodexTurnStateRenewalRecord struct {
	ID            int64  `json:"id"`
	AccountID     int64  `json:"account_id"`
	AccountName   string `json:"account_name"`
	PlanType      string `json:"plan_type"`
	Model         string `json:"model"`
	Attempt       int    `json:"attempt"`
	MaxAttempts   int    `json:"max_attempts"`
	ProxyID       int64  `json:"proxy_id"`
	ProxyName     string `json:"proxy_name"`
	ProxyURL      string `json:"proxy_url"`
	ProxyIP       string `json:"proxy_ip"`
	Route         string `json:"route"`
	StartedAt     int64  `json:"started_at"`
	FinishedAt    int64  `json:"finished_at"`
	DurationMS    int64  `json:"duration_ms"`
	Status        string `json:"status"`
	Reason        string `json:"reason"`
	ExpiresBefore int64  `json:"expires_before"`
	ExpiresAfter  int64  `json:"expires_after"`
}

type CodexTurnStateHistoryFilter struct {
	AccountID                     int64
	Plan, Model, Status, ProxyURL string
}

type CodexTurnStateHistoryProxyFacet struct {
	URL  string `json:"url"`
	Name string `json:"name"`
}

type CodexTurnStateHistoryFacets struct {
	Plans    []string                          `json:"plans"`
	Models   []string                          `json:"models"`
	Accounts []QualityTestAccountFacet         `json:"accounts"`
	Proxies  []CodexTurnStateHistoryProxyFacet `json:"proxies"`
}

type CodexTurnStateHistoryPage struct {
	Records []CodexTurnStateRenewalRecord `json:"records"`
	Total   int                           `json:"total"`
	Facets  CodexTurnStateHistoryFacets   `json:"facets"`
}

func (db *DB) ensureCodexTurnStateHistorySchema(ctx context.Context) error {
	idType := "BIGSERIAL PRIMARY KEY"
	if db.isSQLite() {
		idType = "INTEGER PRIMARY KEY AUTOINCREMENT"
	}
	_, err := db.conn.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS codex_turn_state_renewal_history (
 id %s, account_id BIGINT NOT NULL, account_name TEXT NOT NULL, plan_type TEXT NOT NULL,
 model TEXT NOT NULL, attempt INTEGER NOT NULL, max_attempts INTEGER NOT NULL,
 proxy_id BIGINT NOT NULL, proxy_name TEXT NOT NULL, proxy_url TEXT NOT NULL, proxy_ip TEXT NOT NULL, route TEXT NOT NULL,
 started_at BIGINT NOT NULL, finished_at BIGINT NOT NULL DEFAULT 0, duration_ms BIGINT NOT NULL DEFAULT 0,
 status TEXT NOT NULL DEFAULT 'running', reason TEXT NOT NULL DEFAULT '', expires_before BIGINT NOT NULL, expires_after BIGINT NOT NULL DEFAULT 0)`, idType))
	if err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS idx_turn_state_history_account ON codex_turn_state_renewal_history(account_id,id)`,
		`CREATE INDEX IF NOT EXISTS idx_turn_state_history_status ON codex_turn_state_renewal_history(status,started_at)`,
	} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func renewalHistoryProxyURL(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "***"
	}
	return parsed.Scheme + "://" + parsed.Host
}

func (db *DB) StartCodexTurnStateHistory(ctx context.Context, record CodexTurnStateRenewalRecord) (int64, error) {
	record.ProxyURL = renewalHistoryProxyURL(record.ProxyURL)
	var id int64
	err := db.conn.QueryRowContext(ctx, `INSERT INTO codex_turn_state_renewal_history
 (account_id,account_name,plan_type,model,attempt,max_attempts,proxy_id,proxy_name,proxy_url,proxy_ip,route,started_at,expires_before)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`, record.AccountID, record.AccountName, record.PlanType, record.Model, record.Attempt, record.MaxAttempts, record.ProxyID, record.ProxyName, record.ProxyURL, record.ProxyIP, record.Route, record.StartedAt, record.ExpiresBefore).Scan(&id)
	return id, err
}

func (db *DB) FinishCodexTurnStateHistory(ctx context.Context, id int64, status, reason string, finishedAt, durationMS, expiresAfter int64) error {
	_, err := db.conn.ExecContext(ctx, `UPDATE codex_turn_state_renewal_history
 SET status=$2,reason=$3,finished_at=$4,duration_ms=$5,expires_after=$6 WHERE id=$1 AND status='running'`, id, status, reason, finishedAt, durationMS, expiresAfter)
	return err
}

// A killed process cannot finalize its record. Never leave it permanently running
// or infer success from a later template; the request deadline was 60 seconds.
func (db *DB) RecoverCodexTurnStateHistory(ctx context.Context, now int64) error {
	_, err := db.conn.ExecContext(ctx, `UPDATE codex_turn_state_renewal_history SET status='interrupted',reason='worker_interrupted',finished_at=$1
 WHERE status='running' AND started_at<$2`, now, now-120000)
	return err
}

func (f CodexTurnStateHistoryFilter) where() (string, []any) {
	clauses, args := []string{}, []any{}
	add := func(column string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf("%s=$%d", column, len(args)))
	}
	if f.AccountID > 0 {
		add("account_id", f.AccountID)
	}
	if f.Plan != "" {
		add("plan_type", f.Plan)
	}
	if f.Model != "" {
		add("model", f.Model)
	}
	if f.Status != "" {
		add("status", f.Status)
	}
	if f.ProxyURL != "" {
		add("proxy_url", f.ProxyURL)
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func (db *DB) ListCodexTurnStateHistory(ctx context.Context, page, size int, filter CodexTurnStateHistoryFilter) (CodexTurnStateHistoryPage, error) {
	result := CodexTurnStateHistoryPage{Records: []CodexTurnStateRenewalRecord{}, Facets: CodexTurnStateHistoryFacets{Plans: []string{}, Models: []string{}, Accounts: []QualityTestAccountFacet{}, Proxies: []CodexTurnStateHistoryProxyFacet{}}}
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 50 {
		size = 20
	}
	where, args := filter.where()
	if err := db.conn.QueryRowContext(ctx, `SELECT count(*) FROM codex_turn_state_renewal_history`+where, args...).Scan(&result.Total); err != nil {
		return result, err
	}
	query := `SELECT id,account_id,account_name,plan_type,model,attempt,max_attempts,proxy_id,proxy_name,proxy_url,proxy_ip,route,started_at,finished_at,duration_ms,status,reason,expires_before,expires_after FROM codex_turn_state_renewal_history` + where + fmt.Sprintf(` ORDER BY id DESC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
	args = append(args, size, (page-1)*size)
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var r CodexTurnStateRenewalRecord
		if err := rows.Scan(&r.ID, &r.AccountID, &r.AccountName, &r.PlanType, &r.Model, &r.Attempt, &r.MaxAttempts, &r.ProxyID, &r.ProxyName, &r.ProxyURL, &r.ProxyIP, &r.Route, &r.StartedAt, &r.FinishedAt, &r.DurationMS, &r.Status, &r.Reason, &r.ExpiresBefore, &r.ExpiresAfter); err != nil {
			return result, err
		}
		result.Records = append(result.Records, r)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	rows.Close()
	for _, column := range []string{"plan_type", "model"} {
		values, err := db.conn.QueryContext(ctx, `SELECT DISTINCT `+column+` FROM codex_turn_state_renewal_history WHERE `+column+`<>'' ORDER BY `+column)
		if err != nil {
			return result, err
		}
		for values.Next() {
			var value string
			if err := values.Scan(&value); err != nil {
				values.Close()
				return result, err
			}
			if column == "plan_type" {
				result.Facets.Plans = append(result.Facets.Plans, value)
			} else {
				result.Facets.Models = append(result.Facets.Models, value)
			}
		}
		err = values.Err()
		values.Close()
		if err != nil {
			return result, err
		}
	}
	accounts, err := db.conn.QueryContext(ctx, `SELECT account_id,MAX(account_name) FROM codex_turn_state_renewal_history GROUP BY account_id ORDER BY account_id`)
	if err != nil {
		return result, err
	}
	for accounts.Next() {
		var item QualityTestAccountFacet
		if err := accounts.Scan(&item.ID, &item.Name); err != nil {
			accounts.Close()
			return result, err
		}
		result.Facets.Accounts = append(result.Facets.Accounts, item)
	}
	err = accounts.Err()
	accounts.Close()
	if err != nil {
		return result, err
	}
	proxies, err := db.conn.QueryContext(ctx, `SELECT proxy_url,MAX(proxy_name) FROM codex_turn_state_renewal_history WHERE proxy_url<>'' GROUP BY proxy_url ORDER BY proxy_url`)
	if err != nil {
		return result, err
	}
	defer proxies.Close()
	for proxies.Next() {
		var item CodexTurnStateHistoryProxyFacet
		if err := proxies.Scan(&item.URL, &item.Name); err != nil {
			return result, err
		}
		result.Facets.Proxies = append(result.Facets.Proxies, item)
	}
	return result, proxies.Err()
}
