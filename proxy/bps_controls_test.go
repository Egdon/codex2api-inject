package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/turnstate"
	"github.com/gin-gonic/gin"
)

// A standalone transport mock: none of these tests opens a listener or sends a
// real model request, even on the native path.
type bpsSourceRoundTripper func(*http.Request) (*http.Response, error)

func (f bpsSourceRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func preserveBPSMaster(t *testing.T, enabled bool) {
	t.Helper()
	old := auth.OpenAIExcelBPSEnabled()
	auth.SetOpenAIExcelBPSEnabled(enabled)
	t.Cleanup(func() { auth.SetOpenAIExcelBPSEnabled(old) })
}

func TestBPSMasterSnapshotSurvivesAccountAndMasterToggle(t *testing.T) {
	preserveBPSMaster(t, true)
	account := testExcelBPSAccount()
	ctx := ContextWithUpstreamTrace(context.Background(), nil)
	ctx, admitted := WithExcelBPSRouteSnapshot(ctx, account, "gpt-5.5")
	if !admitted {
		t.Fatal("BPS account not admitted")
	}
	account.SetExcelBPSEnabled(false)
	auth.SetOpenAIExcelBPSEnabled(false)
	oldDo := excelBPSDo
	t.Cleanup(func() { excelBPSDo = oldDo })
	calls := 0
	excelBPSDo = func(req *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
		calls++
		if got := UpstreamSourceFromContext(req.Context()); got != "bps" {
			t.Fatalf("dispatch source = %q", got)
		}
		return nil, errors.New("synthetic transport failure")
	}
	_, err := ExecuteExcelBPSRequest(ctx, account, []byte(`{"model":"gpt-5.5","input":"hello"}`), "test", "test", "", false)
	if err == nil || calls != 1 {
		t.Fatalf("admitted calls=%d err=%v", calls, err)
	}
	if got := UpstreamSourceFromContext(ctx); got != "bps" {
		t.Fatalf("failed attempt source = %q", got)
	}

	// A new, non-admitted request is rejected before dispatch.
	newCtx := ContextWithUpstreamTrace(context.Background(), nil)
	_, err = ExecuteExcelBPSRequest(newCtx, account, []byte(`{"model":"gpt-5.5","input":"hello"}`), "test", "test", "", false)
	if err == nil || calls != 1 || UpstreamSourceFromContext(newCtx) != "" {
		t.Fatal("new disabled request dispatched")
	}
}

func TestBPSSourceUsesDispatchNotSavedFlagOrRequestID(t *testing.T) {
	preserveBPSMaster(t, false)
	account := testExcelBPSAccount()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	attachUpstreamTrace(c, nil)
	client := &http.Client{Transport: bpsSourceRoundTripper(func(req *http.Request) (*http.Response, error) {
		account.SetExcelBPSEnabled(false)
		return nil, errors.New("synthetic dial failure")
	})}
	req, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "https://example.invalid", nil)
	_, _ = doTracedUpstreamRequest(client, req, account, "")
	input := &database.UsageLogInput{AccountID: account.ID(), RequestID: "already-set"}
	PopulateUpstreamTrace(c, input)
	if input.UpstreamSource != "codex" || input.RequestID != "already-set" {
		t.Fatalf("source=%q request=%q", input.UpstreamSource, input.RequestID)
	}

	resetUpstreamAttemptTrace(c.Request.Context())
	if UpstreamSourceFromContext(c.Request.Context()) != "" {
		t.Fatal("reset retained source")
	}
	other := &auth.Account{DBID: 92, UpstreamType: auth.UpstreamOpenAIResponses, APIKey: "synthetic", BaseURL: "https://example.invalid"}
	req, _ = http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "https://example.invalid", nil)
	_, _ = doTracedUpstreamRequest(client, req, other, "")
	if got := UpstreamSourceFromContext(c.Request.Context()); got != "other" {
		t.Fatalf("relay source=%q", got)
	}
}

func TestBPSGateFreezesMasterAndPreventsCrossRouteRetry(t *testing.T) {
	preserveBPSMaster(t, false)
	account := testExcelBPSAccount()
	gate := &responsesRouteGate{master: auth.OpenAIExcelBPSEnabled()}
	auth.SetOpenAIExcelBPSEnabled(true)
	_, bps, ok := gate.admit(context.Background(), account, "gpt-5.5")
	if !ok || bps {
		t.Fatal("master-off request changed routes after toggle")
	}
	newGate := &responsesRouteGate{master: true}
	native := &auth.Account{DBID: 2, AccessToken: "synthetic"}
	_, bps, ok = newGate.admit(context.Background(), native, "gpt-5.5")
	if !ok || bps || newGate.permits(account, "gpt-5.5") {
		t.Fatal("native attempt allowed BPS fallback")
	}
	if _, _, ok = newGate.admit(context.Background(), account, "gpt-5.5"); ok {
		t.Fatal("cross-route retry admitted")
	}
}

func TestBPSIndependentGatesAtomicallyClaimSession(t *testing.T) {
	preserveBPSMaster(t, true)
	t.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
	for _, endpoint := range []string{"/v1/responses", "/v1/responses/compact"} {
		for _, candidate := range []struct {
			name    string
			account *auth.Account
			allowed bool
		}{
			{name: "native", account: &auth.Account{DBID: 92, AccessToken: "synthetic"}},
			{name: "different_bps_account", account: &auth.Account{DBID: 92, AccessToken: "synthetic", ExcelBPSEnabled: true}},
			{name: "same_binding", account: testExcelBPSAccount(), allowed: true},
		} {
			t.Run(endpoint+"/"+candidate.name, func(t *testing.T) {
				resetResponseCacheStateForTest(defaultResponseCacheConfig())
				makeGate := func() (*responsesRouteGate, context.Context) {
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, endpoint, nil)
					gate, ok := newResponsesRouteGate(c, nil, "key:1", "shared", []byte(`{"input":"hi"}`))
					if !ok {
						t.Fatal("empty session gate rejected")
					}
					return gate, c.Request.Context()
				}
				// Both gates capture empty provenance before either can dispatch.
				a, ctxA := makeGate()
				b, ctxB := makeGate()
				if a.binding.Source != "" || b.binding.Source != "" {
					t.Fatal("test did not create stale empty snapshots")
				}
				accountA := testExcelBPSAccount()
				ctxA, useBPS, admitted := a.admit(ctxA, accountA, "gpt-5.5")
				if !admitted || !useBPS {
					t.Fatal("first BPS claim rejected")
				}
				want := responseRouteBinding{Source: "bps", AccountID: accountA.ID()}
				if got := cachedResponseRoute("key:1", "session:shared"); got != want {
					t.Fatalf("claim missing before dispatch: %+v", got)
				}
				// Model an actual dispatch observation without network I/O.
				beginUpstreamTraceWithSource(ctxA, accountA, "", false, UpstreamSourceBPS)(nil)
				_, _, admitted = b.admit(ctxB, candidate.account, "gpt-5.5")
				if admitted != candidate.allowed {
					t.Fatalf("second admission=%t want %t", admitted, candidate.allowed)
				}
				if got := UpstreamSourceFromContext(ctxB); got != "" {
					t.Fatalf("admission fabricated dispatch source %q", got)
				}
				if got := cachedResponseRoute("key:1", "session:shared"); got != want {
					t.Fatalf("second gate replaced binding: %+v", got)
				}
				// Late observations must not overwrite any still-live marker.
				for _, key := range []string{"session:shared", "response:shared", "call:shared", "encrypted:shared"} {
					rememberResponseRoute("key:1", key, want)
					rememberResponseRoute("key:1", key, responseRouteBinding{Source: "native"})
					rememberResponseRoute("key:1", key, responseRouteBinding{Source: "bps", AccountID: 92})
					if got := cachedResponseRoute("key:1", key); got != want {
						t.Fatalf("late observation replaced %s: %+v", key, got)
					}
				}
			})
		}
	}
}

func TestBPSDisabledRouteCachePreservesUnknownNativeSessions(t *testing.T) {
	preserveBPSMaster(t, true)
	config := defaultResponseCacheConfig()
	config.maxEntries = 0
	resetResponseCacheStateForTest(config)
	t.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
	ctx := withResponseRouteContext(context.Background(), "key:1", "uncached")
	nativeGate := &responsesRouteGate{master: true}
	bpsGate := &responsesRouteGate{master: true}
	if _, bps, ok := nativeGate.admit(ctx, &auth.Account{DBID: 92, AccessToken: "synthetic"}, "gpt-5.5"); !ok || bps {
		t.Fatal("cache-disabled native request lost its existing route")
	}
	if _, _, ok := bpsGate.admit(ctx, testExcelBPSAccount(), "gpt-5.5"); ok {
		t.Fatal("BPS admitted a session without retaining its claim")
	}
	if got := cachedResponseRoute("key:1", "session:uncached"); got.Source != "" {
		t.Fatal("cache-disabled provenance should remain unknown")
	}
}

func TestBPSRouteProvenanceIsScopedBoundedAndFailClosedWhenKnown(t *testing.T) {
	preserveBPSMaster(t, false)
	config := defaultResponseCacheConfig()
	resetResponseCacheStateForTest(config)
	t.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
	binding := responseRouteBinding{Source: "bps", AccountID: 91}
	rememberResponseRoute("key:1", "response:resp-1", binding)
	rememberResponseRoute("key:1", "call:call-1", binding)
	rememberResponseRoute("key:1", "encrypted:opaque", binding)
	for _, raw := range []string{
		`{"previous_response_id":"resp-1"}`,
		`{"input":[{"type":"function_call_output","call_id":"call-1","output":"x"}]}`,
		`{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`,
		`{"input":[{"type":"compaction","encrypted_content":"opaque"}]}`,
	} {
		if got, ok := responseRouteHistory("key:1", "", []byte(raw)); !ok || got != binding {
			t.Fatalf("history %s = %+v/%t", raw, got, ok)
		}
		if got, _ := responseRouteHistory("key:2", "", []byte(raw)); got.Source != "" {
			t.Fatal("cross-key provenance leak")
		}
		for _, endpoint := range []string{"/v1/responses", "/v1/responses/compact"} {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, endpoint, nil)
			if _, ok := newResponsesRouteGate(c, nil, "key:1", "", []byte(raw)); ok || w.Code != http.StatusConflict {
				t.Fatal("disabled known BPS history silently switched")
			}
		}
	}
	rememberResponseRoute("key:1", "session:thread", responseRouteBinding{Source: "native"})
	if _, ok := responseRouteHistory("key:1", "thread", []byte(`{"previous_response_id":"resp-1"}`)); ok {
		t.Fatal("mixed known routes accepted")
	}
	// Expiry is unknown, not an assertion that reconstruction exists.
	respCache.mu.Lock()
	respCache.store[responseRouteKey("key:1", "response:resp-1")].expiresAt = time.Now().Add(-time.Second)
	respCache.mu.Unlock()
	if got := cachedResponseRoute("key:1", "response:resp-1"); got.Source != "" {
		t.Fatal("expired provenance retained")
	}
	config.maxEntries = 1
	resetResponseCacheStateForTest(config)
	rememberResponseRoute("key:1", "response:first", binding)
	rememberResponseRoute("key:1", "response:second", binding)
	if got := cachedResponseRoute("key:1", "response:first"); got.Source != "" {
		t.Fatal("route cache ignored entry bound")
	}
}

func TestBPSDispatchRecordsCompletionProvenance(t *testing.T) {
	preserveBPSMaster(t, true)
	resetResponseCacheStateForTest(defaultResponseCacheConfig())
	t.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
	oldDo := excelBPSDo
	t.Cleanup(func() { excelBPSDo = oldDo })
	excelBPSDo = func(*http.Request, *auth.Account, string) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-bps\",\"status\":\"completed\",\"output\":[]}}\n\n"))}, nil
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	gate, ok := newResponsesRouteGate(c, nil, "key:1", "thread", []byte(`{"input":"hi"}`))
	if !ok {
		t.Fatal("gate")
	}
	ctx, _, _ := gate.admit(c.Request.Context(), testExcelBPSAccount(), "gpt-5.5")
	_, err := forwardExcelBPS(ctx, c, testExcelBPSAccount(), []byte(`{"model":"gpt-5.5","input":"hi"}`), "test", "test", "", false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"response:resp-bps", "session:thread"} {
		if got := cachedResponseRoute("key:1", key); got.Source != "bps" || got.AccountID != 91 {
			t.Fatalf("%s route=%+v", key, got)
		}
	}
}

func TestBPSSavedFlagPreventsStaleTurnStateInjectionWithMasterOff(t *testing.T) {
	preserveBPSMaster(t, false)
	old := turnstate.GetConfig()
	turnstate.SetConfig(turnstate.Config{InjectEnabled: true})
	t.Cleanup(func() { turnstate.SetConfig(old); turnstate.Global().ReplaceAll(nil) })
	now := time.Now().Unix()
	turnstate.Global().Put(turnstate.CachedTicket{AccountID: 91, Model: "gpt-5.5", Token: fakeHarvestToken(now), IssuedUnix: now, Length: 292, Blocks: 10})
	account := testExcelBPSAccount()
	for _, websocket := range []bool{false, true} {
		body := []byte(`{"model":"gpt-5.5"}`)
		ctx, gotBody, headers := prepareCodexTurnStateInjection(context.Background(), account, body, nil, websocket)
		if CodexTurnStateInjectionFromContext(ctx) != "" || headers != nil || string(gotBody) != string(body) {
			t.Fatal("saved BPS account received stale harvester ticket")
		}
	}
}
