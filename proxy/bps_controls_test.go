package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// Native source regressions use only mock transports. Retired account flags
// must never select a provider or manufacture an actual-dispatch attribution.
type dispatchSourceRoundTripper func(*http.Request) (*http.Response, error)

func (f dispatchSourceRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNativeFailedDispatchKeepsActualSource(t *testing.T) {
	account := &auth.Account{DBID: 91, AccessToken: "synthetic"}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	attachUpstreamTrace(c, nil)
	client := &http.Client{Transport: dispatchSourceRoundTripper(func(req *http.Request) (*http.Response, error) {
		if got := UpstreamSourceFromContext(req.Context()); got != UpstreamSourceCodex {
			t.Fatalf("dispatch source = %q", got)
		}
		// Later account changes cannot change the source of an already sent attempt.
		account.Mu().Lock()
		account.UpstreamType = auth.UpstreamOpenAIResponses
		account.BaseURL = "https://relay.invalid"
		account.APIKey = "synthetic-relay"
		account.Mu().Unlock()
		return nil, errors.New("synthetic transport failure")
	})}
	req, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "https://example.invalid", nil)
	_, err := doTracedUpstreamRequest(client, req, account, "")
	if err == nil {
		t.Fatal("mock transport failure was lost")
	}
	input := &database.UsageLogInput{AccountID: account.ID(), RequestID: "already-set"}
	PopulateUpstreamTrace(c, input)
	if input.UpstreamSource != UpstreamSourceCodex || input.RequestID != "already-set" {
		t.Fatalf("source=%q request=%q", input.UpstreamSource, input.RequestID)
	}
	resetUpstreamRequestTrace(c)
	if UpstreamSourceFromContext(c.Request.Context()) != "" {
		t.Fatal("new request retained source")
	}
}

func TestNativeSourceTracksLastActualDispatchAcrossPreparationFailure(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	attachUpstreamTrace(c, nil)
	ctx := c.Request.Context()
	account := &auth.Account{DBID: 91, AccessToken: "synthetic"}
	client := &http.Client{Transport: dispatchSourceRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{"X-Request-Id": {"native-attempt"}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid", nil)
	resp, err := doTracedUpstreamRequest(client, req, account, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	attempt := snapshotUpstreamTrace(ctx)

	// A native preparation failure clears attempt details, not actual source.
	_, err = ExecuteCompactRequest(ctx, &auth.Account{DBID: account.ID()}, []byte(`{"model":"gpt-5.4"}`), "", "", "", nil, nil)
	if err == nil {
		t.Fatal("missing native credentials should fail before dispatch")
	}
	input := &database.UsageLogInput{AccountID: account.ID()}
	PopulateUpstreamTrace(c, input)
	if input.UpstreamSource != UpstreamSourceCodex || input.UpstreamRequestID != "" || input.UpstreamProxyName != "" {
		t.Fatalf("local preparation failure = %+v", input)
	}
	otherAccount := &database.UsageLogInput{AccountID: 92}
	PopulateUpstreamTrace(c, otherAccount)
	if otherAccount.UpstreamSource != "" {
		t.Fatal("source leaked across account IDs")
	}

	// A later actual provider dispatch supersedes the last source, even on failure.
	relay := &auth.Account{DBID: 92, UpstreamType: auth.UpstreamAntigravity, AccessToken: "synthetic-antigravity"}
	client.Transport = dispatchSourceRoundTripper(func(req *http.Request) (*http.Response, error) {
		if got := UpstreamSourceFromContext(req.Context()); got != UpstreamSourceOther {
			t.Fatalf("independent provider dispatch source = %q", got)
		}
		return nil, errors.New("synthetic provider failure")
	})
	_, _ = doTracedUpstreamRequest(client, req, relay, "")
	input = &database.UsageLogInput{AccountID: relay.ID()}
	PopulateUpstreamTrace(c, input)
	if input.UpstreamSource != UpstreamSourceOther {
		t.Fatalf("last source = %q", input.UpstreamSource)
	}
	hidden := &database.UsageLogInput{AccountID: account.ID()}
	attempt.apply(hidden)
	PopulateUpstreamTrace(c, hidden)
	if hidden.UpstreamSource != UpstreamSourceCodex || hidden.UpstreamRequestID != "native-attempt" {
		t.Fatalf("hidden attempt lost its own trace: %+v", hidden)
	}
}

func TestNativePreparationDoesNotFabricateDispatch(t *testing.T) {
	codexTestIdentityCache(t)
	ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: ClientCompatModeAuto, CodexMinCLIVersion: "0.999.0", CodexUserAgentConfig: `{"client_kind":"codex-desktop"}`})
	for _, compact := range []bool{false, true} {
		ctx := ContextWithUpstreamTrace(context.Background(), nil)
		account := &auth.Account{DBID: 91, AccessToken: "synthetic", AccountID: "synthetic-account"}
		var err error
		if compact {
			_, err = ExecuteCompactRequest(ctx, account, []byte(`{"model":"gpt-5.4","input":[]}`), "", "", "", nil, nil)
		} else {
			_, err = ExecuteRequest(ctx, account, []byte(`{"model":"gpt-5.4","input":[]}`), "", "", "", nil, nil, false)
		}
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Code != ErrorCodeCodexClientVersionUnavailable {
			t.Fatalf("compact=%t version failure = %v", compact, err)
		}
		if got := UpstreamSourceFromContext(ctx); got != "" {
			t.Fatalf("compact=%t fabricated dispatch source = %q", compact, got)
		}
	}
}

func TestHistoricalSourceSnapshotRemainsReadable(t *testing.T) {
	input := &database.UsageLogInput{AccountID: 91}
	upstreamTraceSnapshot{RequestID: "historical", accountID: 91, UpstreamSource: UpstreamSourceBPS, UpstreamRequestID: "historical-attempt"}.apply(input)
	if input.UpstreamSource != "bps" || input.UpstreamRequestID != "historical-attempt" {
		t.Fatalf("historical attribution changed: %+v", input)
	}
}
