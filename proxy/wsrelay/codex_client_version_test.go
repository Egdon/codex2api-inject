package wsrelay

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

func TestCodexClientVersionUnavailableStopsWebsocketBeforeHandshake(t *testing.T) {
	old := proxy.CurrentRuntimeSettings()
	previousExecutor := proxy.WebsocketExecuteFunc
	proxy.WebsocketExecuteFunc = ExecuteRequestWebsocket
	proxy.ApplyRuntimeSettings(proxy.RuntimeSettings{ClientCompatMode: proxy.ClientCompatModeAuto, CodexMinCLIVersion: "9.999.0", CodexUserAgentConfig: `{"client_kind":"codex-vscode"}`})
	t.Cleanup(func() {
		proxy.ApplyRuntimeSettings(old)
		proxy.WebsocketExecuteFunc = previousExecutor
	})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	for _, uaEnabled := range []string{"true", "false"} {
		t.Setenv("CODEX_WS_SEND_USER_AGENT", uaEnabled)
		for _, throughProxy := range []bool{false, true} {
			for _, priorSource := range []string{"", proxy.UpstreamSourceOther} {
				ctx := proxy.ContextWithUpstreamTrace(context.Background(), nil)
				if priorSource != "" {
					proxy.RecordWebsocketUpstreamDispatch(ctx, &auth.Account{DBID: 2, UpstreamType: auth.UpstreamAntigravity, AccessToken: "other-token"}, "")
				}
				account := &auth.Account{DBID: 1, AccountID: "test-account", AccessToken: "test-token"}
				body := []byte(`{"model":"gpt-5.4","input":[]}`)
				var err error
				if throughProxy {
					_, err = proxy.ExecuteRequest(ctx, account, body, "session", server.URL, "", nil, nil, true)
				} else {
					_, err = NewExecutor().ExecuteRequestViaWebsocket(ctx, account, body, "session", server.URL, "", nil, nil, "")
				}
				var apiErr *proxy.Error
				if !errors.As(err, &apiErr) || apiErr.Code != proxy.ErrorCodeCodexClientVersionUnavailable || apiErr.Retryable {
					t.Fatalf("version error: %v", err)
				}
				if got := proxy.UpstreamSourceFromContext(ctx); got != priorSource {
					t.Fatalf("throughProxy=%t preparation changed source from %q to %q", throughProxy, priorSource, got)
				}
			}
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream called %d times", calls.Load())
	}
}

func TestNativeWebsocketFailedDialRecordsActualSource(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	ctx := proxy.ContextWithUpstreamTrace(context.Background(), nil)
	calls := 0
	manager.dialer.NetDialContext = func(_ context.Context, _, _ string) (net.Conn, error) {
		calls++
		if got := proxy.UpstreamSourceFromContext(ctx); got != proxy.UpstreamSourceCodex {
			t.Fatalf("dial source = %q", got)
		}
		return nil, errors.New("synthetic dial failure")
	}
	account := &auth.Account{DBID: 1, AccountID: "test-account", AccessToken: "test-token"}
	_, err := manager.createConnection(ctx, account, "ws://native.invalid/responses", "session", http.Header{}, "")
	if err == nil || calls != 1 || proxy.UpstreamSourceFromContext(ctx) != proxy.UpstreamSourceCodex {
		t.Fatalf("failed dial attribution: calls=%d source=%q err=%v", calls, proxy.UpstreamSourceFromContext(ctx), err)
	}
}

func TestNativeWebsocketRejectsIndependentProviderCredentials(t *testing.T) {
	executor := NewExecutorWithManager(nil) // Any accidental transport access panics.
	for _, account := range []*auth.Account{
		nil,
		{DBID: 1, UpstreamType: auth.UpstreamAntigravity, AccessToken: "antigravity-token"},
		{DBID: 2, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.invalid", APIKey: "relay-key", AccessToken: "relay-token"},
	} {
		ctx := proxy.ContextWithUpstreamTrace(context.Background(), nil)
		if _, err := executor.ExecuteRequestViaWebsocket(ctx, account, []byte(`{"model":"gpt-5.4"}`), "", "", "", nil, nil, ""); err == nil {
			t.Fatal("independent provider credentials accepted by native executor")
		}
		if source := proxy.UpstreamSourceFromContext(ctx); source != "" {
			t.Fatalf("rejected provider fabricated dispatch: %q", source)
		}
	}
}

func TestWebsocketClientPoolScopesFullVersionPair(t *testing.T) {
	headers := http.Header{"User-Agent": {"codex/0.158.0-alpha.1 (Windows 10; x86_64) (Codex Desktop; 26.928.31416)"}, "Version": {"0.158.0-alpha.1"}, "Originator": {"Codex Desktop"}}
	key := websocketClientPoolKey("pool", headers)
	if key != websocketClientPoolKey("pool", headers.Clone()) {
		t.Fatal("same client did not retain pool")
	}
	for _, name := range []string{"User-Agent", "Version", "Originator"} {
		changed := headers.Clone()
		changed.Set(name, "different")
		if key == websocketClientPoolKey("pool", changed) {
			t.Fatalf("changed %s reused client pool", name)
		}
	}
	if websocketClientPoolKey("", headers) != "" {
		t.Fatal("empty pool key changed")
	}
}

func TestWebsocketContinuationRejectsChangedClientAndReleasesLease(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(*WsConnection) bool { return true }
	connection := newBoundTestConn(t, manager, 7, "base#0")
	connection.upstreamClientIdentity = "old-client"
	manager.BindResponseConn("response", connection, "base#0", 7, "api-key")
	executor := NewExecutorWithManager(manager)
	got, pending, key := executor.acquireClientContinuation(websocketContinuation{responseID: "response", accountID: 7, apiKey: "api-key", identity: "new-client"})
	if got != nil || pending != nil || key != "" || connection.session.PendingCount() != 0 || !canReuseConnection(connection) {
		t.Fatal("changed client retained continuation or leaked unsent lease")
	}
	got, pending, _ = executor.acquireClientContinuation(websocketContinuation{responseID: "response", accountID: 7, apiKey: "api-key", identity: "old-client"})
	if got != connection || pending == nil {
		t.Fatal("same client lost continuation")
	}
	connection.cancelUnsentReadLease(pending.RequestID)
	connection.session.RemovePendingRequest(pending.RequestID)
}
