package turnstate

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestNativeHarvestPublicationRetainsGenerationFence(t *testing.T) {
	for _, action := range []string{"paste", "clear", "cancel"} {
		t.Run(action, func(t *testing.T) {
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "native-publication.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			ctx := context.Background()
			id, err := db.InsertAccountWithCredentials(ctx, "native-fixture", map[string]any{"access_token": "fixture"}, "")
			if err != nil {
				t.Fatal(err)
			}
			acc := testAccount(id)
			h := NewHarvester(db, stubStore{accounts: []*auth.Account{acc}}, NewCache())
			model := "gpt-5.4"
			generation, version := h.retainGeneration(id, model)
			defer h.releaseGeneration(ticketKey(id, model), generation)
			task := scheduledCell{key: ticketKey(id, model), accountID: id, model: model, generation: generation, version: version, max: 1, attempt: 1}
			h.preparePolicyBatch(ctx, &task, DefaultConfig())
			if !task.expectationCaptured || task.policyEpoch != 0 {
				t.Fatal("native batch failed to capture its disabled-policy expectation")
			}
			if !h.publishAttempt(ctx, task, CachedTicket{AccountID: id, Model: model, Token: "original"}) {
				t.Fatal("initial native publication failed")
			}
			workCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			want := "original"
			switch action {
			case "paste":
				want = "manual"
				if _, err := h.ManualPaste(ctx, id, model, want); err != nil {
					t.Fatal(err)
				}
			case "clear":
				want = ""
				if err := h.Clear(ctx, id, model); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				cancel()
			}
			if h.publishAttempt(workCtx, task, CachedTicket{AccountID: id, Model: model, Token: "stale"}) {
				t.Fatal("stale worker publication accepted")
			}
			calls := 0
			h.probeFn = func(context.Context, Config, *auth.Account, string, string) (string, http.Header, error) {
				calls++
				return "unexpected", nil, nil
			}
			if _, _, err := h.observedProbe(workCtx, DefaultConfig(), acc, task, ""); !errors.Is(err, context.Canceled) || calls != 0 {
				t.Fatalf("stale work probed: calls=%d err=%v", calls, err)
			}
			ticket, exists := h.cache.Get(id, model)
			rows, err := db.ListTurnStateTickets(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if want == "" {
				if exists || len(rows) != 0 {
					t.Fatal("stale work resurrected a cleared ticket")
				}
			} else if !exists || ticket.Token != want || len(rows) != 1 || rows[0].Token != want {
				t.Fatal("stale work overwrote cache or persisted ticket")
			}
		})
	}
}

func TestNativeHarvestMissingExpectationFailsClosed(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "native-expectation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	acc := testAccount(1)
	h := NewHarvester(db, stubStore{accounts: []*auth.Account{acc}}, NewCache())
	generation, version := h.retainGeneration(acc.ID(), "gpt-5.4")
	defer h.releaseGeneration(ticketKey(acc.ID(), "gpt-5.4"), generation)
	task := scheduledCell{key: ticketKey(acc.ID(), "gpt-5.4"), accountID: acc.ID(), model: "gpt-5.4", generation: generation, version: version, max: 1, attempt: 1}
	calls := 0
	h.probeFn = func(context.Context, Config, *auth.Account, string, string) (string, http.Header, error) {
		calls++
		return "unexpected", nil, nil
	}
	if result := h.attempt(context.Background(), DefaultConfig(), acc, task, func() {}); result.phase != "skipped" || calls != 0 {
		t.Fatalf("uncaptured expectation allowed work: calls=%d result=%+v", calls, result)
	}
	if h.publishAttempt(context.Background(), task, CachedTicket{AccountID: acc.ID(), Model: task.model, Token: "unexpected"}) {
		t.Fatal("uncaptured expectation allowed publication")
	}
	if _, exists := h.cache.Get(acc.ID(), task.model); exists {
		t.Fatal("failed publication changed cache")
	}
}
