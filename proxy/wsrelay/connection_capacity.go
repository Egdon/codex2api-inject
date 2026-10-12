package wsrelay

import (
	"sort"
	"sync/atomic"
	"time"

	"github.com/codex2api/proxy"
)

type idleAccountConnection struct {
	wc       *WsConnection
	lastUsed int64
}

func isOneShotPoolConn(wc *WsConnection) bool {
	return wc != nil && wc.session != nil && proxy.IsStatelessWebsocketSessionID(wc.session.ID)
}

// 无绑定的一次性连接先回收，其余按业务使用时间排序。
func sortIdleForEviction(idle []idleAccountConnection) {
	sort.Slice(idle, func(i, j int) bool {
		one, other := isOneShotPoolConn(idle[i].wc), isOneShotPoolConn(idle[j].wc)
		if one != other {
			return one
		}
		return idle[i].lastUsed < idle[j].lastUsed
	})
}

func (m *Manager) hasLiveResponseBinding(wc *WsConnection) bool {
	if m == nil || wc == nil {
		return false
	}
	m.respConnMu.Lock()
	defer m.respConnMu.Unlock()
	return m.hasLiveResponseBindingLocked(wc)
}

func (m *Manager) hasLiveResponseBindingLocked(wc *WsConnection) bool {
	if wc.session != nil && !wc.session.hasUserContext() {
		return false
	}
	now := time.Now()
	for _, binding := range m.respConnBindings {
		if binding.conn == wc && now.Before(binding.expiresAt) {
			return true
		}
	}
	return false
}

// 实体连接登记覆盖拨号、入池和摘池后的收尾，直到 socket 实际关闭。
type connectionCapacitySlot struct {
	manager   *Manager
	accountID int64
	kind      atomic.Int32
	released  atomic.Bool
}

func (slot *connectionCapacitySlot) release() {
	if slot == nil || !slot.released.CompareAndSwap(false, true) {
		return
	}
	m := slot.manager
	m.capacityMu.Lock()
	delete(m.connectionSlots[slot.accountID], slot)
	if len(m.connectionSlots[slot.accountID]) == 0 {
		delete(m.connectionSlots, slot.accountID)
	}
	m.capacityMu.Unlock()
	m.notifyAccountWaiters(slot.accountID)
}

func (slot *connectionCapacitySlot) attach(wc *WsConnection) {
	m := slot.manager
	m.capacityMu.Lock()
	wc.capacitySlot.Store(slot)
	m.connectionSlots[slot.accountID][slot] = wc
	m.capacityMu.Unlock()
	if wc.state.Load() == int32(StateDisconnected) {
		slot.release()
	}
}

func (m *Manager) newCapacitySlotLocked(accountID int64) *connectionCapacitySlot {
	if m.connectionSlots == nil {
		m.connectionSlots = make(map[int64]map[*connectionCapacitySlot]*WsConnection)
	}
	if m.connectionSlots[accountID] == nil {
		m.connectionSlots[accountID] = make(map[*connectionCapacitySlot]*WsConnection)
	}
	slot := &connectionCapacitySlot{manager: m, accountID: accountID}
	m.connectionSlots[accountID][slot] = nil
	return slot
}

// 兼容已存在于池中的连接；生产拨号在永久 reader 启动前完成登记。
func (m *Manager) adoptConnectionCapacity(wc *WsConnection) {
	kind := connectionBudgetKind(wc)
	m.capacityMu.Lock()
	if wc.capacitySlot.Load() != nil {
		m.capacityMu.Unlock()
		return
	}
	slot := m.newCapacitySlotLocked(wc.session.AccountID)
	slot.kind.Store(int32(kind))
	wc.capacitySlot.Store(slot)
	m.connectionSlots[slot.accountID][slot] = wc
	m.capacityMu.Unlock()
	if wc.state.Load() == int32(StateDisconnected) {
		slot.release()
	}
}

func (m *Manager) accountConnectionCount(accountID int64) int {
	m.capacityMu.Lock()
	defer m.capacityMu.Unlock()
	return len(m.connectionSlots[accountID])
}

type connectionCapacityRequest struct {
	accountID    int64
	limit        int
	protectedKey string
	extraPending int
	freeSlots    int
	kind         connectionCapacityKind
}

func (m *Manager) idleCapacityConnections(input connectionCapacityRequest) []idleAccountConnection {
	var idle []idleAccountConnection
	m.connections.Range(func(_, value any) bool {
		wc, ok := value.(*WsConnection)
		if !ok || wc == nil || wc.session == nil || wc.session.AccountID != input.accountID {
			return true
		}
		m.adoptConnectionCapacity(wc)
		if connectionBudgetKind(wc) != input.kind {
			return true
		}
		if wc.session.PendingCount() != 0 {
			return true
		}
		if input.kind == capacityChat && !wc.session.hasUserContext() {
			return true
		}
		if !wc.IsConnected() || wc.IsExpired() || wc.IsOverAge() {
			m.discardConnectionFor(wc, connectionExpiryReason(wc))
			return true
		}
		if input.kind == capacityChat || wc.PoolKey != input.protectedKey && !m.hasLiveResponseBinding(wc) {
			idle = append(idle, idleAccountConnection{wc: wc, lastUsed: wc.lastUsed.Load()})
		}
		return true
	})
	return idle
}

// 绑定检查与禁止晚到绑定原子完成，避免终态绑定和容量回收交错。
func (m *Manager) discardUnboundIdleConnection(wc *WsConnection) {
	m.respConnMu.Lock()
	if wc.session.PendingCount() != 0 || m.hasLiveResponseBindingLocked(wc) {
		m.respConnMu.Unlock()
		return
	}
	current, exists := m.connections.Load(wc.PoolKey)
	if !exists || current != wc {
		m.respConnMu.Unlock()
		return
	}
	wc.responseBindingsDisabled = true
	m.respConnMu.Unlock()
	m.discardConnectionFor(wc, closeCapacityReclaim)
}

func (m *Manager) reclaimConnectionCapacity(input connectionCapacityRequest) bool {
	if input.limit < 0 {
		input.limit = 0
	}
	if input.kind == capacityChat {
		m.trimIdleChatConnections(input)
		return true
	}
	idle := m.idleCapacityConnections(input)
	sortIdleForEviction(idle)
	for _, candidate := range idle {
		if input.limit > 0 && m.budgetConnectionCount(input)+input.extraPending+input.freeSlots <= input.limit {
			return true
		}
		m.discardUnboundIdleConnection(candidate.wc)
	}
	// 0 关闭空白保留池，但仍允许请求临时拨号；收尾时关闭连接。
	return input.limit == 0 || m.budgetConnectionCount(input)+input.extraPending+input.freeSlots <= input.limit
}

func (m *Manager) ensureAccountConnectionCapacity(input connectionCapacityRequest) bool {
	input.freeSlots = 1
	return m.reclaimConnectionCapacity(input)
}

func (m *Manager) trimIdleAccountConnections(accountID int64, limit int, protected *WsConnection) {
	input := connectionCapacityRequest{accountID: accountID, limit: limit}
	if protected != nil {
		input.protectedKey = protected.PoolKey
		input.kind = connectionBudgetKind(protected)
	}
	m.reclaimConnectionCapacity(input)
}

// 调用方持有账号锁；聊天拨号只登记实体连接，空白拨号仍检查原预算。
func (m *Manager) reserveAccountConnectionCapacity(input connectionCapacityRequest) *connectionCapacitySlot {
	if !m.ensureAccountConnectionCapacity(input) {
		return nil
	}
	m.capacityMu.Lock()
	defer m.capacityMu.Unlock()
	select {
	case <-m.stopCleanup:
		return nil
	default:
		slot := m.newCapacitySlotLocked(input.accountID)
		slot.kind.Store(int32(input.kind))
		return slot
	}
}

func (m *Manager) closeCapacityConnections() {
	var connections []*WsConnection
	m.capacityMu.Lock()
	for _, slots := range m.connectionSlots {
		for _, wc := range slots {
			if wc != nil {
				connections = append(connections, wc)
			}
		}
	}
	m.capacityMu.Unlock()
	for _, wc := range connections {
		m.discardConnectionFor(wc, closeManagerShutdown)
	}
}
