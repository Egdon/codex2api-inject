package wsrelay

import "sort"

// 调用方持有账号锁。推理、拨号及尚未承载聊天的连接不占空闲保留额度。
func (m *Manager) trimIdleChatConnections(input connectionCapacityRequest) {
	input.kind = capacityChat
	idle := m.idleCapacityConnections(input)
	sort.Slice(idle, func(i, j int) bool {
		return idle[i].lastUsed < idle[j].lastUsed
	})
	excess := len(idle) - input.limit
	for _, candidate := range idle {
		if excess <= 0 {
			return
		}
		if m.discardIdleChatConnection(candidate.wc) {
			excess--
		}
	}
}

// 即使存在续链绑定也可按 LRU 回收；账号锁阻止候选被新推理同时取走。
func (m *Manager) discardIdleChatConnection(wc *WsConnection) bool {
	m.respConnMu.Lock()
	current, exists := m.connections.Load(wc.PoolKey)
	if !exists || current != wc || wc.session.PendingCount() != 0 {
		m.respConnMu.Unlock()
		return false
	}
	wc.responseBindingsDisabled = true
	m.respConnMu.Unlock()
	m.discardConnectionFor(wc, closeIdleChatLRU)
	return true
}

func (m *Manager) retainReleasedConnection(wc *WsConnection) {
	lock, release := m.accountLock(wc.session.AccountID)
	defer release()
	lock.Lock()
	defer lock.Unlock()
	current, exists := m.connections.Load(wc.PoolKey)
	if !exists || current != wc || !wc.IsConnected() {
		return
	}
	if connectionBudgetKind(wc) == capacityBlank && connectionLimit(capacityBlank) == 0 && wc.session.PendingCount() == 0 {
		m.discardConnectionFor(wc, closeCapacityReclaim)
		return
	}
	wc.Touch()
	input := connectionCapacityRequest{accountID: wc.session.AccountID, limit: connectionLimit(capacityChat)}
	m.trimIdleChatConnections(input)
}

// 保存较小的设置后，即使没有新请求，后台巡检也会收缩空闲保留池。
func (m *Manager) trimAllIdleChatConnections() {
	accounts := make(map[int64]struct{})
	m.connections.Range(func(_, value any) bool {
		wc := value.(*WsConnection)
		if wc.session != nil {
			accounts[wc.session.AccountID] = struct{}{}
		}
		return true
	})
	for accountID := range accounts {
		m.trimAccountIdleChatConnections(accountID)
	}
}

func (m *Manager) trimAccountIdleChatConnections(accountID int64) {
	lock, release := m.accountLock(accountID)
	defer release()
	lock.Lock()
	defer lock.Unlock()
	m.trimIdleChatConnections(connectionCapacityRequest{accountID: accountID, limit: connectionLimit(capacityChat)})
	if connectionLimit(capacityBlank) == 0 {
		m.reclaimConnectionCapacity(connectionCapacityRequest{accountID: accountID, kind: capacityBlank})
	}
}
