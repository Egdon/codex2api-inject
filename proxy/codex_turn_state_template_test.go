package proxy

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// syntheticTurnState builds a valid Fernet-shaped X-Codex-Turn-State for blocks.
func syntheticTurnState(blocks int, at time.Time, marker byte) string {
	raw := make([]byte, 57+16*blocks)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(at.Unix()))
	raw[9] = marker
	return base64.URLEncoding.EncodeToString(raw)
}

func enableTurnStateTemplateCache(t *testing.T) {
	t.Helper()
	prev := CurrentRuntimeSettings()
	next := prev
	next.CodexTurnStateTemplateCache = true
	next.CodexTurnStateAccountMode = CodexTurnStateAccountModePersonal
	ApplyRuntimeSettings(next)
	t.Setenv("CODEX_TURN_STATE_INJECT_MODE", "replace-only")
	t.Setenv("CODEX_TURN_STATE_TTL", "1h")
	t.Setenv("CODEX_TURN_STATE_LOG_DECISIONS", "false")
	t.Setenv("CODEX_TURN_STATE_DRY_RUN", "false")
	t.Setenv("CODEX_TURN_STATE_MAX_ENTRIES", "8")
	t.Setenv("CODEX_TURN_STATE_TEMPLATE_LENGTH", "")
	t.Setenv("CODEX_TURN_STATE_REPLACE_LENGTH", "")
	resetTurnStateTemplateStoreForTest()
	t.Cleanup(func() {
		ApplyRuntimeSettings(prev)
		resetTurnStateTemplateStoreForTest()
		setTurnStateTemplateNowForTest(nil)
	})
}

func enableTurnStateTeamMode(t *testing.T) {
	t.Helper()
	enableTurnStateTemplateCache(t)
	next := CurrentRuntimeSettings()
	next.CodexTurnStateAccountMode = CodexTurnStateAccountModeTeam
	ApplyRuntimeSettings(next)
}

func TestParseTurnStateTokenTable(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	ttl := time.Hour

	cases := []struct {
		name    string
		value   string
		wantErr bool
		blocks  int
		accept  bool // Accept with personal template blocks=10
	}{
		{"blocks10", syntheticTurnState(10, now, 1), false, 10, true},
		{"blocks11_degraded", syntheticTurnState(11, now, 1), false, 11, false},
		{"blocks12_team", syntheticTurnState(12, now, 1), false, 12, false},
		{"blocks13_team_deg", syntheticTurnState(13, now, 1), false, 13, false},
		{"expired", syntheticTurnState(10, now.Add(-2*time.Hour), 1), false, 10, false},
		{"nearly_expired", syntheticTurnState(10, now.Add(-(time.Hour - 20*time.Second)), 1), false, 10, false},
		{"future", syntheticTurnState(10, now.Add(time.Minute), 1), false, 10, false},
		{"skew_ok", syntheticTurnState(10, now.Add(20*time.Second), 1), false, 10, true},
		{"empty", "", true, 0, false},
		{"plain", "not-a-token", true, 0, false},
		{"bad_padding", syntheticTurnState(10, now, 1) + "=", true, 0, false},
		{"whitespace", "gAAAA\r\nHeader", true, 0, false},
		{"spaces", "aaaa bbbb", true, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok, err := parseTurnStateToken(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected parse error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if tok.Blocks != tc.blocks {
				t.Fatalf("blocks=%d want %d", tok.Blocks, tc.blocks)
			}
			got := turnStateTokenAccept(tok, now, ttl, turnStatePersonalTemplateBlocks)
			if got != tc.accept {
				t.Fatalf("Accept=%v want %v", got, tc.accept)
			}
		})
	}
}

func TestCaptureCodexTurnStateTemplateRejectsWrongShape(t *testing.T) {
	enableTurnStateTemplateCache(t)
	acc := &auth.Account{DBID: 11}
	model := "gpt-5.4"
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	setTurnStateTemplateNowForTest(func() time.Time { return now })

	for _, blocks := range []int{0, 9, 11, 12, 13} {
		h := http.Header{}
		if blocks > 0 {
			h.Set(codexTurnStateHeader, syntheticTurnState(blocks, now, 'x'))
		}
		CaptureCodexTurnStateTemplate(nil, acc, model, h)
	}
	if globalTurnStateTemplates.lenForTest() != 0 {
		t.Fatalf("store should reject non-template blocks, got %d entries", globalTurnStateTemplates.lenForTest())
	}
	// multi-value rejected
	h := http.Header{}
	h.Add(codexTurnStateHeader, syntheticTurnState(10, now, 'a'))
	h.Add(codexTurnStateHeader, syntheticTurnState(10, now, 'b'))
	CaptureCodexTurnStateTemplate(nil, acc, model, h)
	if globalTurnStateTemplates.lenForTest() != 0 {
		t.Fatal("multi-value header must not be stored")
	}
}

func TestCaptureCodexTurnStateTemplateAccepts292AndIsolatesKeys(t *testing.T) {
	enableTurnStateTemplateCache(t)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	setTurnStateTemplateNowForTest(func() time.Time { return now })
	a1 := &auth.Account{DBID: 1}
	a2 := &auth.Account{DBID: 2}
	m1, m2 := "gpt-5.4", "gpt-5.5"
	v1 := syntheticTurnState(10, now, '1')
	v2 := syntheticTurnState(10, now, '2')
	v3 := syntheticTurnState(10, now, '3')

	h1 := http.Header{}
	h1.Set(codexTurnStateHeader, v1)
	CaptureCodexTurnStateTemplate(nil, a1, m1, h1)

	h2 := http.Header{}
	h2.Set(codexTurnStateHeader, v2)
	CaptureCodexTurnStateTemplate(nil, a1, m2, h2)

	h3 := http.Header{}
	h3.Set(codexTurnStateHeader, v3)
	CaptureCodexTurnStateTemplate(nil, a2, m1, h3)

	cfg := loadTurnStateTemplateConfig()
	p := cfg.policyFor(a1)
	got, ok := globalTurnStateTemplates.lookup(cfg, p, 1, m1)
	if !ok || got != v1 {
		t.Fatalf("acct1/m1 lookup = %q ok=%v", got, ok)
	}
	got, ok = globalTurnStateTemplates.lookup(cfg, p, 1, m2)
	if !ok || got != v2 {
		t.Fatalf("acct1/m2 lookup = %q ok=%v", got, ok)
	}
	got, ok = globalTurnStateTemplates.lookup(cfg, p, 2, m1)
	if !ok || got != v3 {
		t.Fatalf("acct2/m1 lookup = %q ok=%v", got, ok)
	}
}

func TestTurnStateTemplateStrictParseNeverStoresUnusable(t *testing.T) {
	enableTurnStateTemplateCache(t)
	acc := &auth.Account{DBID: 7}
	model := "gpt-5.4"
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	setTurnStateTemplateNowForTest(func() time.Time { return now })

	// Expired Fernet → never store
	old := syntheticTurnState(10, now.Add(-2*time.Hour), 1)
	h := http.Header{}
	h.Set(codexTurnStateHeader, old)
	CaptureCodexTurnStateTemplate(nil, acc, model, h)
	if globalTurnStateTemplates.lenForTest() != 0 {
		t.Fatal("expired Fernet must never be stored")
	}

	// Non-Fernet / invalid envelope → never store (no issued=now fallback)
	plain := base64.URLEncoding.EncodeToString([]byte("not-fernet-envelope!!!!"))
	// pad to wrong shape
	h2 := http.Header{}
	h2.Set(codexTurnStateHeader, plain)
	CaptureCodexTurnStateTemplate(nil, acc, model, h2)
	if globalTurnStateTemplates.lenForTest() != 0 {
		t.Fatal("invalid envelope must never be stored")
	}

	// Whitespace rejected
	h3 := http.Header{}
	h3.Set(codexTurnStateHeader, syntheticTurnState(10, now, 1)+"\n")
	// TrimSpace would strip trailing newline from Get? Header.Set stores as-is;
	// Capture trims each value — Trailing newline after TrimSpace is gone.
	// Embed internal whitespace:
	h3.Set(codexTurnStateHeader, "gAAA A")
	CaptureCodexTurnStateTemplate(nil, acc, model, h3)
	if globalTurnStateTemplates.lenForTest() != 0 {
		t.Fatal("whitespace envelope must never be stored")
	}

	// Good capture then TTL margin expiry
	good := syntheticTurnState(10, now, 'g')
	h4 := http.Header{}
	h4.Set(codexTurnStateHeader, good)
	CaptureCodexTurnStateTemplate(nil, acc, model, h4)
	cfg := loadTurnStateTemplateConfig()
	p := cfg.policyFor(acc)
	got, ok := globalTurnStateTemplates.lookup(cfg, p, 7, model)
	if !ok || got != good {
		t.Fatalf("good capture lookup = %q ok=%v", got, ok)
	}
	// TTL - 30s margin: usable while now < issued+(TTL-30s)
	setTurnStateTemplateNowForTest(func() time.Time { return now.Add(time.Hour - 31*time.Second) })
	if _, ok := globalTurnStateTemplates.lookup(cfg, p, 7, model); !ok {
		t.Fatal("should remain usable inside TTL-30s margin")
	}
	setTurnStateTemplateNowForTest(func() time.Time { return now.Add(time.Hour - 30*time.Second) })
	if _, ok := globalTurnStateTemplates.lookup(cfg, p, 7, model); ok {
		t.Fatal("Accept must reject at TTL-30s boundary")
	}
}

func TestTeamCaptureAndReplaceLengths(t *testing.T) {
	enableTurnStateTeamMode(t)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	setTurnStateTemplateNowForTest(func() time.Time { return now })
	acc := &auth.Account{DBID: 42, PlanType: "team"}
	model := "gpt-5.4"

	tmpl := syntheticTurnState(12, now, 'T')
	if len(tmpl) != 332 {
		t.Fatalf("team template encoded len=%d want 332", len(tmpl))
	}
	hCap := http.Header{}
	hCap.Set(codexTurnStateHeader, tmpl)
	CaptureCodexTurnStateTemplate(nil, acc, model, hCap)
	if globalTurnStateTemplates.lenForTest() != 1 {
		t.Fatal("team blocks=12 should store")
	}

	// personal 10 must not store under team mode
	hBad := http.Header{}
	hBad.Set(codexTurnStateHeader, syntheticTurnState(10, now, 'P'))
	CaptureCodexTurnStateTemplate(nil, &auth.Account{DBID: 43}, model, hBad)
	if globalTurnStateTemplates.lenForTest() != 1 {
		t.Fatal("personal shape must not store under team mode")
	}

	degraded := syntheticTurnState(13, now, 'D')
	if len(degraded) != 356 {
		t.Fatalf("team degraded encoded len=%d want 356", len(degraded))
	}
	out := http.Header{}
	out.Set(codexTurnStateHeader, degraded)
	ctx := withTurnStateTemplateAudit(context.Background())
	ApplyCodexTurnStateTemplate(ctx, out, acc, model)
	if got := out.Get(codexTurnStateHeader); got != tmpl {
		t.Fatalf("team substitute: got len=%d want 332 template", len(got))
	}

	input := &database.UsageLogInput{}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req, _ := http.NewRequest(http.MethodPost, "/", nil)
	c.Request = req.WithContext(ctx)
	populateTurnStateTemplateMetaFromRequest(c, input)
	if !input.TurnStateOverridden || input.TurnStateRewriteNote != "356→332" {
		t.Fatalf("usage meta = overridden=%v note=%q", input.TurnStateOverridden, input.TurnStateRewriteNote)
	}
}

func TestAutoModeUsesPlanHint(t *testing.T) {
	enableTurnStateTemplateCache(t)
	next := CurrentRuntimeSettings()
	next.CodexTurnStateAccountMode = CodexTurnStateAccountModeAuto
	ApplyRuntimeSettings(next)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	setTurnStateTemplateNowForTest(func() time.Time { return now })

	team := &auth.Account{DBID: 50, PlanType: "team"}
	cfg := loadTurnStateTemplateConfig()
	p := cfg.policyFor(team)
	if p.Mode != "team" || p.TemplateBlocks != 12 || p.ReplaceBlocks != 13 {
		t.Fatalf("auto+team plan policy = %+v", p)
	}
	h := http.Header{}
	h.Set(codexTurnStateHeader, syntheticTurnState(12, now, 'T'))
	CaptureCodexTurnStateTemplate(nil, team, "gpt-5.4", h)
	if globalTurnStateTemplates.lenForTest() != 1 {
		t.Fatal("auto mode should capture team template for team plan")
	}

	personal := &auth.Account{DBID: 51, PlanType: "plus"}
	p2 := cfg.policyFor(personal)
	if p2.Mode != "personal" || p2.TemplateBlocks != 10 {
		t.Fatalf("auto+plus policy = %+v", p2)
	}
}

func TestApplyCodexTurnStateTemplateReplaceOnlyAndAlways(t *testing.T) {
	enableTurnStateTemplateCache(t)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	setTurnStateTemplateNowForTest(func() time.Time { return now })
	acc := &auth.Account{DBID: 9}
	model := "gpt-5.4"
	tmpl := syntheticTurnState(10, now, 'T')
	hCap := http.Header{}
	hCap.Set(codexTurnStateHeader, tmpl)
	CaptureCodexTurnStateTemplate(nil, acc, model, hCap)

	// replace-only + inbound blocks=11 → substitute
	out := http.Header{}
	out.Set(codexTurnStateHeader, syntheticTurnState(11, now, 'D'))
	ApplyCodexTurnStateTemplate(nil, out, acc, model)
	if got := out.Get(codexTurnStateHeader); got != tmpl {
		t.Fatalf("substitute: got len=%d want template", len(got))
	}
	if len(out.Values(codexTurnStateHeader)) != 1 {
		t.Fatal("Del+Set must leave a single header value")
	}

	// replace-only + empty → pass
	empty := http.Header{}
	ApplyCodexTurnStateTemplate(nil, empty, acc, model)
	if got := empty.Get(codexTurnStateHeader); got != "" {
		t.Fatalf("replace-only must not inject into empty, got len=%d", len(got))
	}

	// replace-only + already blocks=10 (different) → pass
	other := http.Header{}
	otherVal := syntheticTurnState(10, now, 'O')
	other.Set(codexTurnStateHeader, otherVal)
	ApplyCodexTurnStateTemplate(nil, other, acc, model)
	if got := other.Get(codexTurnStateHeader); got != otherVal {
		t.Fatal("replace-only must leave non-replace inbound alone")
	}

	// always + empty → inject
	t.Setenv("CODEX_TURN_STATE_INJECT_MODE", "always")
	alwaysEmpty := http.Header{}
	ApplyCodexTurnStateTemplate(nil, alwaysEmpty, acc, model)
	if got := alwaysEmpty.Get(codexTurnStateHeader); got != tmpl {
		t.Fatalf("always inject: got len=%d", len(got))
	}

	// always + degraded → inject
	alwaysDeg := http.Header{}
	alwaysDeg.Set(codexTurnStateHeader, syntheticTurnState(11, now, 'Z'))
	ApplyCodexTurnStateTemplate(nil, alwaysDeg, acc, model)
	if got := alwaysDeg.Get(codexTurnStateHeader); got != tmpl {
		t.Fatalf("always on degraded: got len=%d", len(got))
	}
}

func TestApplyCodexTurnStateTemplateDryRunAndDisabled(t *testing.T) {
	enableTurnStateTemplateCache(t)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	setTurnStateTemplateNowForTest(func() time.Time { return now })
	acc := &auth.Account{DBID: 3}
	model := "gpt-5.4"
	tmpl := syntheticTurnState(10, now, 'T')
	hCap := http.Header{}
	hCap.Set(codexTurnStateHeader, tmpl)
	CaptureCodexTurnStateTemplate(nil, acc, model, hCap)

	t.Setenv("CODEX_TURN_STATE_DRY_RUN", "true")
	out := http.Header{}
	degraded := syntheticTurnState(11, now, 'D')
	out.Set(codexTurnStateHeader, degraded)
	ApplyCodexTurnStateTemplate(nil, out, acc, model)
	if got := out.Get(codexTurnStateHeader); got != degraded {
		t.Fatal("dry-run must not mutate headers")
	}

	t.Setenv("CODEX_TURN_STATE_DRY_RUN", "false")
	off := CurrentRuntimeSettings()
	off.CodexTurnStateTemplateCache = false
	ApplyRuntimeSettings(off)
	out2 := http.Header{}
	out2.Set(codexTurnStateHeader, degraded)
	ApplyCodexTurnStateTemplate(nil, out2, acc, model)
	if got := out2.Get(codexTurnStateHeader); got != degraded {
		t.Fatal("feature-off must not mutate headers")
	}
}

func TestGuardThenApplyReplaceOnlyLeavesStrippedEmpty(t *testing.T) {
	enableTurnStateTemplateCache(t)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	setTurnStateTemplateNowForTest(func() time.Time { return now })
	minter := &auth.Account{DBID: 101}
	other := &auth.Account{DBID: 202}
	model := "gpt-5.4"
	affinityKey := "turn-tmpl-compose::api-key:1"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(affinityKey) })

	tmpl := syntheticTurnState(10, now, 'T')
	hCap := http.Header{}
	hCap.Set(codexTurnStateHeader, tmpl)
	CaptureCodexTurnStateTemplate(nil, other, model, hCap)

	noteCodexTurnStateProvenance(affinityKey, minter)
	echo := http.Header{}
	echo.Set(codexTurnStateHeader, syntheticTurnState(11, now, 'D'))
	guardCodexTurnStateEcho(affinityKey, other, echo)
	if got := echo.Get(codexTurnStateHeader); got != "" {
		t.Fatalf("guard should strip foreign echo, got len=%d", len(got))
	}
	ApplyCodexTurnStateTemplate(nil, echo, other, model)
	if got := echo.Get(codexTurnStateHeader); got != "" {
		t.Fatalf("replace-only after strip must not reinject, got len=%d", len(got))
	}

	t.Setenv("CODEX_TURN_STATE_INJECT_MODE", "always")
	ApplyCodexTurnStateTemplate(nil, echo, other, model)
	if got := echo.Get(codexTurnStateHeader); got != tmpl {
		t.Fatalf("always after strip should inject current tmpl, got len=%d", len(got))
	}
}

func TestTurnStateTemplateMaxEntriesEvictsOldest(t *testing.T) {
	enableTurnStateTemplateCache(t)
	t.Setenv("CODEX_TURN_STATE_MAX_ENTRIES", "2")
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	setTurnStateTemplateNowForTest(func() time.Time { return now })

	cfg := loadTurnStateTemplateConfig()
	for i, seed := range []byte{'a', 'b', 'c'} {
		acc := &auth.Account{DBID: int64(i + 1)}
		h := http.Header{}
		h.Set(codexTurnStateHeader, syntheticTurnState(10, now, seed))
		CaptureCodexTurnStateTemplate(nil, acc, "gpt-5.4", h)
		now = now.Add(time.Minute)
		setTurnStateTemplateNowForTest(func() time.Time { return now })
	}
	if globalTurnStateTemplates.lenForTest() != 2 {
		t.Fatalf("expected 2 entries after eviction, got %d", globalTurnStateTemplates.lenForTest())
	}
	p := cfg.policyFor(&auth.Account{DBID: 1})
	if _, ok := globalTurnStateTemplates.lookup(cfg, p, 1, "gpt-5.4"); ok {
		t.Fatal("oldest account=1 should have been evicted")
	}
	if _, ok := globalTurnStateTemplates.lookup(cfg, p, 2, "gpt-5.4"); !ok {
		t.Fatal("account=2 should remain")
	}
	if _, ok := globalTurnStateTemplates.lookup(cfg, p, 3, "gpt-5.4"); !ok {
		t.Fatal("account=3 should remain")
	}
}

func TestApplyCodexTurnStateTemplateSkipsRelayAccounts(t *testing.T) {
	enableTurnStateTemplateCache(t)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	setTurnStateTemplateNowForTest(func() time.Time { return now })
	relay := &auth.Account{DBID: 55, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://example.invalid", APIKey: "sk-test"}
	model := "gpt-5.4"
	tmpl := syntheticTurnState(10, now, 'T')
	hCap := http.Header{}
	hCap.Set(codexTurnStateHeader, tmpl)
	CaptureCodexTurnStateTemplate(nil, relay, model, hCap)
	if globalTurnStateTemplates.lenForTest() != 0 {
		t.Fatal("relay accounts must not harvest templates")
	}
	out := http.Header{}
	degraded := syntheticTurnState(11, now, 'D')
	out.Set(codexTurnStateHeader, degraded)
	ApplyCodexTurnStateTemplate(nil, out, relay, model)
	if got := out.Get(codexTurnStateHeader); got != degraded {
		t.Fatal("relay accounts must not apply templates")
	}
}

func TestCaptureRejectsUnusableWithoutEvictingFullCache(t *testing.T) {
	enableTurnStateTemplateCache(t)
	t.Setenv("CODEX_TURN_STATE_MAX_ENTRIES", "2")
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	setTurnStateTemplateNowForTest(func() time.Time { return now })

	for i, seed := range []byte{'a', 'b'} {
		acc := &auth.Account{DBID: int64(i + 1)}
		h := http.Header{}
		h.Set(codexTurnStateHeader, syntheticTurnState(10, now, seed))
		CaptureCodexTurnStateTemplate(nil, acc, "gpt-5.4", h)
	}
	if globalTurnStateTemplates.lenForTest() != 2 {
		t.Fatalf("want 2 live entries, got %d", globalTurnStateTemplates.lenForTest())
	}

	expired := syntheticTurnState(10, now.Add(-2*time.Hour), 1)
	h := http.Header{}
	h.Set(codexTurnStateHeader, expired)
	CaptureCodexTurnStateTemplate(nil, &auth.Account{DBID: 99}, "gpt-5.4", h)
	if globalTurnStateTemplates.lenForTest() != 2 {
		t.Fatalf("expired capture must not shrink cache, got %d", globalTurnStateTemplates.lenForTest())
	}

	future := syntheticTurnState(10, now.Add(time.Hour), 1)
	h2 := http.Header{}
	h2.Set(codexTurnStateHeader, future)
	CaptureCodexTurnStateTemplate(nil, &auth.Account{DBID: 100}, "gpt-5.4", h2)
	if globalTurnStateTemplates.lenForTest() != 2 {
		t.Fatalf("future-issued capture must not shrink cache, got %d", globalTurnStateTemplates.lenForTest())
	}

	// Unparseable never stores / never evicts
	h3 := http.Header{}
	h3.Set(codexTurnStateHeader, "not-valid!!!!")
	CaptureCodexTurnStateTemplate(nil, &auth.Account{DBID: 101}, "gpt-5.4", h3)
	if globalTurnStateTemplates.lenForTest() != 2 {
		t.Fatalf("unparseable must not shrink cache, got %d", globalTurnStateTemplates.lenForTest())
	}

	cfg := loadTurnStateTemplateConfig()
	p := cfg.policyFor(&auth.Account{DBID: 1})
	if _, ok := globalTurnStateTemplates.lookup(cfg, p, 1, "gpt-5.4"); !ok {
		t.Fatal("account=1 should remain after rejected captures")
	}
	if _, ok := globalTurnStateTemplates.lookup(cfg, p, 2, "gpt-5.4"); !ok {
		t.Fatal("account=2 should remain after rejected captures")
	}
}

func TestPostInjectStrikeKeepsLiveTemplateUntilExpiry(t *testing.T) {
	enableTurnStateTemplateCache(t)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	setTurnStateTemplateNowForTest(func() time.Time { return now })
	acc := &auth.Account{DBID: 77}
	model := "gpt-5.4"
	tmpl := syntheticTurnState(10, now, 'T')
	hCap := http.Header{}
	hCap.Set(codexTurnStateHeader, tmpl)
	CaptureCodexTurnStateTemplate(nil, acc, model, hCap)
	if globalTurnStateTemplates.lenForTest() != 1 {
		t.Fatal("want template stored")
	}

	// Apply rewrite (substitute) then observe bad response shape twice.
	ctx := withTurnStateTemplateAudit(context.Background())
	out := http.Header{}
	out.Set(codexTurnStateHeader, syntheticTurnState(11, now, 'D'))
	ApplyCodexTurnStateTemplate(ctx, out, acc, model)
	if !turnStateTemplateRewrittenFromContext(ctx) {
		t.Fatal("expected rewritten audit flag")
	}

	bad := http.Header{}
	bad.Set(codexTurnStateHeader, syntheticTurnState(11, now, 'B')) // degraded, not Accept template
	CaptureCodexTurnStateTemplate(ctx, acc, model, bad)
	if globalTurnStateTemplates.lenForTest() != 1 {
		t.Fatal("first strike should keep template")
	}
	if got := globalTurnStateTemplates.strikesForTest(77, model); got != 1 {
		t.Fatalf("strikes=%d want 1", got)
	}

	CaptureCodexTurnStateTemplate(ctx, acc, model, bad)
	if globalTurnStateTemplates.lenForTest() != 1 {
		t.Fatal("second failed observation must not revoke a live template")
	}
}

func TestTurnStateTemplateAuditRecordsRewriteNote(t *testing.T) {
	enableTurnStateTemplateCache(t)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	setTurnStateTemplateNowForTest(func() time.Time { return now })
	acc := &auth.Account{DBID: 7}
	model := "gpt-5.4"
	tmpl := syntheticTurnState(10, now, 'T')
	hCap := http.Header{}
	hCap.Set(codexTurnStateHeader, tmpl)
	CaptureCodexTurnStateTemplate(nil, acc, model, hCap)

	ctx := withTurnStateTemplateAudit(context.Background())
	out := http.Header{}
	out.Set(codexTurnStateHeader, syntheticTurnState(11, now, 'D'))
	ApplyCodexTurnStateTemplate(ctx, out, acc, model)
	if got := out.Get(codexTurnStateHeader); got != tmpl {
		t.Fatalf("expected substitute, got len=%d", len(got))
	}

	input := &database.UsageLogInput{}
	audit := turnStateTemplateAuditFromContext(ctx)
	if audit == nil || !audit.recorded || !audit.rewritten || audit.decision != "substitute" {
		t.Fatalf("audit = %+v", audit)
	}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req, _ := http.NewRequest(http.MethodPost, "/", nil)
	c.Request = req.WithContext(ctx)
	populateTurnStateTemplateMetaFromRequest(c, input)
	if !input.TurnStateOverridden || input.TurnStateRewriteNote != "312→292" {
		t.Fatalf("usage meta = overridden=%v note=%q", input.TurnStateOverridden, input.TurnStateRewriteNote)
	}
}

func TestEncodedLengthsMatchPolicy(t *testing.T) {
	if turnStateEncodedLength(10) != 292 || turnStateEncodedLength(11) != 312 {
		t.Fatalf("personal lengths %d/%d", turnStateEncodedLength(10), turnStateEncodedLength(11))
	}
	if turnStateEncodedLength(12) != 332 || turnStateEncodedLength(13) != 356 {
		t.Fatalf("team lengths %d/%d", turnStateEncodedLength(12), turnStateEncodedLength(13))
	}
}

func TestPurgeExpiredDoesNotDropOtherBlockPolicy(t *testing.T) {
	enableTurnStateTeamMode(t)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	setTurnStateTemplateNowForTest(func() time.Time { return now })

	teamAcc := &auth.Account{DBID: 10, PlanType: "team"}
	personalAcc := &auth.Account{DBID: 11, PlanType: "plus"}
	teamModel := "gpt-5.4-team"
	personalModel := "gpt-5.4-personal"

	teamVal := syntheticTurnState(12, now, 'T')
	hTeam := http.Header{}
	hTeam.Set(codexTurnStateHeader, teamVal)
	CaptureCodexTurnStateTemplate(nil, teamAcc, teamModel, hTeam)
	if globalTurnStateTemplates.lenForTest() != 1 {
		t.Fatalf("want team template stored, len=%d", globalTurnStateTemplates.lenForTest())
	}

	// Switch to personal without resetting the store.
	next := CurrentRuntimeSettings()
	next.CodexTurnStateAccountMode = CodexTurnStateAccountModePersonal
	ApplyRuntimeSettings(next)

	personalVal := syntheticTurnState(10, now, 'P')
	hPers := http.Header{}
	hPers.Set(codexTurnStateHeader, personalVal)
	CaptureCodexTurnStateTemplate(nil, personalAcc, personalModel, hPers)
	if globalTurnStateTemplates.lenForTest() != 2 {
		t.Fatalf("personal capture must not purge valid team entry, len=%d", globalTurnStateTemplates.lenForTest())
	}

	cfg := loadTurnStateTemplateConfig()
	pPers := cfg.policyFor(personalAcc)
	if _, ok := globalTurnStateTemplates.lookup(cfg, pPers, personalAcc.ID(), personalModel); !ok {
		t.Fatal("personal lookup failed")
	}
	if globalTurnStateTemplates.lenForTest() != 2 {
		t.Fatalf("personal lookup purge must not drop team entry, len=%d", globalTurnStateTemplates.lenForTest())
	}

	// Reverse: team lookup must not drop personal.
	next.CodexTurnStateAccountMode = CodexTurnStateAccountModeTeam
	ApplyRuntimeSettings(next)
	cfg = loadTurnStateTemplateConfig()
	pTeam := cfg.policyFor(teamAcc)
	got, ok := globalTurnStateTemplates.lookup(cfg, pTeam, teamAcc.ID(), teamModel)
	if !ok || got != teamVal {
		t.Fatalf("team lookup = %q ok=%v", got, ok)
	}
	if globalTurnStateTemplates.lenForTest() != 2 {
		t.Fatalf("team lookup purge must not drop personal entry, len=%d", globalTurnStateTemplates.lenForTest())
	}
}

func TestRecordTurnStateTemplateAuditPreservesSubstituteOverPass(t *testing.T) {
	ctx := withTurnStateTemplateAudit(context.Background())
	recordTurnStateTemplateAudit(ctx, "substitute", 312, 292, true)
	recordTurnStateTemplateAudit(ctx, "pass", 0, 0, false)

	audit := turnStateTemplateAuditFromContext(ctx)
	if audit == nil {
		t.Fatal("missing audit")
	}
	audit.mu.Lock()
	decision, rewritten, inbound, outbound := audit.decision, audit.rewritten, audit.inboundLen, audit.outboundLen
	audit.mu.Unlock()
	if decision != "substitute" || !rewritten || inbound != 312 || outbound != 292 {
		t.Fatalf("pass overwrote substitute: decision=%s rewritten=%v inbound=%d outbound=%d", decision, rewritten, inbound, outbound)
	}

	// A later inject/substitute may still update.
	recordTurnStateTemplateAudit(ctx, "inject", 0, 292, true)
	audit.mu.Lock()
	decision, rewritten, outbound = audit.decision, audit.rewritten, audit.outboundLen
	audit.mu.Unlock()
	if decision != "inject" || !rewritten || outbound != 292 {
		t.Fatalf("inject should update prior substitute: decision=%s rewritten=%v outbound=%d", decision, rewritten, outbound)
	}
}
