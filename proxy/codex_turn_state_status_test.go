package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestTurnStateStatusThresholdsIsolationAndExpiry(t *testing.T) {
	for _, tc := range []struct {
		plan              string
		healthy, degraded int
	}{{"pro", 10, 11}, {"team", 12, 13}} {
		t.Run(tc.plan, func(t *testing.T) {
			enableTurnStateTemplateCache(t)
			now := time.Now()
			setTurnStateTemplateNowForTest(func() time.Time { return now })
			account := &auth.Account{DBID: 71, PlanType: tc.plan}
			observe := func(model string, blocks int) {
				CaptureCodexTurnStateTemplate(nil, account, model, http.Header{codexTurnStateHeader: []string{syntheticTurnState(blocks, now, 1)}})
			}
			observe("model-a", tc.healthy)
			if GetCodexTurnStateStatus(account).State == "healthy" {
				t.Fatal("single observation must not confirm normal")
			}
			observe("model-b", tc.healthy)
			if GetCodexTurnStateStatus(account).State == "healthy" {
				t.Fatal("models must not share consecutive counts")
			}
			observe("model-a", tc.healthy)
			status := GetCodexTurnStateStatus(account)
			if status.Models[0].State != "healthy" || status.TemplateLength != turnStateEncodedLength(tc.healthy) {
				t.Fatalf("unexpected normal state: %+v", status)
			}
			observe("model-c", tc.degraded)
			observe("model-c", tc.degraded)
			if GetCodexTurnStateStatus(account).State != "degraded" {
				t.Fatal("worst model state must win")
			}
			if GetCodexTurnStateStatus(&auth.Account{DBID: 72}).State != "unknown" {
				t.Fatal("account observation leaked")
			}
			now = now.Add(time.Hour)
			if got := GetCodexTurnStateStatus(account); got.State != "unknown" || len(got.Models) != 0 {
				t.Fatal("stale observations survived TTL")
			}
		})
	}
}

func TestTurnStateStatusActualReplacementKeepsLiveTemplate(t *testing.T) {
	enableTurnStateTemplateCache(t)
	account := &auth.Account{DBID: 73, PlanType: "pro"}
	healthy := http.Header{codexTurnStateHeader: []string{syntheticTurnState(10, time.Now(), 1)}}
	degraded := http.Header{codexTurnStateHeader: []string{syntheticTurnState(11, time.Now(), 2)}}
	CaptureCodexTurnStateTemplate(nil, account, "model", healthy)
	for i := 1; i <= 2; i++ {
		ctx := BeginCodexTurnStateTemplateAttempt(context.Background())
		headers := degraded.Clone()
		ApplyCodexTurnStateTemplate(ctx, headers, account, "model")
		if GetCodexTurnStateStatus(account).State == "recovering" && i == 1 {
			t.Fatal("attempted rewrite is not an actual send")
		}
		ConfirmCodexTurnStateTemplate(ctx, headers, account, "model")
		if GetCodexTurnStateStatus(account).State != "recovering" {
			t.Fatal("successful replacement must be orange")
		}
		CaptureCodexTurnStateTemplate(ctx, account, "model", degraded)
		want := "recovering"
		if GetCodexTurnStateStatus(account).State != want {
			t.Fatalf("after %d responses expected %s", i, want)
		}
	}
	if globalTurnStateTemplates.lenForTest() != 1 {
		t.Fatal("failed observations must retain the live template")
	}
	CaptureCodexTurnStateTemplate(nil, account, "model", healthy)
	CaptureCodexTurnStateTemplate(nil, account, "model", healthy)
	if GetCodexTurnStateStatus(account).State != "healthy" {
		t.Fatal("two healthy observations must recover normal state")
	}
}

func TestTurnStateStatusNoFalseRecovery(t *testing.T) {
	for _, mode := range []string{"disabled", "dry-run", "manual", "retry"} {
		t.Run(mode, func(t *testing.T) {
			enableTurnStateTemplateCache(t)
			account := &auth.Account{DBID: 74, PlanType: "pro"}
			CaptureCodexTurnStateTemplate(nil, account, "model", http.Header{codexTurnStateHeader: []string{syntheticTurnState(10, time.Now(), 1)}})
			if mode == "disabled" {
				cfg := CurrentRuntimeSettings()
				cfg.CodexTurnStateTemplateCache = false
				ApplyRuntimeSettings(cfg)
			}
			if mode == "dry-run" {
				t.Setenv("CODEX_TURN_STATE_DRY_RUN", "true")
			}
			ctx := BeginCodexTurnStateTemplateAttempt(context.Background())
			headers := http.Header{codexTurnStateHeader: []string{syntheticTurnState(11, time.Now(), 2)}}
			ApplyCodexTurnStateTemplate(ctx, headers, account, "model")
			if mode == "manual" {
				headers.Set(codexTurnStateHeader, "manual-value")
			}
			if mode == "retry" {
				ctx = BeginCodexTurnStateTemplateAttempt(ctx)
			}
			ConfirmCodexTurnStateTemplate(ctx, headers, account, "model")
			if GetCodexTurnStateStatus(account).State == "recovering" {
				t.Fatal("false recovery")
			}
		})
	}
}

func TestTurnStateStatusDisabledStillObservesWithoutCaching(t *testing.T) {
	enableTurnStateTemplateCache(t)
	cfg := CurrentRuntimeSettings()
	cfg.CodexTurnStateTemplateCache = false
	ApplyRuntimeSettings(cfg)
	account := &auth.Account{DBID: 75, PlanType: "pro"}
	for i := 0; i < 2; i++ {
		CaptureCodexTurnStateTemplate(nil, account, "model", http.Header{codexTurnStateHeader: []string{syntheticTurnState(10, time.Now(), 1)}})
	}
	if GetCodexTurnStateStatus(account).State != "healthy" || globalTurnStateTemplates.lenForTest() != 0 {
		t.Fatal("observation must not enable template cache")
	}
}

func TestTurnStateTemplateHTTPExactModelAndHeadersIsolation(t *testing.T) {
	enableTurnStateTemplateCache(t)
	account := &auth.Account{DBID: 76, AccessToken: "test", PlanType: "pro"}
	healthy := syntheticTurnState(10, time.Now(), 1)
	degraded := syntheticTurnState(11, time.Now(), 2)
	var sent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = r.Header.Get(codexTurnStateHeader)
		w.Header().Set(codexTurnStateHeader, healthy)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin); clientPool.Delete(fmt.Sprintf("resin|%d", account.ID())) })
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "test"})
	clientPool.Delete(fmt.Sprintf("resin|%d", account.ID()))
	downstream := http.Header{codexTurnStateHeader: []string{degraded}}
	for i := 0; i < 2; i++ {
		response, err := ExecuteRequest(WithCodexClientModel(context.Background(), "alias"), account, []byte(`{"model":"exact-upstream","input":"hi"}`), "", "", "", nil, downstream, false)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		want := degraded
		if i == 1 {
			want = healthy
		}
		if sent != want {
			t.Fatalf("request %d used wrong template", i)
		}
		if downstream.Get(codexTurnStateHeader) != degraded {
			t.Fatal("caller headers mutated across attempts")
		}
	}
	cfg := loadTurnStateTemplateConfig()
	if _, ok := globalTurnStateTemplates.lookup(cfg, cfg.policyFor(account), account.ID(), "alias"); ok {
		t.Fatal("template stored against client alias")
	}
}

func TestTurnStateTemplateIgnoresClientMetadataEcho(t *testing.T) {
	token := syntheticTurnState(10, time.Now(), 1)
	for _, event := range []string{"response.created", "response.completed"} {
		frame := []byte(fmt.Sprintf(`{"type":%q,"response":{"client_metadata":{"x-codex-turn-state":%q}}}`, event, token))
		if ObserveCodexTurnStateFrame(nil, frame) != "" {
			t.Fatal("client metadata echo must not seed templates")
		}
	}
}

func TestTurnStateStatusRecoveryEndsWhenTemplateExpires(t *testing.T) {
	enableTurnStateTemplateCache(t)
	now := time.Now()
	setTurnStateTemplateNowForTest(func() time.Time { return now })
	account := &auth.Account{DBID: 79, PlanType: "pro"}
	CaptureCodexTurnStateTemplate(nil, account, "model", http.Header{codexTurnStateHeader: []string{syntheticTurnState(10, now.Add(-58*time.Minute), 1)}})
	ctx := BeginCodexTurnStateTemplateAttempt(nil)
	headers := http.Header{codexTurnStateHeader: []string{syntheticTurnState(11, now, 2)}}
	ApplyCodexTurnStateTemplate(ctx, headers, account, "model")
	ConfirmCodexTurnStateTemplate(ctx, headers, account, "model")
	if GetCodexTurnStateStatus(account).State != "recovering" {
		t.Fatal("expected actual recovery")
	}
	now = now.Add(2 * time.Minute)
	if GetCodexTurnStateStatus(account).State == "recovering" {
		t.Fatal("expired template cannot continue recovery")
	}
}

func TestTurnStateAutoReviewExcludedFromCacheAndStatus(t *testing.T) {
	enableTurnStateTemplateCache(t)
	now := time.Now()
	account := &auth.Account{DBID: 79, PlanType: "pro", CodexTurnStateModels: "*"}
	healthy := syntheticTurnState(10, now, 1)
	degraded := syntheticTurnState(11, now, 2)
	for _, model := range []string{"codex-auto-review", " CODEX-AUTO-REVIEW "} {
		if CodexTurnStateModelAllowed(account, model) {
			t.Error("auto-review must not participate even with wildcard scope")
		}
		CaptureCodexTurnStateTemplate(nil, account, model, http.Header{codexTurnStateHeader: []string{healthy}})
		CaptureCodexTurnStateTemplate(nil, account, model, http.Header{codexTurnStateHeader: []string{degraded}})
		CaptureCodexTurnStateTemplate(nil, account, model, http.Header{codexTurnStateHeader: []string{degraded}})
	}
	if globalTurnStateTemplates.lenForTest() != 0 || len(GetCodexTurnStateStatus(account).Models) != 0 {
		t.Error("auto-review was cached or affected status")
	}
	// Old cached values and observations must also disappear from the projection.
	cfg := loadTurnStateTemplateConfig()
	globalTurnStateTemplates.capture(cfg, cfg.policyFor(account), account.ID(), "codex-auto-review", healthy)
	globalTurnStateTemplates.observeStatus(cfg, account, "codex-auto-review", degraded, false)
	globalTurnStateTemplates.observeStatus(cfg, account, "codex-auto-review", degraded, false)
	headers := http.Header{codexTurnStateHeader: []string{degraded}}
	ApplyCodexTurnStateTemplate(WithCodexTurnStateAdminProbe(context.Background()), headers, account, "codex-auto-review")
	if headers.Get(codexTurnStateHeader) != degraded {
		t.Error("old auto-review template was injected")
	}
	for i := 0; i < 2; i++ {
		CaptureCodexTurnStateTemplate(nil, account, "gpt-5.6-luna", http.Header{codexTurnStateHeader: []string{healthy}})
	}
	status := GetCodexTurnStateStatus(account)
	if status.State != "healthy" || len(status.Models) != 1 || status.Models[0].Model != "gpt-5.6-luna" {
		t.Fatalf("excluded model contaminated healthy account: %+v", status)
	}
	for i := 0; i < 2; i++ {
		CaptureCodexTurnStateTemplate(nil, account, "gpt-5.6-sol", http.Header{codexTurnStateHeader: []string{degraded}})
	}
	if GetCodexTurnStateStatus(account).State != "degraded" {
		t.Fatal("supported model must still affect degraded status")
	}
}
