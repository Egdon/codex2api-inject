package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexUnifiedClientIdentitySettingPersistence(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "settings.db")
			if driver == "postgres" {
				dsn = os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("requires isolated CODEX2API_TEST_POSTGRES_DSN")
				}
			}
			db, err := New(driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx := context.Background()
			settings := &SystemSettings{PromptFilterCustomPatterns: `[{"id":"fixture"}]`, PromptFilterReviewAPIKey: "fixture-key"}
			if err := db.UpdateSystemSettings(ctx, settings); err != nil {
				t.Fatal(err)
			}
			got, err := db.GetSystemSettings(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got.CodexUnifiedClientIdentityEnabled {
				t.Fatal("unified client identity must default off")
			}
			for _, enabled := range []bool{true, false} {
				settings.CodexUnifiedClientIdentityEnabled = enabled
				settings.PreservePromptFilterCustomPatterns = true
				settings.PreservePromptFilterReviewAPIKey = true
				settings.PromptFilterCustomPatterns = "[]"
				settings.PromptFilterReviewAPIKey = ""
				if err := db.UpdateSystemSettings(ctx, settings); err != nil {
					t.Fatal(err)
				}
				got, err := db.GetSystemSettings(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if got.CodexUnifiedClientIdentityEnabled != enabled {
					t.Fatalf("unified client identity = %v, want %v", got.CodexUnifiedClientIdentityEnabled, enabled)
				}
				if got.PromptFilterCustomPatterns != `[{"id":"fixture"}]` || got.PromptFilterReviewAPIKey != "fixture-key" {
					t.Fatal("new SQL parameter disturbed preserve flags")
				}
			}
			// 模拟旧库升级：缺列时迁移补列并默认关闭。
			if _, err := db.conn.ExecContext(ctx, "ALTER TABLE system_settings DROP COLUMN codex_unified_client_identity_enabled"); err != nil {
				t.Fatal(err)
			}
			upgraded, err := New(driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer upgraded.Close()
			got, err = upgraded.GetSystemSettings(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got.CodexUnifiedClientIdentityEnabled || got.PromptFilterReviewAPIKey != "fixture-key" {
				t.Fatal("migration did not preserve old settings and default off")
			}
		})
	}
}
