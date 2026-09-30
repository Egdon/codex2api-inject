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

// These source regressions use transport mocks only. The obsolete default-on
// master, frozen admission and route-claim tests were replaced by upstream
// tri-state/routing/fallback coverage, rather than retaining a second policy.
type bpsSourceRoundTripper func(*http.Request) (*http.Response, error)

func (f bpsSourceRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBPSFailedDispatchKeepsActualSource(t *testing.T) {
	setExcelBPSGlobalForTest(t, false, "")
	account := testExcelBPSAccount()
	ctx := ContextWithUpstreamTrace(context.Background(), nil)
	oldDo := excelBPSDo
	t.Cleanup(func() { excelBPSDo = oldDo })
	calls := 0
	excelBPSDo = func(req *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
		calls++
		if got := UpstreamSourceFromContext(req.Context()); got != UpstreamSourceBPS {
			t.Fatalf("dispatch source = %q", got)
		}
		return nil, errors.New("synthetic transport failure")
	}
	_, err := ExecuteExcelBPSRequest(ctx, account, []byte(`{"model":"gpt-5.5","input":"hello"}`), "test", "test", "", false)
	if err == nil || calls != 1 || UpstreamSourceFromContext(ctx) != UpstreamSourceBPS {
		t.Fatalf("calls=%d source=%q err=%v", calls, UpstreamSourceFromContext(ctx), err)
	}
	account.SetExcelBPSEnabled(false)
	newCtx := ContextWithUpstreamTrace(context.Background(), nil)
	_, err = ExecuteExcelBPSRequest(newCtx, account, []byte(`{"model":"gpt-5.5","input":"hello"}`), "test", "test", "", false)
	if err == nil || calls != 1 || UpstreamSourceFromContext(newCtx) != "" {
		t.Fatal("disabled request dispatched or fabricated source")
	}
}

func TestBPSSourceUsesDispatchNotSavedFlagOrRequestID(t *testing.T) {
	setExcelBPSGlobalForTest(t, false, "")
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
	if input.UpstreamSource != UpstreamSourceCodex || input.RequestID != "already-set" {
		t.Fatalf("source=%q request=%q", input.UpstreamSource, input.RequestID)
	}
	resetUpstreamRequestTrace(c)
	if UpstreamSourceFromContext(c.Request.Context()) != "" {
		t.Fatal("new request retained source")
	}
	other := &auth.Account{DBID: 92, UpstreamType: auth.UpstreamOpenAIResponses, APIKey: "synthetic", BaseURL: "https://example.invalid"}
	req, _ = http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "https://example.invalid", nil)
	_, _ = doTracedUpstreamRequest(client, req, other, "")
	if got := UpstreamSourceFromContext(c.Request.Context()); got != UpstreamSourceOther {
		t.Fatalf("relay source=%q", got)
	}
}

func TestBPSIngressFallbackSourceTracksLastActualDispatch(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
		t.Run(endpoint, func(t *testing.T) {
			setExcelBPSGlobalForTest(t, true, "")
			resetExcelBPSHealthForTest(t)
			oldDo := excelBPSDo
			t.Cleanup(func() { excelBPSDo = oldDo })
			excelBPSDo = func(*http.Request, *auth.Account, string) (*http.Response, error) {
				return &http.Response{StatusCode: 502, Header: http.Header{"X-Request-Id": {"bps-retry"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"server_error"}}`))}, nil
			}
			account := testExcelBPSAccount()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, endpoint, nil)
			attachUpstreamTrace(c, nil)
			ctx := c.Request.Context()
			fallback := ""
			var handler *Handler
			resp, served, err := handler.openExcelBPSStream(ctx, c, account, []byte(`{"model":"gpt-5.5","input":"hello"}`), excelBPSIngress{Endpoint: endpoint, EffectiveModel: "gpt-5.5", Scope: "test", ThreadKey: "test", Fallback: &fallback})
			if resp != nil || served || err != nil || fallback != "upstream_5xx" || UpstreamSourceFromContext(ctx) != UpstreamSourceBPS {
				t.Fatalf("served=%t fallback=%q source=%q err=%v", served, fallback, UpstreamSourceFromContext(ctx), err)
			}
			bpsAttempt := snapshotUpstreamTrace(ctx)
			// ExecuteRequest resets attempt metadata before local preparation. A
			// failure there must not erase the preceding actual BPS dispatch.
			resetUpstreamUserAgentAudit(ctx)
			input := &database.UsageLogInput{AccountID: account.ID()}
			PopulateUpstreamTrace(c, input)
			if input.UpstreamSource != UpstreamSourceBPS || input.UpstreamRequestID != "" || input.UpstreamProxyName != "" {
				t.Fatalf("pre-dispatch fallback failure = %+v", input)
			}
			client := &http.Client{Transport: bpsSourceRoundTripper(func(req *http.Request) (*http.Response, error) {
				if UpstreamSourceFromContext(req.Context()) != UpstreamSourceCodex {
					t.Fatal("native dispatch still reports BPS")
				}
				return nil, errors.New("synthetic native failure")
			})}
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid", nil)
			_, _ = doTracedUpstreamRequest(client, req, account, "")
			input = &database.UsageLogInput{AccountID: account.ID()}
			PopulateUpstreamTrace(c, input)
			if input.UpstreamSource != UpstreamSourceCodex {
				t.Fatalf("final native error source=%q", input.UpstreamSource)
			}
			hidden := &database.UsageLogInput{AccountID: account.ID()}
			bpsAttempt.apply(hidden)
			PopulateUpstreamTrace(c, hidden)
			if hidden.UpstreamSource != UpstreamSourceBPS || hidden.UpstreamRequestID != "bps-retry" {
				t.Fatalf("hidden retry lost its own trace: %+v", hidden)
			}
		})
	}
}

func TestBPSRequestShapeFallbackDoesNotFabricateDispatch(t *testing.T) {
	account := testExcelBPSAccount()
	ctx := ContextWithUpstreamTrace(context.Background(), nil)
	var handler *Handler
	fallback := ""
	_, served, err := handler.openExcelBPSStream(ctx, nil, account, []byte(`{"model":"gpt-5.5","previous_response_id":"opaque","input":"hello"}`), excelBPSIngress{Fallback: &fallback})
	if served || err != nil || fallback != "stored_response" || UpstreamSourceFromContext(ctx) != "" {
		t.Fatalf("served=%t fallback=%q source=%q err=%v", served, fallback, UpstreamSourceFromContext(ctx), err)
	}
}

func TestBPSConfiguredIntentPreventsStaleTurnStateInjection(t *testing.T) {
	old := turnstate.GetConfig()
	turnstate.SetConfig(turnstate.Config{InjectEnabled: true})
	t.Cleanup(func() { turnstate.SetConfig(old); turnstate.Global().ReplaceAll(nil) })
	now := time.Now().Unix()
	turnstate.Global().Put(turnstate.CachedTicket{AccountID: 91, Model: "gpt-5.5", Token: fakeHarvestToken(now), IssuedUnix: now, Length: 292, Blocks: 10})
	for _, global := range []bool{false, true} {
		setExcelBPSGlobalForTest(t, global, "")
		account := testExcelBPSAccount()
		account.SetExcelBPSEnabled(!global)
		for _, websocket := range []bool{false, true} {
			body := []byte(`{"model":"gpt-5.5"}`)
			ctx, gotBody, headers := prepareCodexTurnStateInjection(context.Background(), account, body, nil, websocket)
			if CodexTurnStateInjectionFromContext(ctx) != "" || headers != nil || string(gotBody) != string(body) {
				t.Fatal("configured BPS account received stale harvester ticket")
			}
		}
	}
}
