package wsrelay

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func signalClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestAccountWaitSignalClosedByPoolChanges(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	const accountID int64 = 42

	first := manager.accountWaitSignal(accountID)
	if signalClosed(first) {
		t.Fatal("fresh wait signal must be open")
	}
	if again := manager.accountWaitSignal(accountID); again != first {
		t.Fatal("waiters registered before any change must share one signal")
	}
	if other := manager.accountWaitSignal(7); other == first {
		t.Fatal("different accounts must not share a wait signal")
	}

	// 在途请求结束 → 唤醒
	session := NewSession(accountID, manager)
	pr := session.AddPendingRequest("session-1")
	session.RemovePendingRequest(pr.RequestID)
	if !signalClosed(first) {
		t.Fatal("RemovePendingRequest did not wake account waiters")
	}
	if signalClosed(manager.accountWaitSignal(7)) {
		t.Fatal("another account's waiters were woken by an unrelated release")
	}

	// 连接销毁 → 唤醒
	second := manager.accountWaitSignal(accountID)
	if second == first {
		t.Fatal("signal must be replaced after it fires")
	}
	discard := &WsConnection{session: NewSession(accountID, manager), PoolKey: "k"}
	manager.DiscardConnection(discard)
	if !signalClosed(second) {
		t.Fatal("DiscardConnection did not wake account waiters")
	}

	// 拨号占位归还 → 唤醒
	third := manager.accountWaitSignal(accountID)
	capacity := manager.reserveAccountConnectionCapacity(connectionCapacityRequest{accountID: accountID, limit: connectionLimit(capacityChat)})
	if capacity == nil {
		t.Fatal("dial capacity reservation failed")
	}
	capacity.release()
	if !signalClosed(third) {
		t.Fatal("capacity release did not wake account waiters")
	}

	// 没有等待者时 notify 是空操作，不应留下条目。
	manager.notifyAccountWaiters(accountID)
	manager.waitNotifyMu.Lock()
	_, lingering := manager.waitNotify[accountID]
	manager.waitNotifyMu.Unlock()
	if lingering {
		t.Fatal("notify without waiters must not leave a map entry")
	}
}

// 被同会话在途请求占住的连接一旦空闲，排队的 acquire 应立即拿到它，
// 而不是等到下一次指数退避到期（封顶 AcquireMaxBackoff）。
func TestAcquireConnectionWakesImmediatelyWhenBusySessionReleases(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(wc *WsConnection) bool { return true }

	account := &auth.Account{DBID: 42}
	wsURL := "wss://example.test/responses"
	busy, blocking := newBusyTestConnection(t, manager, account, wsURL, "session-1")

	type result struct {
		wc  *WsConnection
		pr  *PendingRequest
		err error
		at  time.Time
	}
	done := make(chan result, 1)
	go func() {
		wc, pr, err := manager.AcquireConnection(context.Background(), account, wsURL, "session-1", http.Header{}, "")
		done <- result{wc: wc, pr: pr, err: err, at: time.Now()}
	}()

	// 让等待者进入退避封顶区间（10+20+40+80+160=310ms 后每次等 200ms），
	// 在两次轮询之间释放：无唤醒机制时要再等到下一个 200ms 边界。
	time.Sleep(AcquireMaxBackoff*2 + 160*time.Millisecond)
	releasedAt := time.Now()
	busy.session.RemovePendingRequest(blocking.RequestID)

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("AcquireConnection() error = %v", got.err)
		}
		if got.wc != busy {
			t.Fatal("expected the released connection to be reused")
		}
		if got.pr != nil {
			busy.session.RemovePendingRequest(got.pr.RequestID)
		}
		if lag := got.at.Sub(releasedAt); lag > AcquireMaxBackoff/2 {
			t.Fatalf("acquire woke %s after release, want an immediate wake-up well under the %s backoff cap", lag, AcquireMaxBackoff)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("acquire did not complete after the busy session released")
	}
}
