package turnstate

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

type stubStore struct {
	accounts []*auth.Account
}

func (s stubStore) Accounts() []*auth.Account { return s.accounts }
func (s stubStore) FindByID(id int64) *auth.Account {
	for _, acc := range s.accounts {
		if acc != nil && acc.ID() == id {
			return acc
		}
	}
	return nil
}

func testAccount(id int64) *auth.Account {
	return &auth.Account{
		DBID:         id,
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		AccountID:    fmt.Sprintf("acc-%d", id),
		Email:        fmt.Sprintf("acc%d@example.com", id),
	}
}

func TestNormalizeConfigAllowsTenPlusAttempts(t *testing.T) {
	got := NormalizeConfig(Config{MaxAttempts: 10, CooldownMinutes: 15})
	if got.MaxAttempts != 10 {
		t.Fatalf("max_attempts=10 clamped to %d", got.MaxAttempts)
	}
	if got.CooldownMinutes != 15 {
		t.Fatalf("cooldown=%d", got.CooldownMinutes)
	}
	got = NormalizeConfig(Config{MaxAttempts: 50})
	if got.MaxAttempts != 50 {
		t.Fatalf("max_attempts=50 clamped to %d", got.MaxAttempts)
	}
}

func TestHarvestCellExhaustsPerModelThenCoolsDown(t *testing.T) {
	cache := NewCache()
	h := NewHarvester(nil, stubStore{accounts: []*auth.Account{testAccount(7)}}, cache)
	var shots atomic.Int32
	h.probeFn = func(ctx context.Context, cfg Config, acc *auth.Account, model, inject string) (string, http.Header, error) {
		shots.Add(1)
		return strings.Repeat("A", DegradedChars), http.Header{}, nil
	}
	cfg := DefaultConfig()
	cfg.MaxAttempts = 3
	cfg.CooldownMinutes = 15
	cfg.Models = []string{"gpt-6-astra", "gpt-5.6-sol"}
	acc := testAccount(7)

	h.harvestCell(context.Background(), cfg, acc, "gpt-6-astra")
	if shots.Load() != 3 {
		t.Fatalf("astra shots=%d want 3", shots.Load())
	}
	tkt, ok := cache.Get(7, "gpt-6-astra")
	if !ok || !tkt.Exhausted {
		t.Fatalf("astra ticket exhausted=%v ok=%v", tkt.Exhausted, ok)
	}
	if tkt.CooldownUntil <= time.Now().Unix() {
		t.Fatalf("cooldown not set: %d", tkt.CooldownUntil)
	}
	if !strings.Contains(tkt.LastError, "尝试耗尽") {
		t.Fatalf("last_error=%q", tkt.LastError)
	}

	cells := h.dueCells([]*auth.Account{acc}, cfg, "", false)
	for _, c := range cells {
		if c.model == "gpt-6-astra" {
			t.Fatal("exhausted astra should stay in cooldown")
		}
	}
	foundSol := false
	for _, c := range cells {
		if c.model == "gpt-5.6-sol" {
			foundSol = true
		}
	}
	if !foundSol {
		t.Fatal("sol should still be due")
	}

	forced := h.dueCells([]*auth.Account{acc}, cfg, "gpt-6-astra", true)
	if len(forced) != 1 {
		t.Fatalf("manual force should bypass cooldown, got %d", len(forced))
	}
}

func TestHarvestCellSuccessClearsExhausted(t *testing.T) {
	cache := NewCache()
	h := NewHarvester(nil, stubStore{}, cache)
	tok := fakeFernet(time.Now().Unix(), 160)
	h.probeFn = func(ctx context.Context, cfg Config, acc *auth.Account, model, inject string) (string, http.Header, error) {
		return tok, http.Header{}, nil
	}
	acc := testAccount(3)
	cache.Put(CachedTicket{AccountID: 3, Model: "gpt-6-astra", Exhausted: true, LastError: "old", CooldownUntil: time.Now().Add(time.Hour).Unix()})
	h.setCooldown(3, "gpt-6-astra", time.Now().Add(time.Hour))
	h.harvestCell(context.Background(), DefaultConfig(), acc, "gpt-6-astra")
	tkt, ok := cache.Get(3, "gpt-6-astra")
	if !ok || tkt.Exhausted || tkt.Token != tok {
		t.Fatalf("ticket=%+v ok=%v", tkt, ok)
	}
	if tkt.ConfirmWarning {
		t.Fatal("confirm should pass")
	}
	cells := h.dueCells([]*auth.Account{acc}, Config{Models: []string{"gpt-6-astra"}, SkipTTLMinutes: 15}, "", false)
	if len(cells) != 0 {
		t.Fatalf("fresh 292 should skip, got %+v", cells)
	}
}
