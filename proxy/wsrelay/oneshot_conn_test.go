package wsrelay

import (
	"testing"
	"time"
)

// 一次性池键连接（stateless fallback）不应在容量裁剪时挤掉可复用的槽位连接，
// 也不应在无续链绑定时归还池占位（issue #520）。

func newPooledConn(t *testing.T, manager *Manager, accountID int64, sessionID, poolKey string, lastUsed time.Time) *WsConnection {
	t.Helper()
	session := NewSession(accountID, manager)
	session.ID = sessionID
	session.SetConnected(true)
	session.markUserContext()
	conn := &WsConnection{session: session, URL: "wss://example.test/responses", PoolKey: poolKey}
	conn.SetState(StateConnected)
	conn.lastUsed.Store(lastUsed.UnixNano())
	manager.connections.Store(poolKey, conn)
	manager.sessions.Store(poolKey, session)
	return conn
}

func TestSortIdleForEvictionPrefersOneShotConns(t *testing.T) {
	slotOld := &WsConnection{session: &Session{ID: "det-key#0"}}
	slotMid := &WsConnection{session: &Session{ID: "det-key#1"}}
	oneShot := &WsConnection{session: &Session{ID: "stateless-11111111-aaaa-bbbb-cccc-000000000001"}}

	idle := []idleAccountConnection{
		{wc: slotOld, lastUsed: 100},
		{wc: oneShot, lastUsed: 300}, // 最新使用，但池键一次性
		{wc: slotMid, lastUsed: 200},
	}
	sortIdleForEviction(idle)

	if idle[0].wc != oneShot {
		t.Fatal("expected one-shot connection to be first eviction candidate despite newest lastUsed")
	}
	if idle[1].wc != slotOld || idle[2].wc != slotMid {
		t.Fatal("expected reusable slot connections ordered by LRU after one-shot conns")
	}
}

func TestTrimIdleChatConnectionsEvictsOldestFirst(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	now := time.Now()
	slotOldKey := manager.poolKey(1, "wss://example.test/responses", "det-key#0", "")
	slotMidKey := manager.poolKey(1, "wss://example.test/responses", "det-key#1", "")
	oneShotKey := manager.poolKey(1, "wss://example.test/responses", "stateless-11111111-aaaa-bbbb-cccc-000000000002", "")

	newPooledConn(t, manager, 1, "det-key#0", slotOldKey, now.Add(-2*time.Minute))
	newPooledConn(t, manager, 1, "det-key#1", slotMidKey, now.Add(-1*time.Minute))
	newPooledConn(t, manager, 1, "stateless-11111111-aaaa-bbbb-cccc-000000000002", oneShotKey, now)

	manager.trimIdleAccountConnections(1, 2, nil)

	if _, ok := manager.connections.Load(oneShotKey); !ok {
		t.Fatal("newest one-shot chat should remain retained")
	}
	if _, ok := manager.connections.Load(slotOldKey); ok {
		t.Fatal("oldest chat should be evicted regardless of its pool key")
	}
	if _, ok := manager.connections.Load(slotMidKey); !ok {
		t.Fatal("expected newer slot connection to survive")
	}
}

func TestChatInferenceReservationDoesNotConsumeIdleRetention(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	now := time.Now()
	slotKey := manager.poolKey(1, "wss://example.test/responses", "det-key#0", "")
	oneShotKey := manager.poolKey(1, "wss://example.test/responses", "stateless-11111111-aaaa-bbbb-cccc-000000000003", "")

	newPooledConn(t, manager, 1, "det-key#0", slotKey, now.Add(-2*time.Minute))
	newPooledConn(t, manager, 1, "stateless-11111111-aaaa-bbbb-cccc-000000000003", oneShotKey, now)

	// 两条空闲连接未超保留上限，新增推理不要求它们让位。
	if !manager.ensureAccountConnectionCapacity(connectionCapacityRequest{accountID: 1, limit: 3, extraPending: 1}) {
		t.Fatal("inference should start without evicting retained idle connections")
	}
	if _, ok := manager.connections.Load(oneShotKey); !ok {
		t.Fatal("inference admission should not consume idle retention")
	}
	if _, ok := manager.connections.Load(slotKey); !ok {
		t.Fatal("expected reusable slot connection to survive")
	}
}

func TestCloseDiscardsUnboundOneShotConn(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	sessionID := "stateless-11111111-aaaa-bbbb-cccc-000000000004"
	key := manager.poolKey(1, "wss://example.test/responses", sessionID, "")
	conn := newPooledConn(t, manager, 1, sessionID, key, time.Now())

	resp := &WsResponse{conn: conn, manager: manager, sessionID: sessionID, apiKey: "key-1"}
	resp.markStreamCompleted()
	if err := resp.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("expected unbound one-shot connection to be discarded on release")
	}
	if conn.IsConnected() {
		t.Fatal("expected discarded connection to be closed")
	}
}

func TestCloseKeepsBoundOneShotConn(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	sessionID := "stateless-11111111-aaaa-bbbb-cccc-000000000005"
	key := manager.poolKey(1, "wss://example.test/responses", sessionID, "")
	conn := newPooledConn(t, manager, 1, sessionID, key, time.Now())
	// 续链绑定存活：连接是该 response_id 上下文的唯一载体，必须归还池。
	manager.BindResponseConn("resp_bind_1", conn, sessionID, 1, "key-1")

	resp := &WsResponse{conn: conn, manager: manager, sessionID: sessionID, apiKey: "key-1"}
	resp.markStreamCompleted()
	if err := resp.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if _, ok := manager.connections.Load(key); !ok {
		t.Fatal("expected one-shot connection with live response binding to stay pooled")
	}
	if !conn.IsConnected() {
		t.Fatal("expected pooled connection to remain connected")
	}
}

func TestCloseReleasesKeyedSessionConn(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	sessionID := "session-keyed-1"
	key := manager.poolKey(1, "wss://example.test/responses", sessionID, "")
	conn := newPooledConn(t, manager, 1, sessionID, key, time.Now())

	resp := &WsResponse{conn: conn, manager: manager, sessionID: sessionID, apiKey: "key-1"}
	resp.markStreamCompleted()
	if err := resp.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if _, ok := manager.connections.Load(key); !ok {
		t.Fatal("expected keyed session connection to be released back to pool")
	}
}
