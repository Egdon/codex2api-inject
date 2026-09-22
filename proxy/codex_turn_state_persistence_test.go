package proxy

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestTurnStateDatabaseSurvivesRestartAndScope(t *testing.T) {
	enableTurnStateTemplateCache(t)
	path := filepath.Join(t.TempDir(), "templates.db")
	db, err := database.New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	SetCodexTurnStateTemplateDatabase(db)
	now := time.Now()
	setTurnStateTemplateNowForTest(func() time.Time { return now })
	a := &auth.Account{DBID: 37, PlanType: "pro", CodexTurnStateModels: "gpt-5.6-luna"}
	healthy := syntheticTurnState(10, now, 1)
	CaptureCodexTurnStateTemplate(nil, a, "gpt-5.6-luna", http.Header{codexTurnStateHeader: []string{healthy}})
	CaptureCodexTurnStateTemplate(nil, a, "gpt-5.6-terra", http.Header{codexTurnStateHeader: []string{syntheticTurnState(11, now, 2)}})
	if len(globalTurnStateTemplates.entries) != 0 {
		t.Fatal("production templates retained in memory")
	}
	db.Close()
	resetTurnStateTemplateStoreForTest()
	db, err = database.New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	SetCodexTurnStateTemplateDatabase(db)
	defer SetCodexTurnStateTemplateDatabase(nil)
	setTurnStateTemplateNowForTest(func() time.Time { return now })
	s := GetCodexTurnStateStatus(a)
	if s.State != "ready" || len(s.Models) != 1 || !s.Models[0].TemplateCached {
		t.Fatalf("restart/scope status: %+v", s)
	}
	headers := http.Header{}
	ctx := WithCodexTurnStateAdminProbe(context.Background())
	ApplyCodexTurnStateTemplate(ctx, headers, a, "gpt-5.6-luna")
	if headers.Get(codexTurnStateHeader) != healthy {
		t.Fatal("restart lost database template")
	}
	foreign := http.Header{}
	ApplyCodexTurnStateTemplate(ctx, foreign, &auth.Account{DBID: 38}, "gpt-5.6-luna")
	if foreign.Get(codexTurnStateHeader) != "" {
		t.Fatal("cross-account template reuse")
	}
	now = now.Add(time.Hour)
	headers = http.Header{}
	ApplyCodexTurnStateTemplate(ctx, headers, a, "gpt-5.6-luna")
	if headers.Get(codexTurnStateHeader) != "" {
		t.Fatal("persistence extended expiry")
	}
}

func TestTurnStateAdminProbeInjectsWithoutChangingOrdinaryRequests(t *testing.T) {
	enableTurnStateTemplateCache(t)
	now := time.Now()
	a := &auth.Account{DBID: 39, PlanType: "pro", CodexTurnStateModels: "gpt-5.6-luna"}
	healthy := syntheticTurnState(10, now, 1)
	CaptureCodexTurnStateTemplate(nil, a, "gpt-5.6-luna", http.Header{codexTurnStateHeader: []string{healthy}})
	for i := 0; i < 2; i++ {
		CaptureCodexTurnStateTemplate(nil, a, "gpt-5.6-luna", http.Header{codexTurnStateHeader: []string{syntheticTurnState(11, now, 2)}})
	}
	if GetCodexTurnStateStatus(a).State != "ready" {
		t.Fatal("valid template incorrectly displayed degraded")
	}
	ordinary := http.Header{}
	ApplyCodexTurnStateTemplate(nil, ordinary, a, "gpt-5.6-luna")
	if len(ordinary) != 0 {
		t.Fatal("ordinary replace-only behavior changed")
	}
	ctx := WithCodexTurnStateAdminProbe(nil)
	headers := http.Header{}
	ApplyCodexTurnStateTemplate(ctx, headers, a, "gpt-5.6-luna")
	if headers.Get(codexTurnStateHeader) != healthy {
		t.Fatal("admin probe did not inject")
	}
	ConfirmCodexTurnStateTemplate(ctx, headers, a, "gpt-5.6-luna")
	if GetCodexTurnStateStatus(a).State != "recovering" {
		t.Fatal("successful send not reflected")
	}
	t.Setenv("CODEX_TURN_STATE_DRY_RUN", "true")
	headers = http.Header{}
	ApplyCodexTurnStateTemplate(WithCodexTurnStateAdminProbe(nil), headers, a, "gpt-5.6-luna")
	if len(headers) != 0 {
		t.Fatal("dry-run sent template")
	}
}

func TestTurnStateRefreshRequiresValidatedCandidate(t *testing.T) {
	enableTurnStateTemplateCache(t)
	a := &auth.Account{DBID: 40, PlanType: "pro"}
	model := "gpt-6-astra"
	healthy := syntheticTurnState(10, time.Now(), 1)
	acquire, candidate := NewCodexTurnStateRefresh(nil, a.ID(), model, nil)
	CaptureCodexTurnStateTemplate(acquire, a, model, http.Header{codexTurnStateHeader: []string{healthy}})
	if !candidate.HasCandidate() || candidate.SaveVerified(a) || globalTurnStateTemplates.lenForTest() != 0 {
		t.Fatal("unverified candidate saved")
	}
	verify, validated := NewCodexTurnStateRefresh(nil, a.ID(), model, candidate)
	headers := http.Header{}
	ApplyCodexTurnStateTemplate(verify, headers, a, model)
	if headers.Get(codexTurnStateHeader) != healthy {
		t.Fatal("candidate not sent for verification")
	}
	CaptureCodexTurnStateTemplate(verify, a, model, http.Header{codexTurnStateHeader: []string{syntheticTurnState(11, time.Now(), 2)}})
	if validated.SaveVerified(a) {
		t.Fatal("failed validation saved")
	}
	verify, validated = NewCodexTurnStateRefresh(nil, a.ID(), model, candidate)
	CaptureCodexTurnStateTemplate(verify, a, model, http.Header{codexTurnStateHeader: []string{healthy}})
	if !validated.SaveVerified(a) {
		t.Fatal("verified template not saved")
	}
	// A failed refresh must preserve an existing working template.
	acquire, candidate = NewCodexTurnStateRefresh(nil, a.ID(), model, nil)
	CaptureCodexTurnStateTemplate(acquire, a, model, http.Header{codexTurnStateHeader: []string{syntheticTurnState(11, time.Now(), 3)}})
	if candidate.HasCandidate() || !GetCodexTurnStateStatus(a).Models[0].TemplateCached {
		t.Fatal("failed refresh damaged existing template")
	}
}

func TestTurnStateRefreshWithoutReissuePreservesOriginalExpiry(t *testing.T) {
	enableTurnStateTemplateCache(t)
	account := &auth.Account{DBID: 42, PlanType: "pro"}
	model := "gpt-5.6-luna"
	now := time.Now()
	healthy := syntheticTurnState(10, now.Add(-10*time.Minute), 1)
	acquire, candidate := NewCodexTurnStateRefresh(nil, account.ID(), model, nil)
	CaptureCodexTurnStateTemplate(acquire, account, model, http.Header{codexTurnStateHeader: []string{healthy}})
	_, verified := NewCodexTurnStateRefresh(nil, account.ID(), model, candidate)
	if !verified.SaveVerified(account) {
		t.Fatal("completed validation without reissue rejected acquired template")
	}
	status := GetCodexTurnStateStatus(account)
	if status.State != "recovering" || len(status.Models) != 1 || status.Models[0].Consecutive != 1 {
		t.Fatalf("validation without reissue fabricated a second shape observation: %+v", status)
	}
	cfg := loadTurnStateTemplateConfig()
	value, ok := globalTurnStateTemplates.lookup(cfg, cfg.policyFor(account), account.ID(), model)
	if !ok || value != healthy {
		t.Fatal("original candidate was not retained")
	}
	setTurnStateTemplateNowForTest(func() time.Time { return now.Add(50 * time.Minute) })
	if verified.SaveVerified(account) {
		t.Fatal("validation extended the acquired template's expiry")
	}
}

func TestTurnStateRefreshRejectsLastInvalidObservation(t *testing.T) {
	enableTurnStateTemplateCache(t)
	account := &auth.Account{DBID: 43, PlanType: "pro"}
	model := "gpt-5.6-luna"
	healthy := syntheticTurnState(10, time.Now(), 1)
	acquire, candidate := NewCodexTurnStateRefresh(nil, account.ID(), model, nil)
	CaptureCodexTurnStateTemplate(acquire, account, model, http.Header{codexTurnStateHeader: []string{healthy}})
	for _, values := range [][]string{{"invalid"}, {healthy, healthy}, {syntheticTurnState(11, time.Now(), 2)}} {
		verify, verified := NewCodexTurnStateRefresh(nil, account.ID(), model, candidate)
		CaptureCodexTurnStateTemplate(verify, account, model, http.Header{codexTurnStateHeader: []string{healthy}})
		CaptureCodexTurnStateTemplate(verify, account, model, http.Header{codexTurnStateHeader: values})
		if verified.SaveVerified(account) {
			t.Fatal("invalid final observation fell back to earlier candidate")
		}
	}
}
