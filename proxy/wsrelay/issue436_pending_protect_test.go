package wsrelay

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestHeartbeatTickPingFailureKeepsPendingRequests 验证 issue #436 修复：
// 心跳 Ping 写失败时，若会话仍有在途请求，不能 Close 会话（会把全部 pending
// 同秒截断），只停止心跳链，交给读路径裁决。
func TestHeartbeatTickPingFailureKeepsPendingRequests(t *testing.T) {
	s := NewSession(1, nil)
	s.SetConnected(true)
	pr := s.AddPendingRequest("sess-1")

	s.heartbeatTick(func() error { return errors.New("write: broken pipe") })

	if s.PendingCount() != 1 {
		t.Fatalf("pending count = %d, want 1 (ping failure must not kill in-flight requests)", s.PendingCount())
	}
	pr.closeMu.Lock()
	closed := pr.closed
	pr.closeMu.Unlock()
	if closed {
		t.Fatal("pending request was closed by heartbeat ping failure")
	}
	if !s.IsConnected() {
		t.Fatal("session was disconnected by heartbeat ping failure while requests are in flight")
	}
}

// TestHeartbeatTickPingFailureClosesIdleSession 验证空闲会话（无在途请求）的
// 心跳失败仍走原有 Close 语义。
func TestHeartbeatTickPingFailureClosesIdleSession(t *testing.T) {
	s := NewSession(1, nil)
	s.SetConnected(true)

	s.heartbeatTick(func() error { return errors.New("write: broken pipe") })

	if s.IsConnected() {
		t.Fatal("idle session should be closed on heartbeat ping failure")
	}
}

// TestSendHeartbeatBusyConnectionNotClosed 验证 issue #436 修复：
// SendHeartbeat 对带在途请求的连接 Ping 失败时，只把连接摘出池子（阻止新复用），
// 不关闭底层 socket——写路径故障不代表读路径已死，在途流可能仍在正常下发。
func TestSendHeartbeatBusyConnectionNotClosed(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	// 拨通后立即关闭底层连接：后续 WriteControl 必然失败，模拟写路径故障。
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	_ = conn.Close()

	manager := NewManager()
	t.Cleanup(manager.Stop)

	key := manager.poolKey(1, wsURL, "sess-1", "")
	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := NewWsConnection(conn, session, wsURL)
	wc.PoolKey = key
	wc.SetState(StateConnected)
	session.AddPendingRequest("sess-1")
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)

	if err := manager.SendHeartbeat(wc); err == nil {
		t.Fatal("expected ping failure on closed connection")
	}

	// 连接应被摘出池子（阻止复用）
	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("busy connection with failed ping should be removed from pool")
	}
	// 但在途请求不能被杀
	if session.PendingCount() != 1 {
		t.Fatalf("pending count = %d, want 1 (busy connection ping failure must not kill in-flight requests)", session.PendingCount())
	}
	if !session.IsConnected() {
		t.Fatal("session must stay connected for in-flight drain after busy-connection ping failure")
	}
}

// TestSendHeartbeatIdleConnectionDiscarded 验证空闲连接（无在途请求）Ping 失败
// 仍走 DiscardConnection：关 socket + 摘池，语义与修复前一致。
func TestSendHeartbeatIdleConnectionDiscarded(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	_ = conn.Close()

	manager := NewManager()
	t.Cleanup(manager.Stop)

	key := manager.poolKey(1, wsURL, "sess-idle", "")
	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := NewWsConnection(conn, session, wsURL)
	wc.PoolKey = key
	wc.SetState(StateConnected)
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, session)

	if err := manager.SendHeartbeat(wc); err == nil {
		t.Fatal("expected ping failure on closed connection")
	}
	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("idle connection with failed ping should be discarded from pool")
	}
	if session.IsConnected() {
		t.Fatal("idle session should be marked disconnected after discard")
	}
}

// TestEvictExpiredSkipsConnectionsWithPendingRequests 验证 issue #436 修复：
// evictExpired 对带在途请求的连接/会话一律跳过，即使 lastUsed/LastActiveAt
// 已超过 IdleTimeout（长思考、pong 丢失都会造成活跃对象被误判为空闲）。
func TestEvictExpiredSkipsConnectionsWithPendingRequests(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	session := NewSession(1, manager)
	session.SetConnected(true)
	wc := NewWsConnection(nil, session, "ws://example")
	wc.PoolKey = "key-busy"
	wc.SetState(StateConnected)
	wc.Touch()
	// 伪造超龄空闲时间戳：lastUsed 早于 IdleTimeout，LastActiveAt 同步做旧
	wc.lastUsed.Store(time.Now().Add(-2 * IdleTimeout).UnixNano())
	session.mu.Lock()
	session.LastActiveAt = time.Now().Add(-2 * IdleTimeout)
	session.mu.Unlock()
	pr := session.AddPendingRequest("sess-busy")

	manager.connections.Store("key-busy", wc)
	manager.sessions.Store("key-busy", session)

	manager.evictExpired()

	if _, ok := manager.connections.Load("key-busy"); !ok {
		t.Fatal("evictExpired removed a connection with in-flight requests")
	}
	if _, ok := manager.sessions.Load("key-busy"); !ok {
		t.Fatal("evictExpired removed a session with in-flight requests")
	}
	if !wc.IsConnected() {
		t.Fatal("evictExpired closed a connection with in-flight requests")
	}

	// 在途收尾后，同样的过期状态下一轮应被正常清理
	session.RemovePendingRequest(pr.RequestID)
	manager.evictExpired()

	if _, ok := manager.connections.Load("key-busy"); ok {
		t.Fatal("evictExpired should remove the idle-expired connection once pending drained")
	}
	if _, ok := manager.sessions.Load("key-busy"); ok {
		t.Fatal("evictExpired should remove the idle-expired session once pending drained")
	}
}
