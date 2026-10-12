package wsrelay

import (
	"context"
	"errors"
	"net/http"

	"github.com/codex2api/proxy"
	"github.com/tidwall/gjson"
)

type connectionCapacityKind int32

const (
	capacityChat connectionCapacityKind = iota
	capacityBlank
)

func (kind connectionCapacityKind) String() string {
	if kind == capacityBlank {
		return "blank"
	}
	return "chat"
}

type connectionCapacityContextKey struct{}

func requestConnectionCapacityKind(ctx context.Context) connectionCapacityKind {
	if kind, ok := ctx.Value(connectionCapacityContextKey{}).(connectionCapacityKind); ok {
		return kind
	}
	return capacityChat
}

// 只有没有聊天内容的明确预热使用空白预算；携带上下文仍属于聊天连接。
func withConnectionCapacityKind(ctx context.Context, headers http.Header, body []byte) context.Context {
	kind := capacityChat
	prewarm := gjson.GetBytes(body, "generate").Type == gjson.False || proxy.IsCodexWebsocketPrewarmRequest(headers, body)
	if prewarm && !prewarmHasChatContext(body) {
		kind = capacityBlank
	}
	return context.WithValue(ctx, connectionCapacityContextKey{}, kind)
}

func prewarmHasChatContext(body []byte) bool {
	for _, key := range []string{"input", "instructions", "tools", "prompt", "previous_response_id"} {
		value := gjson.GetBytes(body, key)
		if !value.Exists() || value.Type == gjson.Null {
			continue
		}
		if value.IsArray() && len(value.Array()) == 0 || value.Type == gjson.String && value.String() == "" {
			continue
		}
		return true
	}
	return false
}

func connectionLimit(kind connectionCapacityKind) int {
	if kind == capacityBlank {
		return statelessConnectionSlots()
	}
	return proxy.CurrentRuntimeSettings().CodexWSDownstreamKeepaliveSlots
}

func connectionBudgetKind(wc *WsConnection) connectionCapacityKind {
	if slot := wc.capacitySlot.Load(); slot != nil {
		return connectionCapacityKind(slot.kind.Load())
	}
	if wc.session != nil && wc.session.hasUserContext() {
		return capacityChat
	}
	return capacityBlank
}

func (m *Manager) budgetConnectionCountLocked(accountID int64, kind connectionCapacityKind) int {
	count := 0
	for slot := range m.connectionSlots[accountID] {
		if connectionCapacityKind(slot.kind.Load()) == kind {
			count++
		}
	}
	return count
}

func (m *Manager) budgetConnectionCount(input connectionCapacityRequest) int {
	m.capacityMu.Lock()
	defer m.capacityMu.Unlock()
	return m.budgetConnectionCountLocked(input.accountID, input.kind)
}

// 首次用户使用空白连接前转为聊天连接；推理不受空闲保留额度限制。
func (m *Manager) ensureConnectionBudget(wc *WsConnection, kind connectionCapacityKind) error {
	m.adoptConnectionCapacity(wc)
	if kind == capacityBlank || connectionBudgetKind(wc) == capacityChat {
		return nil
	}
	m.capacityMu.Lock()
	defer m.capacityMu.Unlock()
	slot := wc.capacitySlot.Load()
	if slot.released.Load() || !wc.IsConnected() {
		return errors.New("websocket disconnected before budget transfer")
	}
	slot.kind.Store(int32(capacityChat))
	return nil
}

// 未发送请求就被本地额度拒绝的新连接没有用户上下文，可退回空白预算。
func (m *Manager) releaseUnusedChatCapacity(wc *WsConnection) {
	if wc.session.hasUserContext() || wc.session.PendingCount() != 0 {
		return
	}
	m.capacityMu.Lock()
	slot := wc.capacitySlot.Load()
	keep := slot != nil && !slot.released.Load() && wc.IsConnected()
	if keep && connectionCapacityKind(slot.kind.Load()) == capacityChat {
		keep = m.budgetConnectionCountLocked(slot.accountID, capacityBlank) < connectionLimit(capacityBlank)
		if keep {
			slot.kind.Store(int32(capacityBlank))
		}
	}
	m.capacityMu.Unlock()
	if !keep {
		m.discardConnectionFor(wc, closeCapacityReclaim)
	}
}

func isConnectionCapacityError(err error) bool {
	var capacityErr *proxy.UpstreamWSConnectionCapacityError
	return errors.As(err, &capacityErr)
}

func (m *Manager) discardAcquisitionFailure(wc *WsConnection, err error) {
	if !isConnectionCapacityError(err) {
		m.DiscardConnection(wc)
	}
}
