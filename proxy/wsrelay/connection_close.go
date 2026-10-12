package wsrelay

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
)

const (
	closeCapacityReclaim  = "capacity_reclaim"
	closeIdleChatLRU      = "idle_chat_lru"
	closeIdleTimeout      = "idle_timeout"
	closeLifetimeRotation = "lifetime_rotation"
	closeDisconnected     = "disconnected"
	closeManagerShutdown  = "manager_shutdown"
	closeProbeFailed      = "probe_failed"
	closePoolRemoved      = "pool_removed"
	closeUnusable         = "unusable_connection"
	closeUnconsumed       = "unconsumed_response"
	closeOneShotComplete  = "unbound_oneshot_complete"
)

type connectionCloseCause struct {
	source string
	reason string
	code   int
	bound  bool
}

func (wc *WsConnection) recordCloseCause(cause connectionCloseCause) {
	wc.closeCause.CompareAndSwap(nil, &cause)
}

func (wc *WsConnection) recordDirectClose() {
	if wc.closeCause.Load() != nil {
		return
	}
	cause := connectionCloseCause{source: "local", reason: "explicit_close"}
	if slot := wc.capacitySlot.Load(); slot != nil {
		cause.bound = slot.manager.hasLiveResponseBinding(wc)
	}
	wc.recordCloseCause(cause)
}

func (m *Manager) discardConnectionFor(wc *WsConnection, reason string) {
	if wc == nil {
		return
	}
	wc.recordCloseCause(connectionCloseCause{source: "local", reason: reason, bound: m.hasLiveResponseBinding(wc)})
	m.DiscardConnection(wc)
}

func connectionExpiryReason(wc *WsConnection) string {
	if !wc.IsConnected() {
		return closeDisconnected
	}
	if wc.IsExpired() {
		return closeIdleTimeout
	}
	return closeLifetimeRotation
}

func (wc *WsConnection) recordReadClose(err error) {
	cause := connectionCloseCause{source: "network", reason: "read_failure"}
	var peer *websocket.CloseError
	if errors.As(err, &peer) {
		cause.code = peer.Code
		if peer.Code != websocket.CloseAbnormalClosure && peer.Code != websocket.CloseNoStatusReceived && peer.Code != websocket.CloseTLSHandshake {
			cause.source, cause.reason = "upstream", "peer_close"
		}
	}
	if slot := wc.capacitySlot.Load(); slot != nil {
		cause.bound = slot.manager.hasLiveResponseBinding(wc)
	}
	wc.recordCloseCause(cause)
}

func (wc *WsConnection) logClose() {
	cause := wc.closeCause.Load()
	if cause == nil {
		cause = &connectionCloseCause{source: "local", reason: "explicit_close"}
	}
	accountID, pending, remaining := int64(0), 0, 0
	if wc.session != nil {
		accountID, pending = wc.session.AccountID, wc.session.PendingCount()
	}
	if slot := wc.capacitySlot.Load(); slot != nil {
		remaining = slot.manager.accountConnectionCount(accountID)
	}
	age, idle := time.Duration(0), time.Duration(0)
	if wc.createdAt > 0 {
		age = time.Since(time.Unix(0, wc.createdAt))
	}
	if used := wc.lastUsed.Load(); used > 0 {
		idle = time.Since(time.Unix(0, used))
	}
	log.Printf("[WS] 连接关闭 account=%d connection=%p source=%s reason=%s code=%d bound=%t pending=%d age_ms=%d idle_ms=%d connections=%d chat_idle_limit=%d blank_limit=%d",
		accountID, wc, cause.source, cause.reason, cause.code, cause.bound, pending, age.Milliseconds(), idle.Milliseconds(), remaining, connectionLimit(capacityChat), connectionLimit(capacityBlank))
}

func (m *Manager) connectionCapacityError(accountID int64, kind connectionCapacityKind) error {
	select {
	case <-m.stopCleanup:
		return context.Canceled
	default:
	}
	limit := connectionLimit(kind)
	log.Printf("[WS] 新建连接容量拒绝 account=%d budget=%s connections=%d limit=%d", accountID, kind.String(), m.budgetConnectionCount(connectionCapacityRequest{accountID: accountID, kind: kind}), limit)
	return &proxy.UpstreamWSConnectionCapacityError{Limit: limit}
}
