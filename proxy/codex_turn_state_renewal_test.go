package proxy

import (
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestTurnStateRenewalWindowAndEligibility(t *testing.T) {
	enableTurnStateTemplateCache(t)
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		name   string
		age    time.Duration
		blocks int
		want   bool
	}{
		{"before window", 49*time.Minute + 29*time.Second, 10, false},
		{"at ten minutes", 49*time.Minute + 30*time.Second, 10, true},
		{"just before expiry", 59*time.Minute + 29*time.Second, 10, true},
		{"expired", 59*time.Minute + 30*time.Second, 10, false},
		{"degraded", 50 * time.Minute, 11, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &auth.Account{DBID: 901, PlanType: "pro"}
			issued := now.Add(-tc.age)
			row := database.CodexTurnStateTemplate{AccountID: a.ID(), Model: "gpt-5.6-luna", Value: syntheticTurnState(tc.blocks, issued, 1), IssuedAt: issued.Unix()}
			if got := CodexTurnStateTemplateRenewalDue(a, row, now); got != tc.want {
				t.Fatalf("due=%t want %t", got, tc.want)
			}
			if CodexTurnStateTemplateRenewalDue(nil, row, now) {
				t.Fatal("nil account renewed")
			}
			a.CodexTurnStateDisabled = true
			if CodexTurnStateTemplateRenewalDue(a, row, now) {
				t.Fatal("disabled account renewed")
			}
			a.CodexTurnStateDisabled = false
			a.CodexTurnStateModels = "gpt-6-astra"
			if CodexTurnStateTemplateRenewalDue(a, row, now) {
				t.Fatal("out-of-scope model renewed")
			}
		})
	}
	cfg := CurrentRuntimeSettings()
	cfg.CodexTurnStateAccountMode = "auto"
	ApplyRuntimeSettings(cfg)
	a := &auth.Account{DBID: 902, PlanType: "team"}
	issued := now.Add(-50 * time.Minute)
	row := database.CodexTurnStateTemplate{AccountID: a.ID(), Model: "gpt-5.6-luna", Value: syntheticTurnState(12, issued, 1), IssuedAt: issued.Unix()}
	if !CodexTurnStateTemplateRenewalDue(a, row, now) {
		t.Fatal("normal team template not renewed")
	}
	t.Setenv("CODEX_TURN_STATE_DRY_RUN", "true")
	if CodexTurnStateTemplateRenewalDue(a, row, now) {
		t.Fatal("dry run renewed")
	}
}

func TestTurnStateAutoReviewNeverRenews(t *testing.T) {
	enableTurnStateTemplateCache(t)
	now := time.Now()
	issued := now.Add(-50 * time.Minute)
	account := &auth.Account{DBID: 903, PlanType: "pro", CodexTurnStateModels: "codex-auto-review"}
	row := database.CodexTurnStateTemplate{AccountID: account.ID(), Model: "codex-auto-review", Value: syntheticTurnState(10, issued, 1), IssuedAt: issued.Unix()}
	if CodexTurnStateTemplateRenewalDue(account, row, now) {
		t.Fatal("legacy auto-review template triggered renewal")
	}
}
