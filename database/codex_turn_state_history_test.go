package database

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTurnStateHistorySnapshotsFiltersPaginationAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UnixMilli()
	for i := 1; i <= 25; i++ {
		row := CodexTurnStateRenewalRecord{AccountID: 37, AccountName: "snapshot account", PlanType: "pro", Model: "gpt-5.6-luna", Attempt: 1, MaxAttempts: 10, ProxyID: 7, ProxyName: "snapshot proxy", ProxyURL: "socks5://user:private-password@host.example:1080/secret-path?token=secret", ProxyIP: "192.0.2.1", Route: "pool", StartedAt: now, ExpiresBefore: now + 600000}
		id, err := db.StartCodexTurnStateHistory(ctx, row)
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			if err := db.FinishCodexTurnStateHistory(ctx, id, "failed", "probe_failed", now+300, 300, 0); err != nil {
				t.Fatal(err)
			}
			if err := db.FinishCodexTurnStateHistory(ctx, id, "success", "renewed", now+400, 400, now+3600000); err != nil {
				t.Fatal(err)
			}
		} else if err := db.FinishCodexTurnStateHistory(ctx, id, "success", "renewed", now+500, 500, now+3600000); err != nil {
			t.Fatal(err)
		}
	}
	page, err := db.ListCodexTurnStateHistory(ctx, 1, 20, CodexTurnStateHistoryFilter{})
	if err != nil || page.Total != 25 || len(page.Records) != 20 || page.Records[0].ID != 25 {
		t.Fatalf("first page: %#v %v", page, err)
	}
	second, err := db.ListCodexTurnStateHistory(ctx, 2, 20, CodexTurnStateHistoryFilter{})
	if err != nil || len(second.Records) != 5 || second.Records[4].Status != "failed" {
		t.Fatalf("second page: %#v %v", second, err)
	}
	filtered, err := db.ListCodexTurnStateHistory(ctx, 1, 20, CodexTurnStateHistoryFilter{AccountID: 37, Plan: "pro", Model: "gpt-5.6-luna", Status: "failed", ProxyURL: "socks5://host.example:1080"})
	if err != nil || filtered.Total != 1 || filtered.Records[0].ProxyName != "snapshot proxy" || len(filtered.Facets.Proxies) != 1 {
		t.Fatalf("filtered: %#v %v", filtered, err)
	}
	encoded, _ := json.Marshal(page)
	for _, secret := range []string{"private-password", "secret-path", "token=secret", "user:"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("history leaked %s", secret)
		}
	}
	malicious, err := db.ListCodexTurnStateHistory(ctx, 1, 20, CodexTurnStateHistoryFilter{Model: "' OR 1=1 --"})
	if err != nil || malicious.Total != 0 {
		t.Fatal("filter was not bound as data")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PruneCodexTurnStateRenewals(ctx); err != nil {
		t.Fatal(err)
	}
	page, err = db.ListCodexTurnStateHistory(ctx, 1, 20, CodexTurnStateHistoryFilter{})
	if err != nil || page.Total != 25 || page.Records[0].ExpiresAfter != now+3600000 {
		t.Fatal("history lost across restart or scheduler cleanup")
	}
	for _, started := range []int64{now - 121000, now - 1000} {
		if _, err := db.StartCodexTurnStateHistory(ctx, CodexTurnStateRenewalRecord{AccountID: 38, AccountName: "other account", PlanType: "team", Model: "gpt-6-astra", Attempt: 2, MaxAttempts: 10, Route: "direct", StartedAt: started}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecoverCodexTurnStateHistory(ctx, now); err != nil {
		t.Fatal(err)
	}
	interrupted, err := db.ListCodexTurnStateHistory(ctx, 1, 20, CodexTurnStateHistoryFilter{Status: "interrupted"})
	if err != nil || interrupted.Total != 1 || interrupted.Records[0].Reason != "worker_interrupted" || interrupted.Records[0].ExpiresAfter != 0 {
		t.Fatal("stale running record was not recovered safely")
	}
	running, err := db.ListCodexTurnStateHistory(ctx, 1, 20, CodexTurnStateHistoryFilter{Status: "running"})
	if err != nil || running.Total != 1 || len(running.Facets.Plans) != 2 {
		t.Fatal("fresh attempt was interrupted or facets were filtered")
	}
}
