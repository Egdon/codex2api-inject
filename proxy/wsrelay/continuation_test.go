package wsrelay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

func continuationFixture(t *testing.T) (*Executor, *WsConnection) {
	t.Helper()
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(*WsConnection) bool { return true }
	wc := newBoundTestConn(t, manager, 7, "continuation")
	wc.session.ID = "continuation"
	wc.upstreamClientIdentity = "client"
	manager.BindResponseConn("previous", wc, wc.session.ID, 7, "key")
	return NewExecutorWithManager(manager), wc
}

func TestContinuationWaitsForOriginalConnection(t *testing.T) {
	executor, original := continuationFixture(t)
	busy, err := executor.manager.addPendingAndBeginReadLease(original, "busy", capacityChat)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan *WsConnection, 1)
	go func() {
		wc, pending, _, err := executor.acquireContinuation(ctx, websocketContinuation{responseID: "previous", accountID: 7, apiKey: "key", identity: "client"})
		if err == nil {
			original.cancelUnsentReadLease(pending.RequestID)
			original.session.RemovePendingRequest(pending.RequestID)
		}
		done <- wc
	}()
	select {
	case <-done:
		t.Fatal("busy original connection fell through")
	case <-time.After(20 * time.Millisecond):
	}
	original.cancelUnsentReadLease(busy.RequestID)
	original.session.RemovePendingRequest(busy.RequestID)
	select {
	case got := <-done:
		if original != got {
			t.Fatal("continuation did not reuse original connection")
		}
	case <-ctx.Done():
		t.Fatal("release did not wake continuation")
	}
}

func TestContinuationCancellationAndLostBinding(t *testing.T) {
	executor, original := continuationFixture(t)
	busy, _ := executor.manager.addPendingAndBeginReadLease(original, "busy", capacityChat)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err := executor.acquireContinuation(ctx, websocketContinuation{responseID: "previous", accountID: 7, apiKey: "key", identity: "client"})
	if !errors.Is(err, context.Canceled) || original.session.PendingCount() != 1 {
		t.Fatalf("cancellation leaked a reservation: %v", err)
	}
	original.cancelUnsentReadLease(busy.RequestID)
	original.session.RemovePendingRequest(busy.RequestID)
	executor.manager.DiscardConnection(original)
	_, _, _, err = executor.acquireContinuation(context.Background(), websocketContinuation{responseID: "previous", accountID: 7, apiKey: "key", identity: "client"})
	var lost *proxy.ResponsesContinuationLostError
	if !errors.As(err, &lost) {
		t.Fatalf("lost binding returned %v", err)
	}
}

func TestContinuationSendFailureDoesNotRedialWithOldID(t *testing.T) {
	executor, original := continuationFixture(t)
	account := &auth.Account{DBID: 7, AccessToken: "fixture-token"}
	headers := executor.prepareWebsocketHeaders(context.Background(), account.AccessToken, account, "", "session", "key", nil, http.Header{}, nil, "")
	original.upstreamClientIdentity = websocketClientIdentity(headers)
	wsURL, err := buildWebsocketURL(proxy.CodexBaseURL + CodexWsEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	executor.manager.connections.Delete(original.PoolKey)
	executor.manager.sessions.Delete(original.PoolKey)
	original.PoolKey = executor.manager.poolKey(account.ID(), wsURL, original.session.ID, "")
	executor.manager.connections.Store(original.PoolKey, original)
	executor.manager.sessions.Store(original.PoolKey, original.session)
	original.conn = newClosedTestWebsocketConn(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = executor.ExecuteRequestViaWebsocket(ctx, account, []byte(`{"model":"gpt-5.5","previous_response_id":"previous","input":[]}`), "session", "", "key", nil, http.Header{}, "")
	var lost *proxy.ResponsesContinuationLostError
	if !errors.As(err, &lost) || lost.Reason != "original_connection_send_failed" {
		t.Fatalf("send retried stale ID: %v", err)
	}
	if executor.manager.ConnectionCount() != 0 || original.session.PendingCount() != 0 {
		t.Fatal("failed original retained connection or reservation")
	}
}

func TestContinuationBindingPrecedesTerminalDelivery(t *testing.T) {
	executor, original := continuationFixture(t)
	response := &WsResponse{conn: original, manager: executor.manager, sessionID: original.session.ID, apiKey: "key"}
	err := response.handleMessage([]byte(`{"type":"response.completed","response":{"id":"latest"}}`), func([]byte) bool {
		got, _ := executor.manager.lookupResponseConn("latest", 7, "key")
		if original != got {
			t.Fatal("terminal exposed before binding")
		}
		if err := response.Close(); err != nil || !original.IsConnected() {
			t.Fatal("terminal-triggered cancellation discarded completed connection")
		}
		return false
	})
	if !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if got, _ := executor.manager.lookupResponseConn("latest", 7, "key"); original != got {
		t.Fatal("consumed terminal lost reusable connection")
	}
}

func TestContinuationBusyTimeoutPreservesOriginal(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	proxy.UpdateRuntimeSettings(func(settings proxy.RuntimeSettings) proxy.RuntimeSettings {
		settings.CodexWSBusyMaxWaitSec = 1
		return settings
	})
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	executor, original := continuationFixture(t)
	busy, err := executor.manager.addPendingAndBeginReadLease(original, "busy", capacityChat)
	if err != nil {
		t.Fatal(err)
	}
	defer original.session.RemovePendingRequest(busy.RequestID)
	defer original.cancelUnsentReadLease(busy.RequestID)
	_, _, _, err = executor.acquireContinuation(context.Background(), websocketContinuation{responseID: "previous", accountID: 7, apiKey: "key", identity: "client"})
	var full *proxy.ResponsesContinuationBusyError
	if !errors.As(err, &full) || !original.IsConnected() || original.session.PendingCount() != 1 {
		t.Fatalf("busy continuation changed original or misclassified timeout: %v", err)
	}
}

func TestContinuationChecksModelAndEgress(t *testing.T) {
	executor, original := continuationFixture(t)
	executor.manager.bindResponseConn("previous", responseConnBinding{conn: original, sessionKey: original.session.ID,
		accountID: 7, apiKey: "key", model: "model-a"})
	base := websocketContinuation{responseID: "previous", accountID: 7, apiKey: "key", identity: "client", model: "model-a", url: original.URL}
	for _, changed := range []websocketContinuation{
		{responseID: "previous", accountID: 7, apiKey: "key", identity: "client", model: "model-b"},
		{responseID: "previous", accountID: 7, apiKey: "key", identity: "client", url: "wss://other.test/responses"},
		{responseID: "previous", accountID: 7, apiKey: "key", identity: "client", url: original.URL, proxyURL: "http://other.test:8080"},
	} {
		if got, _ := executor.manager.lookupContinuationConn(changed); got != nil {
			t.Fatal("changed model or egress reused original")
		}
	}
	if got, _ := executor.manager.lookupContinuationConn(base); original != got {
		t.Fatal("matching model and egress lost original")
	}
}

func TestIncompleteResponseEndsReadLeaseAndBinds(t *testing.T) {
	executor, original := continuationFixture(t)
	response := &WsResponse{conn: original, manager: executor.manager, sessionID: original.session.ID, apiKey: "key"}
	payload := []byte(`{"type":"response.incomplete","response":{"id":"partial"}}`)
	if !isReadLeaseTerminal(payload) || response.handleMessage(payload, func([]byte) bool { return true }) != io.EOF {
		t.Fatal("incomplete response retained active stream")
	}
	if got, _ := executor.manager.lookupResponseConn("partial", 7, "key"); original != got {
		t.Fatal("incomplete response lost continuation binding")
	}
}

func TestIdleChatRetentionEvictsOldestBoundAndProtectsActive(t *testing.T) {
	executor, bound := continuationFixture(t)
	manager := executor.manager
	active := newBoundTestConn(t, manager, 7, "active")
	active.session.AddPendingRequest("active")
	active.lastUsed.Store(time.Now().Add(-2 * connectionIdleTimeout()).UnixNano())
	bound.lastUsed.Store(time.Now().Add(-2 * time.Minute).UnixNano())
	unbound := newBoundTestConn(t, manager, 7, "unbound")
	unbound.lastUsed.Store(time.Now().Add(-time.Minute).UnixNano())
	if !manager.ensureAccountConnectionCapacity(connectionCapacityRequest{accountID: 7, limit: 1}) {
		t.Fatal("idle retention should not reject new inference")
	}
	if !active.IsConnected() || bound.IsConnected() || !unbound.IsConnected() {
		t.Fatal("LRU should evict the oldest bound chat and preserve active requests")
	}
	if got, _ := manager.lookupResponseConn("previous", 7, "key"); got == bound {
		t.Fatal("evicted connection retained its continuation binding")
	}
}
