package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestTurnStateRenewalBudgetPersistsAndResetsOnlyForNewIssuance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "renewal.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	row := CodexTurnStateTemplate{AccountID: 1, Model: "gpt-5.6-luna", Value: "fixture", IssuedAt: 100}
	if err := db.SaveCodexTurnStateTemplate(ctx, row); err != nil {
		t.Fatal(err)
	}
	claim := func(now int64, want int) {
		t.Helper()
		got, err := db.ClaimCodexTurnStateRenewal(ctx, row, now, now+90)
		if err != nil || got != want {
			t.Fatalf("claim = %d, %v; want %d", got, err, want)
		}
	}
	claim(1000, 1)
	claim(1000, 0)
	if err := db.FinishCodexTurnStateRenewal(ctx, row, 1, 1010); err != nil {
		t.Fatal(err)
	}
	claim(1009, 0)
	if rows, err := db.ListCodexTurnStateRenewalCandidates(ctx, 1009); err != nil || len(rows) != 0 {
		t.Fatalf("pending candidates: %d %v", len(rows), err)
	}
	if err := db.RecordCodexTurnStateRenewalProxy(ctx, row, 1, 7, "hashed-initial-url"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	used, err := db.CodexTurnStateRenewalProxyHashes(ctx, row)
	if err != nil || !used["hashed-initial-url"] {
		t.Fatalf("route history lost on restart: %v", err)
	}
	claim(1010, 2)
	for attempt := 3; attempt <= 10; attempt++ {
		claim(1010+int64(attempt-2)*90, attempt)
	}
	if err := db.SaveCodexTurnStateTemplate(ctx, row); err != nil {
		t.Fatal(err)
	}
	claim(2000, 0)
	if rows, err := db.ListCodexTurnStateRenewalCandidates(ctx, 2000); err != nil || len(rows) != 0 {
		t.Fatalf("exhausted candidates: %d %v", len(rows), err)
	}
	row.IssuedAt++
	used, err = db.CodexTurnStateRenewalProxyHashes(ctx, row)
	if err != nil || len(used) != 0 {
		t.Fatal("new issuance inherited old route history")
	}
	if err := db.SaveCodexTurnStateTemplate(ctx, row); err != nil {
		t.Fatal(err)
	}
	claim(2000, 1)
	if err := db.DeleteCodexTurnStateTemplate(ctx, row.AccountID, row.Model); err != nil {
		t.Fatal(err)
	}
	if err := db.PruneCodexTurnStateRenewals(ctx); err != nil {
		t.Fatal(err)
	}
	claim(3000, 0)
	used, err = db.CodexTurnStateRenewalProxyHashes(ctx, CodexTurnStateTemplate{AccountID: row.AccountID, Model: row.Model, IssuedAt: 100})
	if err != nil || len(used) != 0 {
		t.Fatal("orphan route history was not pruned")
	}
	var count int
	if err := db.conn.QueryRowContext(ctx, "SELECT count(*) FROM codex_turn_state_renewals").Scan(&count); err != nil || count != 0 {
		t.Fatalf("orphan renewal rows = %d, %v", count, err)
	}
}
