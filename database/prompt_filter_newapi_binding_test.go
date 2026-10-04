package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	promptFilterBindingDDLDriverOnce sync.Once
	promptFilterBindingDDLQueryMu    sync.Mutex
	promptFilterBindingDDLQueries    []string
	legacySecretMigrationDriverOnce  sync.Once
	legacySecretMigrationQueryMu     sync.Mutex
	legacySecretMigrationQueries     []string
)

var errStopLegacySecretMigrationCapture = errors.New("stop legacy secret migration capture")

type promptFilterBindingDDLDriver struct{}
type promptFilterBindingDDLConn struct{}
type legacySecretMigrationCaptureDriver struct{}
type legacySecretMigrationCaptureConn struct{}

func (promptFilterBindingDDLDriver) Open(string) (driver.Conn, error) {
	return promptFilterBindingDDLConn{}, nil
}
func (promptFilterBindingDDLConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("not supported")
}
func (promptFilterBindingDDLConn) Close() error              { return nil }
func (promptFilterBindingDDLConn) Begin() (driver.Tx, error) { return nil, errors.New("not supported") }
func (promptFilterBindingDDLConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	promptFilterBindingDDLQueryMu.Lock()
	promptFilterBindingDDLQueries = append(promptFilterBindingDDLQueries, query)
	promptFilterBindingDDLQueryMu.Unlock()
	return driver.RowsAffected(0), nil
}

func (legacySecretMigrationCaptureDriver) Open(string) (driver.Conn, error) {
	return legacySecretMigrationCaptureConn{}, nil
}
func (legacySecretMigrationCaptureConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("not supported")
}
func (legacySecretMigrationCaptureConn) Close() error { return nil }
func (legacySecretMigrationCaptureConn) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected pre-DDL transaction")
}
func (legacySecretMigrationCaptureConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	legacySecretMigrationQueryMu.Lock()
	legacySecretMigrationQueries = append(legacySecretMigrationQueries, query)
	legacySecretMigrationQueryMu.Unlock()
	if strings.Contains(query, "CREATE TABLE IF NOT EXISTS accounts") {
		return nil, errStopLegacySecretMigrationCapture
	}
	return driver.RowsAffected(0), nil
}

func TestPromptFilterNewAPIBindingCRUDAndSecretRotationSQLite(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "bindings.sqlite"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	apiKeyID, err := db.InsertAPIKey(ctx, "gateway-a", "sk-gateway-a-binding-test")
	if err != nil {
		t.Fatalf("InsertAPIKey: %v", err)
	}
	otherAPIKeyID, err := db.InsertAPIKey(ctx, "gateway-b", "sk-gateway-b-binding-test")
	if err != nil {
		t.Fatalf("InsertAPIKey other: %v", err)
	}
	binding := &PromptFilterNewAPIBinding{
		APIKeyID: apiKeyID, PlatformCode: "gateway-a", PlatformName: "示例平台 NewAPI",
		Secret: "01234567890123456789012345678901", Enabled: true, RequireSignedIdentity: true,
		PromptFilterScope: PromptFilterScopeLocalOnly,
		PolicyMode:        PromptFilterPolicyModeEnforce, PolicyProfile: PromptFilterPolicyProfileBalanced,
	}
	if err := db.CreatePromptFilterNewAPIBinding(ctx, binding); err != nil {
		t.Fatalf("Create binding: %v", err)
	}
	got, err := db.GetPromptFilterNewAPIBinding(ctx, apiKeyID)
	if err != nil {
		t.Fatalf("Get binding: %v", err)
	}
	if got.PlatformCode != "gateway-a" || got.Secret != binding.Secret || !got.Enabled || !got.RequireSignedIdentity || got.PromptFilterScope != PromptFilterScopeLocalOnly {
		t.Fatalf("binding = %#v", got)
	}
	if got.PolicyMode != PromptFilterPolicyModeInherit || got.PolicyProfile != PromptFilterPolicyProfileInherit {
		t.Fatalf("create retained retired policy override: %#v", got)
	}
	invalid := *binding
	invalid.APIKeyID = otherAPIKeyID
	invalid.PlatformCode = strings.Repeat("a", 33)
	if err := db.CreatePromptFilterNewAPIBinding(ctx, &invalid); err == nil {
		t.Fatal("overlong platform_code was accepted by database boundary")
	}

	duplicate := *binding
	duplicate.APIKeyID = otherAPIKeyID
	duplicate.Secret = "abcdefghijklmnopqrstuvwxyz123456"
	if err := db.CreatePromptFilterNewAPIBinding(ctx, &duplicate); !errors.Is(err, ErrPromptFilterNewAPIBindingConflict) {
		t.Fatalf("duplicate platform error = %v, want conflict", err)
	}

	got.PlatformCode = "gateway-a-prod"
	got.PlatformName = "示例平台生产站"
	got.PolicyMode = PromptFilterPolicyModeWarn
	got.PolicyProfile = PromptFilterPolicyProfileStrict
	got.PromptFilterScope = PromptFilterScopeOff
	got.Enabled = false
	if err := db.UpdatePromptFilterNewAPIBinding(ctx, got); err != nil {
		t.Fatalf("Update binding: %v", err)
	}
	updated, err := db.GetPromptFilterNewAPIBinding(ctx, apiKeyID)
	if err != nil {
		t.Fatalf("Get updated binding: %v", err)
	}
	if updated.PolicyMode != PromptFilterPolicyModeInherit || updated.PolicyProfile != PromptFilterPolicyProfileInherit {
		t.Fatalf("update retained retired policy override: %#v", updated)
	}
	if updated.PromptFilterScope != PromptFilterScopeOff {
		t.Fatalf("update lost prompt filter scope: %#v", updated)
	}

	newSecret := "abcdefghijklmnopqrstuvwxyzABCDEF"
	previousExpiresAt := time.Now().UTC().Add(time.Hour)
	if err := db.ReplacePromptFilterNewAPIBindingSecretAt(ctx, apiKeyID, newSecret, &previousExpiresAt); err != nil {
		t.Fatalf("Replace secret: %v", err)
	}
	rotated, err := db.GetPromptFilterNewAPIBinding(ctx, apiKeyID)
	if err != nil {
		t.Fatalf("Get rotated binding: %v", err)
	}
	if rotated.Secret != newSecret || rotated.PreviousSecret != binding.Secret || rotated.PreviousSecretExpiresAt == nil || !rotated.PreviousSecretExpiresAt.Equal(previousExpiresAt) {
		t.Fatalf("rotated binding = %#v", rotated)
	}
	if err := db.ReplacePromptFilterNewAPIBindingSecret(ctx, apiKeyID, binding.Secret, 0); err != nil {
		t.Fatalf("Replace secret without grace: %v", err)
	}
	rotated, err = db.GetPromptFilterNewAPIBinding(ctx, apiKeyID)
	if err != nil {
		t.Fatalf("Get no-grace binding: %v", err)
	}
	if rotated.PreviousSecret != "" || rotated.PreviousSecretExpiresAt != nil {
		t.Fatalf("previous secret should be cleared: %#v", rotated)
	}
	bindings, err := db.ListPromptFilterNewAPIBindings(ctx)
	if err != nil || len(bindings) != 1 {
		t.Fatalf("List bindings len=%d err=%v", len(bindings), err)
	}
	if err := db.DeletePromptFilterNewAPIBinding(ctx, apiKeyID); err != nil {
		t.Fatalf("Delete binding: %v", err)
	}
	if _, err := db.GetPromptFilterNewAPIBinding(ctx, apiKeyID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Get deleted error = %v, want sql.ErrNoRows", err)
	}
}

func TestNormalizePromptFilterPlatformCode(t *testing.T) {
	valid32 := "A" + strings.Repeat("b", 31)
	if got, ok := NormalizePromptFilterPlatformCode(valid32); !ok || got != strings.ToLower(valid32) {
		t.Fatalf("32-character platform code = %q ok=%v", got, ok)
	}
	for _, value := range []string{
		strings.Repeat("a", 33),
		"_gateway-a",
		"-gateway-a",
		"gateway-a.prod",
		"gateway-a/prod",
		"gateway-a:prod",
		"",
	} {
		if got, ok := NormalizePromptFilterPlatformCode(value); ok {
			t.Fatalf("invalid platform code %q normalized to %q", value, got)
		}
	}
}

func TestPromptFilterNewAPIBindingPostgresMigrationDDL(t *testing.T) {
	promptFilterBindingDDLDriverOnce.Do(func() {
		sql.Register("prompt-filter-binding-ddl-capture", promptFilterBindingDDLDriver{})
	})
	conn, err := sql.Open("prompt-filter-binding-ddl-capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	db := &DB{conn: conn, driver: "postgres"}
	promptFilterBindingDDLQueryMu.Lock()
	promptFilterBindingDDLQueries = nil
	promptFilterBindingDDLQueryMu.Unlock()
	if err := db.ensurePromptFilterNewAPIBindingsTable(context.Background()); err != nil {
		t.Fatalf("ensure postgres table: %v", err)
	}
	promptFilterBindingDDLQueryMu.Lock()
	queries := append([]string(nil), promptFilterBindingDDLQueries...)
	promptFilterBindingDDLQueryMu.Unlock()
	if len(queries) != 3 {
		t.Fatalf("ensure executed %d statements, want DDL, scope migration, and override retirement", len(queries))
	}
	query := queries[0]
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS prompt_filter_newapi_bindings",
		"api_key_id INT PRIMARY KEY",
		"platform_code VARCHAR(32) NOT NULL UNIQUE",
		"require_signed_identity BOOLEAN NOT NULL DEFAULT FALSE",
		"prompt_filter_scope VARCHAR(16) NOT NULL DEFAULT 'inherit'",
		"previous_secret_expires_at TIMESTAMPTZ NULL",
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("postgres DDL missing %q: %s", fragment, query)
		}
	}
	if !strings.Contains(queries[1], "ADD COLUMN IF NOT EXISTS prompt_filter_scope") {
		t.Fatalf("postgres migration did not add prompt filter scope: %s", queries[1])
	}
	if !strings.Contains(queries[2], "SET policy_mode='inherit', policy_profile='inherit'") || !strings.Contains(queries[2], "prompt_filter_scope IN ('inherit','local_only','off')") {
		t.Fatalf("postgres migration did not retire policy overrides and preserve valid scopes: %s", queries[2])
	}
}

func TestPromptFilterNewAPIBindingMigrationNeutralizesLegacyPolicyOverrides(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "binding-policy-retirement.sqlite"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	apiKeyID, err := db.InsertAPIKey(ctx, "legacy-policy", "sk-legacy-policy-binding-test")
	if err != nil {
		t.Fatalf("InsertAPIKey: %v", err)
	}
	if err := db.CreatePromptFilterNewAPIBinding(ctx, &PromptFilterNewAPIBinding{
		APIKeyID: apiKeyID, PlatformCode: "legacy-policy", Secret: "01234567890123456789012345678901", Enabled: true,
	}); err != nil {
		t.Fatalf("Create binding: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE prompt_filter_newapi_bindings SET policy_mode='shadow', policy_profile='research', prompt_filter_scope='invalid' WHERE api_key_id=?`, apiKeyID); err != nil {
		t.Fatalf("seed legacy policy override: %v", err)
	}
	if err := db.ensurePromptFilterNewAPIBindingsTable(ctx); err != nil {
		t.Fatalf("rerun binding migration: %v", err)
	}
	got, err := db.GetPromptFilterNewAPIBinding(ctx, apiKeyID)
	if err != nil {
		t.Fatalf("Get migrated binding: %v", err)
	}
	if got.PolicyMode != PromptFilterPolicyModeInherit || got.PolicyProfile != PromptFilterPolicyProfileInherit {
		t.Fatalf("legacy policy override survived migration: %#v", got)
	}
	if got.PromptFilterScope != PromptFilterScopeInherit {
		t.Fatalf("invalid prompt filter scope survived migration: %#v", got)
	}
}

func TestSQLiteMigrationDropsLegacyPromptFilterSecretsAndKeepsBindings(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-prompt-filter-secret.sqlite")
	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `
		CREATE TABLE prompt_filter_secrets (
			name TEXT PRIMARY KEY,
			secret TEXT NOT NULL
		);
		INSERT INTO prompt_filter_secrets (name, secret)
		VALUES ('newapi', 'legacy-global-secret');
	`); err != nil {
		db.Close()
		t.Fatalf("create legacy secret table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close sqlite before migration: %v", err)
	}

	db, err = New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen sqlite for migration: %v", err)
	}
	defer db.Close()

	var legacyTableCount int
	if err := db.conn.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM sqlite_master
		WHERE type = 'table' AND name = 'prompt_filter_secrets'
	`).Scan(&legacyTableCount); err != nil {
		t.Fatalf("query legacy table: %v", err)
	}
	if legacyTableCount != 0 {
		t.Fatalf("legacy prompt_filter_secrets table count = %d, want 0", legacyTableCount)
	}

	var bindingTableCount int
	if err := db.conn.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM sqlite_master
		WHERE type = 'table' AND name = 'prompt_filter_newapi_bindings'
	`).Scan(&bindingTableCount); err != nil {
		t.Fatalf("query binding table: %v", err)
	}
	if bindingTableCount != 1 {
		t.Fatalf("prompt_filter_newapi_bindings table count = %d, want 1", bindingTableCount)
	}
}

func TestPostgresMigrationDropsLegacyPromptFilterSecrets(t *testing.T) {
	legacySecretMigrationDriverOnce.Do(func() {
		sql.Register("legacy-prompt-filter-secret-migration-capture", legacySecretMigrationCaptureDriver{})
	})
	legacySecretMigrationQueryMu.Lock()
	legacySecretMigrationQueries = nil
	legacySecretMigrationQueryMu.Unlock()

	conn, err := sql.Open("legacy-prompt-filter-secret-migration-capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	db := &DB{conn: conn, driver: "postgres"}
	if err := db.migrate(context.Background()); !errors.Is(err, errStopLegacySecretMigrationCapture) {
		t.Fatalf("migrate error = %v, want capture stop", err)
	}

	legacySecretMigrationQueryMu.Lock()
	queries := append([]string(nil), legacySecretMigrationQueries...)
	legacySecretMigrationQueryMu.Unlock()
	var query string
	for _, statement := range queries {
		if strings.Contains(statement, "CREATE TABLE IF NOT EXISTS accounts") {
			query = statement
			break
		}
	}
	if query == "" {
		t.Fatal("postgres migration did not execute schema SQL")
	}
	if !strings.Contains(query, "DROP TABLE IF EXISTS prompt_filter_secrets") {
		t.Fatalf("postgres migration does not drop legacy secret table: %s", query)
	}
	if strings.Contains(query, "CREATE TABLE IF NOT EXISTS prompt_filter_secrets") {
		t.Fatalf("postgres migration recreates legacy secret table: %s", query)
	}
}

func TestDeleteAPIKeyDeletesPromptFilterNewAPIBinding(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "delete-binding.sqlite"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	apiKeyID, err := db.InsertAPIKey(ctx, "delete", "sk-delete-binding-test")
	if err != nil {
		t.Fatalf("InsertAPIKey: %v", err)
	}
	if err := db.CreatePromptFilterNewAPIBinding(ctx, &PromptFilterNewAPIBinding{
		APIKeyID: apiKeyID, PlatformCode: "delete-test", PlatformName: "Delete Test",
		Secret: "01234567890123456789012345678901", Enabled: true,
		PolicyMode: PromptFilterPolicyModeInherit, PolicyProfile: PromptFilterPolicyProfileInherit,
	}); err != nil {
		t.Fatalf("Create binding: %v", err)
	}
	if err := db.DeleteAPIKey(ctx, apiKeyID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	if _, err := db.GetPromptFilterNewAPIBinding(ctx, apiKeyID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("orphan binding error=%v, want sql.ErrNoRows", err)
	}
}
