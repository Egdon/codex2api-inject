package turnstate

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestEnqueueSelectedUsesOneJobAndOnlySelectedAccounts(t *testing.T) {
	old := GetConfig()
	t.Cleanup(func() { SetConfig(old) })
	cfg := DefaultConfig()
	cfg.ZooUserPrefix = "fixture"
	cfg.ZooPassword = "fixture"
	cfg.Models = []string{"gpt-6-astra"}
	cfg.MaxAttempts = 1
	SetConfig(cfg)

	store := stubStore{accounts: []*auth.Account{testAccount(1), testAccount(2), testAccount(3)}}
	h := NewHarvester(nil, store, NewCache())
	var mu sync.Mutex
	var called []int64
	h.probeFn = func(ctx context.Context, cfg Config, acc *auth.Account, model, inject string) (string, http.Header, error) {
		mu.Lock()
		called = append(called, acc.ID())
		mu.Unlock()
		return "", nil, fmt.Errorf("fixture failure")
	}

	if _, err := h.EnqueueSelected([]int64{2, 2}, "", true); err == nil {
		t.Fatal("duplicate IDs should be rejected")
	}
	if _, err := h.EnqueueSelected([]int64{2, 999}, "", true); err == nil {
		t.Fatal("unknown ID should be rejected")
	}
	job, err := h.EnqueueSelected([]int64{3, 1}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(job.AccountIDs) != 2 || job.AccountIDs[0] != 3 || job.AccountIDs[1] != 1 {
		t.Fatalf("job selection = %v", job.AccountIDs)
	}
	deadline := time.After(2 * time.Second)
	for {
		snapshot := h.CurrentJob()
		if snapshot.Status == "done" {
			if snapshot.Total != 2 || snapshot.Done != 2 {
				t.Fatalf("job cells = %d/%d", snapshot.Done, snapshot.Total)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("selected job did not finish")
		case <-time.After(5 * time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(called) != 2 || (called[0] != 1 && called[0] != 3) || (called[1] != 1 && called[1] != 3) || called[0] == called[1] {
		t.Fatalf("selected probes = %v, want [1,3]", called)
	}
}

func TestBatchParticipationPersistsAtomically(t *testing.T) {
	old := GetConfig()
	t.Cleanup(func() { SetConfig(old) })
	SetConfig(DefaultConfig())
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "turn-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := NewHarvester(db, stubStore{accounts: []*auth.Account{testAccount(1), testAccount(2)}}, NewCache())
	if _, err := h.SetHarvestParticipationBatch(context.Background(), []int64{1, 999}, false); err == nil {
		t.Fatal("unknown account should reject entire batch")
	}
	if !AccountHarvestEnabled(GetConfig(), 1) {
		t.Fatal("invalid batch changed memory config")
	}
	cfg, err := h.SetHarvestParticipationBatch(context.Background(), []int64{1, 2}, false)
	if err != nil {
		t.Fatal(err)
	}
	if AccountHarvestEnabled(cfg, 1) || AccountHarvestEnabled(cfg, 2) {
		t.Fatalf("batch did not pause both: %v", cfg.DisabledAccountIDs)
	}
	if _, err := h.SetHarvestParticipationBatch(context.Background(), []int64{1}, true); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.LoadTurnStateConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseConfigJSON(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !AccountHarvestEnabled(parsed, 1) || AccountHarvestEnabled(parsed, 2) {
		t.Fatalf("unexpected persisted participation: %v", parsed.DisabledAccountIDs)
	}
}
