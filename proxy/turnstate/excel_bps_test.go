package turnstate

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestHarvestSavedBPSFlagExcludesEveryAdmission(t *testing.T) {
	old := GetConfig()
	defer SetConfig(old)
	cfg := DefaultConfig()
	cfg.ZooUserPrefix, cfg.ZooPassword = "fixture", "fixture"
	cfg.AutoHarvest = true
	SetConfig(cfg)
	acc := testAccount(7)
	acc.SetExcelBPSEnabled(true)
	h := NewHarvester(nil, stubStore{accounts: []*auth.Account{acc}}, NewCache())
	calls := 0
	h.probeFn = func(context.Context, Config, *auth.Account, string, string) (string, http.Header, error) {
		calls++
		return "", nil, nil
	}
	// A saved flag remains excluded even when temporarily ineligible for BPS.
	acc.Mu().Lock()
	acc.APIKey = "temporarily-ineligible"
	acc.Mu().Unlock()
	if acc.IsExcelBPSEnabled() {
		t.Fatal("fixture should be route-ineligible")
	}
	if got := filterHarvestAccounts([]*auth.Account{acc}, cfg); len(got) != 0 {
		t.Fatal("saved BPS flag ignored")
	}
	for _, force := range []bool{false, true} {
		_, s, err := h.submit(context.Background(), acc.ID(), nil, "", force, true)
		if err != nil {
			t.Fatal(err)
		}
		if s != nil {
			<-s.done
		}
		if job := h.CurrentJob(); job != nil && job.Total != 0 {
			t.Fatalf("manual force=%t admitted BPS: %+v", force, job)
		}
		_, s, err = h.submit(context.Background(), 0, []int64{acc.ID()}, "", force, true)
		if err != nil {
			t.Fatal(err)
		}
		if s != nil {
			<-s.done
		}
		if job := h.CurrentJob(); job != nil && job.Total != 0 {
			t.Fatal("selected admission included BPS")
		}
	}
	h.scanAuto(context.Background())
	h.harvestCell(context.Background(), cfg, acc, cfg.Models[0])
	if calls != 0 {
		t.Fatalf("excluded account probed %d times", calls)
	}
}

func TestHarvestBPSInflightAndPublicationFence(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "harvest-bps.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "bps-fence", map[string]any{"access_token": "fixture"}, "")
	if err != nil {
		t.Fatal(err)
	}
	acc := testAccount(id)
	h := NewHarvester(db, stubStore{accounts: []*auth.Account{acc}}, NewCache())
	cfg := DefaultConfig()
	// No Astra policy/model restriction: ticket publication for every model is fenced.
	model := "gpt-5.6-sol"
	generation, version := h.retainGeneration(id, model)
	defer h.releaseGeneration(ticketKey(id, model), generation)
	task := scheduledCell{key: ticketKey(id, model), accountID: id, model: model, generation: generation, version: version, max: 1, attempt: 1}
	h.preparePolicyBatch(ctx, &task, cfg)
	if !task.bpsCaptured || task.policyEpoch != 0 {
		t.Fatal("disabled-policy batch must still capture BPS revision")
	}
	original := CachedTicket{AccountID: id, Model: model, Token: "original"}
	if !h.publishAttempt(ctx, task, original) {
		t.Fatal("initial publish failed")
	}
	for _, flag := range []bool{true, false} {
		if err := db.UpdateCredentials(ctx, id, map[string]any{"openai_excel_bps": flag}); err != nil {
			t.Fatal(err)
		}
		// Simulate an outdated runtime store: persistence must be authoritative.
		if h.harvestBPSCurrent(ctx, acc, task) {
			t.Fatal("stale generation still probe-eligible")
		}
		if h.publishAttempt(ctx, task, CachedTicket{AccountID: id, Model: model, Token: "stale"}) {
			t.Fatal("stale publication accepted")
		}
		if got, _ := h.cache.Get(id, model); got.Token != "original" {
			t.Fatal("failed publish changed cache")
		}
	}
	h.preparePolicyBatch(ctx, &task, cfg)
	if !h.harvestBPSCurrent(ctx, acc, task) {
		t.Fatal("disabling did not enable future harvest")
	}
	calls := 0
	h.probeFn = func(context.Context, Config, *auth.Account, string, string) (string, http.Header, error) {
		calls++
		if err := db.UpdateCredentials(ctx, id, map[string]any{"openai_excel_bps": true}); err != nil {
			t.Fatal(err)
		}
		if err := db.UpdateCredentials(ctx, id, map[string]any{"openai_excel_bps": false}); err != nil {
			t.Fatal(err)
		}
		return "old-result", nil, nil
	}
	if result := h.attempt(ctx, cfg, acc, task, func() {}); result.phase != "skipped" {
		t.Fatalf("inflight result survived toggle: %+v", result)
	}
	if calls != 1 {
		t.Fatalf("unexpected probes %d", calls)
	}
	if got, _ := h.cache.Get(id, model); got.Token != "original" {
		t.Fatal("inflight task overwrote cache")
	}
	h.preparePolicyBatch(ctx, &task, cfg)
	if err := db.SaveOpenAIExcelBPSEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateCredentials(ctx, id, map[string]any{"openai_excel_bps": true}); err != nil {
		t.Fatal(err)
	}
	if h.harvestBPSCurrent(ctx, acc, task) {
		t.Fatal("master-off bypassed saved-flag exclusion")
	}
}
