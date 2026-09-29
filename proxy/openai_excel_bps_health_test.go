package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
)

// resetExcelBPSHealthForTest isolates the process-wide route health state.
func resetExcelBPSHealthForTest(t *testing.T) {
	t.Helper()
	reset := func() {
		excelBPSHealth.mu.Lock()
		excelBPSHealth.pauses, excelBPSHealth.cooldowns = nil, nil
		excelBPSHealth.accounts, excelBPSHealth.proxies, excelBPSHealth.probing = nil, nil, nil
		excelBPSHealth.store, excelBPSHealth.backing, excelBPSHealth.now = nil, nil, nil
		excelBPSHealth.initLocked()
		excelBPSHealth.mu.Unlock()
		excelBPSAccessProbe, excelBPSGenerationProbe = nil, nil
	}
	reset()
	t.Cleanup(reset)
}

type excelBPSTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *excelBPSTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *excelBPSTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func useExcelBPSTestClock(t *testing.T) *excelBPSTestClock {
	t.Helper()
	clock := &excelBPSTestClock{now: time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)}
	excelBPSHealth.mu.Lock()
	excelBPSHealth.now = clock.Now
	excelBPSHealth.mu.Unlock()
	return clock
}

func setExcelBPSPauseSettingsForTest(t *testing.T, disabled bool, intervalMinutes int) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
		s.CodexBasispoints403PauseDisabled = disabled
		s.CodexBasispoints403ProbeIntervalMin = intervalMinutes
		return s
	})
}

func excelBPSHealthTestAccount(id int64) *auth.Account {
	return &auth.Account{DBID: id, AccessToken: "synthetic-access-token", AccountID: "chatgpt-account", ExcelBPSEnabled: true}
}

func TestExcelBPSRetryAfterIsClamped(t *testing.T) {
	now := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	for value, want := range map[string]time.Duration{
		"":      excelBPSRateLimitDefault,
		"30":    30 * time.Second,
		"0":     excelBPSRateLimitMin,
		"99999": excelBPSRateLimitMax,
		now.Add(2 * time.Minute).Format(http.TimeFormat): 2 * time.Minute,
	} {
		header := make(http.Header)
		if value != "" {
			header.Set("Retry-After", value)
		}
		if got := excelBPSRetryAfter(header, now); got != want {
			t.Fatalf("Retry-After %q = %s, want %s", value, got, want)
		}
	}
}

func TestExcelBPSHealthRateLimitCoolsOnlyTheRoute(t *testing.T) {
	resetExcelBPSHealthForTest(t)
	clock := useExcelBPSTestClock(t)
	account := excelBPSHealthTestAccount(301)
	header := make(http.Header)
	header.Set("Retry-After", "30")
	excelBPSHealth.observeFailure(context.Background(), account, "gpt-6-astra", http.StatusTooManyRequests, "", header, "")
	if !excelBPSHealth.blocks(account, "gpt-6-astra") || !excelBPSHealth.blocks(account, "gpt-5.6-sol") {
		t.Fatal("429 did not cool the account's Basispoints route")
	}
	if !account.IsExcelBPSEnabled() {
		t.Fatal("429 changed the account setting")
	}
	clock.Advance(31 * time.Second)
	if excelBPSHealth.blocks(account, "gpt-6-astra") {
		t.Fatal("cooldown outlived Retry-After")
	}
}

func TestExcelBPSHealthForbiddenPausesAccountOrModel(t *testing.T) {
	resetExcelBPSHealthForTest(t)
	useExcelBPSTestClock(t)
	setExcelBPSPauseSettingsForTest(t, false, 1)
	ctx := context.Background()

	modelOnly := excelBPSHealthTestAccount(302)
	excelBPSHealth.observeFailure(ctx, modelOnly, "GPT-5.5", http.StatusForbidden, "basispoints_model_access_changed", nil, "")
	if !excelBPSHealth.blocks(modelOnly, "gpt-5.5") || excelBPSHealth.blocks(modelOnly, "gpt-6-astra") {
		t.Fatal("model access 403 must pause only that model")
	}
	if view := ExcelBPSPauseFor(302); view == nil || view.Scope != "models" || len(view.Models) != 1 || view.Models[0] != "gpt-5.5" || view.NextProbeAt == nil {
		t.Fatalf("model pause view = %+v", view)
	}

	whole := excelBPSHealthTestAccount(303)
	excelBPSHealth.observeFailure(ctx, whole, "gpt-6-astra", http.StatusForbidden, "", nil, "")
	if !excelBPSHealth.blocks(whole, "gpt-6-astra") || !excelBPSHealth.blocks(whole, "gpt-5.6-sol") {
		t.Fatal("generic 403 must pause every model of the account")
	}
	if view := ExcelBPSPauseFor(303); view == nil || view.Scope != "account" || view.Reason != excelBPSPauseForbidden {
		t.Fatalf("account pause view = %+v", view)
	}
	if !ClearExcelBPSPause(303) || excelBPSHealth.blocks(whole, "gpt-6-astra") || ExcelBPSPauseFor(303) != nil {
		t.Fatal("clear did not resume the account")
	}

	probing := excelBPSHealthTestAccount(304)
	excelBPSHealth.observeFailure(context.WithValue(ctx, excelBPSProbeContextKey{}, true), probing, "gpt-6-astra", http.StatusForbidden, "", nil, "")
	if excelBPSHealth.blocks(probing, "gpt-6-astra") {
		t.Fatal("a recovery probe must not pause the route it checks")
	}
}

func TestExcelBPSHealthPauseCanBeDisabled(t *testing.T) {
	resetExcelBPSHealthForTest(t)
	setExcelBPSPauseSettingsForTest(t, true, 1)
	account := excelBPSHealthTestAccount(305)
	excelBPSHealth.observeFailure(context.Background(), account, "gpt-6-astra", http.StatusForbidden, "", nil, "")
	if excelBPSHealth.blocks(account, "gpt-6-astra") || ExcelBPSPauseFor(305) != nil {
		t.Fatal("disabled auto pause still paused the route")
	}
}

func TestExcelBPSHealthProbesOnIntervalAndRecovers(t *testing.T) {
	resetExcelBPSHealthForTest(t)
	clock := useExcelBPSTestClock(t)
	setExcelBPSPauseSettingsForTest(t, false, 1)
	ctx := context.Background()
	account := excelBPSHealthTestAccount(306)
	excelBPSHealth.observeFailure(ctx, account, "gpt-5.5", http.StatusForbidden, "basispoints_model_access_changed", nil, "")

	var accessCalls int
	models := []string{"gpt-6-astra"}
	excelBPSAccessProbe = func(context.Context, *auth.Account, string) (excelBPSAccess, error) {
		accessCalls++
		return excelBPSAccess{Allowed: true, Models: models}, nil
	}
	excelBPSGenerationProbe = func(context.Context, *auth.Account, string, string) error {
		t.Fatal("a model pause must not spend a generation probe")
		return nil
	}

	excelBPSHealth.runDue(ctx)
	if accessCalls != 0 {
		t.Fatal("probed before the interval elapsed")
	}
	clock.Advance(time.Minute)
	excelBPSHealth.runDue(ctx)
	if accessCalls != 1 || !excelBPSHealth.blocks(account, "gpt-5.5") {
		t.Fatalf("first probe calls=%d, pause must remain while the model is absent", accessCalls)
	}
	if view := ExcelBPSPauseFor(306); view == nil || view.Failures != 1 || view.LastProbeAt == nil {
		t.Fatalf("failed probe view = %+v", view)
	}

	// A longer interval applies to the pending pause immediately.
	setExcelBPSPauseSettingsForTest(t, false, 5)
	clock.Advance(time.Minute)
	excelBPSHealth.runDue(ctx)
	if accessCalls != 1 {
		t.Fatal("new interval was not applied to the pending pause")
	}
	clock.Advance(4 * time.Minute)
	models = []string{"gpt-6-astra", "GPT-5.5"}
	excelBPSHealth.runDue(ctx)
	if accessCalls != 2 || excelBPSHealth.blocks(account, "gpt-5.5") {
		t.Fatalf("recovered model is still paused (calls=%d)", accessCalls)
	}
}

func TestExcelBPSHealthAccountProbeNeedsAccessThenExactGeneration(t *testing.T) {
	resetExcelBPSHealthForTest(t)
	clock := useExcelBPSTestClock(t)
	setExcelBPSPauseSettingsForTest(t, false, 1)
	ctx := context.Background()
	account := excelBPSHealthTestAccount(307)
	excelBPSHealth.observeFailure(ctx, account, "gpt-6-astra", http.StatusForbidden, "", nil, "")

	allowed := false
	generationCalls := 0
	var generationModel string
	generationErr := errors.New("synthetic mismatch")
	excelBPSAccessProbe = func(context.Context, *auth.Account, string) (excelBPSAccess, error) {
		return excelBPSAccess{Allowed: allowed, Models: []string{"gpt-6-astra"}}, nil
	}
	excelBPSGenerationProbe = func(_ context.Context, _ *auth.Account, _ string, model string) error {
		generationCalls++
		generationModel = model
		return generationErr
	}
	for step := 0; step < 3; step++ {
		clock.Advance(time.Minute)
		if step == 1 {
			allowed = true
		}
		if step == 2 {
			generationErr = nil
		}
		excelBPSHealth.runDue(ctx)
	}
	if generationCalls != 2 || generationModel != "gpt-6-astra" {
		t.Fatalf("generation calls=%d model=%q, want 2 calls after access allowed", generationCalls, generationModel)
	}
	if excelBPSHealth.blocks(account, "gpt-6-astra") {
		t.Fatal("account stayed paused after an exact generation probe")
	}
}

func TestExcelBPSHealthDropsPausesForAccountsThatLeftBasispoints(t *testing.T) {
	resetExcelBPSHealthForTest(t)
	clock := useExcelBPSTestClock(t)
	setExcelBPSPauseSettingsForTest(t, false, 1)
	account := excelBPSHealthTestAccount(308)
	excelBPSHealth.observeFailure(context.Background(), account, "gpt-6-astra", http.StatusForbidden, "", nil, "")
	account.SetExcelBPSEnabled(false)
	excelBPSAccessProbe = func(context.Context, *auth.Account, string) (excelBPSAccess, error) {
		t.Fatal("probed an account that no longer uses Basispoints")
		return excelBPSAccess{}, nil
	}
	clock.Advance(time.Minute)
	excelBPSHealth.runDue(context.Background())
	if ExcelBPSPauseFor(308) != nil {
		t.Fatal("stale pause was kept")
	}
}

type sharedExcelBPSTestCache struct{ cache.TokenCache }

func (sharedExcelBPSTestCache) SharedAcrossInstances() bool { return true }

func TestExcelBPSHealthKeepsAccountPausesAcrossRestart(t *testing.T) {
	resetExcelBPSHealthForTest(t)
	setExcelBPSPauseSettingsForTest(t, false, 1)
	shared := sharedExcelBPSTestCache{cache.NewMemory(1)}
	excelBPSHealth.configure(nil, shared)
	account := excelBPSHealthTestAccount(309)
	excelBPSHealth.observeFailure(context.Background(), account, "gpt-6-astra", http.StatusForbidden, "", nil, "")
	deadline := time.Now().Add(2 * time.Second)
	for {
		raw, ok, _ := shared.GetRuntime(context.Background(), excelBPSHealthNamespace, excelBPSHealthKey)
		if ok && strings.Contains(string(raw), `"account_id":309`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("account pause was not persisted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	restarted := &excelBPSHealthState{}
	restarted.mu.Lock()
	restarted.initLocked()
	restarted.mu.Unlock()
	restarted.load(shared)
	if !restarted.blocks(account, "gpt-5.6-sol") {
		t.Fatal("restart lost the account pause")
	}
}

var excelBPSProbeNoncePattern = regexp.MustCompile(`bps-probe-[0-9a-f-]{36}`)

func TestProbeExcelBPSUsesAccessAndExactNonce(t *testing.T) {
	resetExcelBPSHealthForTest(t)
	old := excelBPSDo
	t.Cleanup(func() { excelBPSDo = old })
	echo := true
	excelBPSDo = func(req *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
		header := make(http.Header)
		if req.Method == http.MethodGet {
			if req.URL.String() != excelBPSAccessURL || req.Header.Get("Authorization") != "Bearer synthetic-access-token" {
				t.Fatalf("access request = %s %v", req.URL, req.Header)
			}
			return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(`{"allowed":true,"models":["gpt-6-astra"]}`))}, nil
		}
		body, _ := io.ReadAll(req.Body)
		nonce := excelBPSProbeNoncePattern.FindString(string(body))
		if nonce == "" {
			t.Fatalf("probe request has no nonce: %s", body)
		}
		if !echo {
			nonce = "something else"
		}
		header.Set("Content-Type", "text/event-stream")
		return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(bpsTestTextSSE("resp_probe", nonce)))}, nil
	}
	account := excelBPSHealthTestAccount(310)
	access, err := probeExcelBPSAccess(context.Background(), account, "")
	if err != nil || !access.Allowed || len(access.Models) != 1 {
		t.Fatalf("access = %+v, %v", access, err)
	}
	if err := probeExcelBPSGeneration(context.Background(), account, "", "gpt-6-astra"); err != nil {
		t.Fatalf("exact nonce probe failed: %v", err)
	}
	echo = false
	if err := probeExcelBPSGeneration(context.Background(), account, "", "gpt-6-astra"); err == nil {
		t.Fatal("a different answer counted as recovery")
	}
}

func TestParseExcelBPSAccessReadsCatalogAndLegacyShapes(t *testing.T) {
	for _, tc := range []struct {
		name, raw  string
		allowed    bool
		models     string
		denial     string
		wantErrors bool
	}{
		{name: "catalog", raw: `{"allowed":true,"model_catalog":{"models":[{"id":"gpt-5.6-sol"},{"id":"GPT-6-Astra"}],"restricted_models":[]}}`, allowed: true, models: "gpt-5.6-sol,gpt-6-astra"},
		{name: "restricted catalog model", raw: `{"allowed":true,"model_catalog":{"models":[{"id":"gpt-5.6-sol"},{"id":"gpt-6-astra"}],"restricted_models":["gpt-6-astra"]}}`, allowed: true, models: "gpt-5.6-sol"},
		{name: "legacy names", raw: `{"allowed":true,"models":["gpt-6-astra","gpt-6-astra"]}`, allowed: true, models: "gpt-6-astra"},
		{name: "denied", raw: `{"allowed":false,"model_catalog":null,"denial_reason":"workspace disabled"}`, denial: "workspace disabled"},
		{name: "not an object", raw: `[]`, wantErrors: true},
		{name: "invalid", raw: `{`, wantErrors: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			access, err := parseExcelBPSAccess([]byte(tc.raw))
			if tc.wantErrors {
				if err == nil {
					t.Fatalf("parse(%s) accepted", tc.raw)
				}
				return
			}
			if err != nil || access.Allowed != tc.allowed || strings.Join(access.Models, ",") != tc.models || access.DenialReason != tc.denial {
				t.Fatalf("parse(%s) = %+v, %v", tc.raw, access, err)
			}
		})
	}
}

func TestExcelBPSModelProbeUsesTheCatalogShape(t *testing.T) {
	resetExcelBPSHealthForTest(t)
	old := excelBPSDo
	t.Cleanup(func() { excelBPSDo = old })
	catalog := `{"allowed":true,"model_catalog":{"models":[{"id":"gpt-6-astra"}],"restricted_models":[]}}`
	excelBPSDo = func(req *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(catalog))}, nil
	}
	account := excelBPSHealthTestAccount(311)
	if err := excelBPSHealth.checkRecovered(context.Background(), account, "", &excelBPSPause{AccountID: 311, Model: "gpt-6-astra", Reason: excelBPSPauseModelAccess}); err != nil {
		t.Fatalf("listed catalog model did not recover: %v", err)
	}
	catalog = `{"allowed":true,"model_catalog":{"models":[{"id":"gpt-6-astra"}],"restricted_models":["gpt-6-astra"]}}`
	if err := excelBPSHealth.checkRecovered(context.Background(), account, "", &excelBPSPause{AccountID: 311, Model: "gpt-6-astra", Reason: excelBPSPauseModelAccess}); err == nil {
		t.Fatal("a restricted catalog model counted as recovered")
	}
	catalog = `{"allowed":false,"model_catalog":null,"denial_reason":"workspace disabled"}`
	if err := excelBPSHealth.checkRecovered(context.Background(), account, "", &excelBPSPause{AccountID: 311, Model: "gpt-6-astra", Reason: excelBPSPauseModelAccess}); err == nil || !strings.Contains(err.Error(), "workspace disabled") {
		t.Fatalf("denied access error = %v", err)
	}
}

func TestResponsesExcelBPSPausedRoutesGoStraightToNative(t *testing.T) {
	for _, tc := range []struct {
		name, body   string
		wantFirst    int
		wantFallback bool
	}{
		{name: "model access", body: `{"error":{"code":"basispoints_model_access_changed"}}`, wantFirst: 200, wantFallback: true},
		{name: "generic forbidden", body: `{"error":{"code":"forbidden"}}`, wantFirst: http.StatusForbidden},
		{name: "rate limited", body: `{"error":{"code":"rate_limit_exceeded"}}`, wantFirst: 200, wantFallback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setExcelBPSGlobalForTest(t, true, "")
			setExcelBPSPauseSettingsForTest(t, false, 1)
			status := http.StatusForbidden
			if tc.name == "rate limited" {
				status = http.StatusTooManyRequests
			}
			harness := newExcelBPSRouteHarness(t, func(int) (int, string) { return status, tc.body })
			raw := `{"model":"gpt-6-astra","input":[{"role":"user","content":"hello"}],"stream":true}`
			first := harness.post(t, raw)
			if first.Code != tc.wantFirst {
				t.Fatalf("first = %d %s", first.Code, first.Body.String())
			}
			if tc.wantFallback != (first.Header().Get("X-Codex2api-Upstream-Fallback") != "") {
				t.Fatalf("fallback header = %q", first.Header().Get("X-Codex2api-Upstream-Fallback"))
			}
			second := harness.post(t, raw)
			if second.Code != 200 || !strings.Contains(second.Body.String(), "native-ok") {
				t.Fatalf("second = %d %s", second.Code, second.Body.String())
			}
			if len(harness.bpsBodies) != 1 {
				t.Fatalf("BPS calls = %d, want 1 (paused route must skip BPS)", len(harness.bpsBodies))
			}
			if ExcelBPSPauseFor(harness.account.ID()) == nil {
				t.Fatal("paused route has no administrator view")
			}
		})
	}
}
