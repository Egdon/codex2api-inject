package wsrelay

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ==================== 心跳配置常量 ====================

const (
	// 心跳间隔：每 30 秒发送 Ping
	HeartbeatPingInterval = 30 * time.Second

	// 活跃流存活复核间隔：业务帧静默达到 120 秒时不直接断开，而是先检查
	// 最近入站活动，再按需发送带唯一 payload 的 Ping 等待匹配 Pong。
	ReadLivenessCheckInterval = 120 * time.Second

	// 活跃流近期入站窗口：覆盖两个心跳周期，允许偶发一次 Pong 延迟或丢失。
	// 数据帧、对端 Ping、我方 Ping 的 Pong 回执都会刷新入站时间。
	ActiveReadRecentInboundWindow = 2 * HeartbeatPingInterval

	// 活跃流探活超时：仅在既无业务帧、近期也无任何入站活动时触发。
	// 正常请求和有心跳回执的长推理不会产生这次额外往返。
	ActiveReadProbeTimeout = 5 * time.Second

	// 活跃流业务帧静默上限：存活复核证明传输层活着,但证明不了上游还在处理
	// 本请求;单轮响应两个业务帧之间的静默超过该值即按超时收尾,防止 worker
	// 卡死但心跳仍通的连接把请求与租约无限钉死。
	// env CODEX_WS_MAX_TURN_SILENCE 可调("0" 关闭)。
	ActiveReadMaxTurnSilence = 15 * time.Minute

	// 写超时：30 秒
	WriteTimeout = 30 * time.Second

	// 业务空闲超时：30 分钟未处理聊天请求或响应则断开，心跳不续期。
	IdleTimeout = 30 * time.Minute

	// 连接最大寿命：上游 chatgpt backend 对每条 Responses WS 连接强制 60 分钟
	// 寿命上限，超限后该连接上的 response.create 一律返回
	// websocket_connection_limit_reached（且 Ping 探活仍成功，无法靠探活识别）。
	// 提前到 50 分钟在空闲时主动轮转销毁，避免活跃会话撞线（issue #346）。
	MaxConnLifetime = 50 * time.Minute

	// 握手超时：30 秒
	HandshakeTimeout = 30 * time.Second

	// 复用同一 session 的连接时，等待其空闲的轮询退避参数。
	AcquireInitialBackoff = 10 * time.Millisecond  // 初始退避
	AcquireMaxBackoff     = 200 * time.Millisecond // 退避封顶
	AcquireMaxWait        = 30 * time.Second       // 最大累计等待，超时返回错误
)

// ==================== Pending 请求管理 ====================

// PendingRequest 等待响应的请求
type PendingRequest struct {
	// 请求 ID
	RequestID string

	// 会话 ID
	SessionID string

	// 创建时间
	CreatedAt time.Time

	// 响应通道
	ResponseChan chan *Message

	// 流式数据通道
	StreamChan chan *Message

	// 上下文（用于取消）
	Ctx context.Context

	// 取消函数
	Cancel context.CancelFunc

	// 关闭标志，防止重复关闭
	closed  bool
	closeMu sync.Mutex
}

// NewPendingRequest 创建新的等待请求
func NewPendingRequest(sessionID string) *PendingRequest {
	// Ctx 仅用于 Close 时的取消广播;请求时长的应用层上限由读路径的业务帧
	// 静默上限(ActiveReadMaxTurnSilence)执行。历史上这里的 2min WithTimeout
	// 从无消费者,却让"Pending 有 2 分钟兜底"的注释在别处流传,故改为 WithCancel。
	ctx, cancel := context.WithCancel(context.Background())
	return &PendingRequest{
		RequestID:    uuid.New().String(),
		SessionID:    sessionID,
		CreatedAt:    time.Now(),
		ResponseChan: make(chan *Message, 1),
		StreamChan:   make(chan *Message, 64), // 流式数据缓冲
		Ctx:          ctx,
		Cancel:       cancel,
	}
}

// Close 关闭请求，释放资源（幂等）
func (pr *PendingRequest) Close() {
	pr.closeMu.Lock()
	defer pr.closeMu.Unlock()

	if pr.closed {
		return
	}
	pr.closed = true

	pr.Cancel()
	close(pr.ResponseChan)
	close(pr.StreamChan)
}

// ==================== 会话管理 ====================

// Session WebSocket 会话
type Session struct {
	// 会话 ID
	ID string

	// 账号 ID
	AccountID int64

	// 创建时间
	CreatedAt time.Time

	// 最后活跃时间
	LastActiveAt time.Time

	// 曾用于用户聊天；续链绑定过期后仍保留，不能降回空白预热槽位。
	userContext bool

	// 连接状态
	Connected bool

	// 读写锁保护内部状态
	mu sync.RWMutex

	// Pending 请求映射（requestID -> *PendingRequest）
	pending sync.Map

	// 心跳计时器
	heartbeatTimer *time.Timer

	// 连接关闭回调
	onClose func()

	// 会话管理器引用
	manager *Manager
}

// NewSession 创建新会话
func NewSession(accountID int64, manager *Manager) *Session {
	return &Session{
		ID:           uuid.New().String(),
		AccountID:    accountID,
		CreatedAt:    time.Now(),
		LastActiveAt: time.Now(),
		Connected:    false,
		manager:      manager,
	}
}

// SetConnected 设置连接状态
func (s *Session) SetConnected(connected bool) {
	s.mu.Lock()
	s.Connected = connected
	if connected {
		s.LastActiveAt = time.Now()
	}
	s.mu.Unlock()
}

// IsConnected 检查是否已连接
func (s *Session) IsConnected() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Connected
}

// AddPendingRequest 添加等待请求
func (s *Session) AddPendingRequest(sessionID string) *PendingRequest {
	pr := NewPendingRequest(sessionID)
	s.pending.Store(pr.RequestID, pr)
	return pr
}

// GetPendingRequest 获取等待请求
func (s *Session) GetPendingRequest(requestID string) (*PendingRequest, bool) {
	if v, ok := s.pending.Load(requestID); ok {
		return v.(*PendingRequest), true
	}
	return nil, false
}

// RemovePendingRequest 移除等待请求
func (s *Session) RemovePendingRequest(requestID string) {
	if v, ok := s.pending.LoadAndDelete(requestID); ok {
		pr := v.(*PendingRequest)
		pr.Close()
		// 在途请求结束：唤醒等在这条连接上的同会话 acquire，不必等退避到期。
		if s.manager != nil {
			s.manager.notifyAccountWaiters(s.AccountID)
		}
	}
}

// PendingCount returns the number of in-flight requests bound to this session.
func (s *Session) PendingCount() int {
	if s == nil {
		return 0
	}
	count := 0
	s.pending.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

// DeliverResponse 投递响应到等待请求
func (s *Session) DeliverResponse(msg *Message) bool {
	if pr, ok := s.GetPendingRequest(msg.RequestID); ok {
		pr.closeMu.Lock()
		if pr.closed {
			pr.closeMu.Unlock()
			return false
		}
		select {
		case pr.ResponseChan <- msg:
			pr.closeMu.Unlock()
			return true
		default:
			// 通道已满或已关闭
			pr.closeMu.Unlock()
			return false
		}
	}
	return false
}

// DeliverStreamChunk 投递流式数据块
func (s *Session) DeliverStreamChunk(msg *Message) bool {
	if pr, ok := s.GetPendingRequest(msg.RequestID); ok {
		pr.closeMu.Lock()
		if pr.closed {
			pr.closeMu.Unlock()
			return false
		}
		select {
		case pr.StreamChan <- msg:
			pr.closeMu.Unlock()
			return true
		default:
			// 通道已满，丢弃当前流式数据块并返回 false
			log.Printf("DeliverStreamChunk stream channel full: account=%d session=%s requestID=%s capacity=%d length=%d; dropping current chunk", s.AccountID, s.ID, msg.RequestID, cap(pr.StreamChan), len(pr.StreamChan))
			pr.closeMu.Unlock()
			return false
		}
	}
	return false
}

// StartHeartbeat 启动心跳（防重入）
func (s *Session) StartHeartbeat(sendPing func() error) {
	s.mu.Lock()
	// 防重入：如果已有 timer 则直接返回
	if s.heartbeatTimer != nil {
		s.mu.Unlock()
		return
	}
	s.heartbeatTimer = time.AfterFunc(HeartbeatPingInterval, func() {
		s.heartbeatTick(sendPing)
	})
	s.mu.Unlock()
}

// heartbeatTick 心跳定时器的单次触发逻辑。
func (s *Session) heartbeatTick(sendPing func() error) {
	s.mu.RLock()
	connected := s.Connected
	s.mu.RUnlock()

	if !connected {
		return
	}

	// 弱网模式不靠心跳无限续住空闲连接；没有在途请求时停止心跳，
	// 让短空闲窗口把连接自然轮换掉。在途请求仍保留原心跳保护。
	if weakNetworkModeEnabled() && s.PendingCount() == 0 {
		s.StopHeartbeat()
		return
	}

	// 发送 Ping
	if err := sendPing(); err != nil {
		// Ping 写失败只说明写路径故障，读路径可能仍在正常下发；有在途请求时
		// 不能 Close（会把本会话全部 pending 同秒截断，issue #436），交给读路径
		// 裁决：pump 读错误或 ReadMessage 存活复核失败会走各自的失败处理。连接本身已被
		// SendHeartbeat 摘出池子，不会再有新请求进来；心跳链就此停止。
		if s.PendingCount() > 0 {
			s.StopHeartbeat()
			return
		}
		s.Close()
		return
	}

	// 检查 timer 是否仍存在（可能已被 StopHeartbeat 清除）
	s.mu.Lock()
	timer := s.heartbeatTimer
	s.mu.Unlock()

	// 安全重置计时器
	if timer != nil {
		timer.Reset(HeartbeatPingInterval)
	}
}

// StopHeartbeat 停止心跳
func (s *Session) StopHeartbeat() {
	s.mu.Lock()
	if s.heartbeatTimer != nil {
		s.heartbeatTimer.Stop()
		s.heartbeatTimer = nil
	}
	s.mu.Unlock()
}

// Close 关闭会话
func (s *Session) Close() {
	s.StopHeartbeat()
	s.SetConnected(false)

	// 关闭所有等待请求
	s.pending.Range(func(key, value any) bool {
		pr := value.(*PendingRequest)
		pr.Close()
		s.pending.Delete(key)
		return true
	})

	// 调用关闭回调
	if s.onClose != nil {
		s.onClose()
	}
}

// SetOnClose 设置关闭回调
func (s *Session) SetOnClose(fn func()) {
	s.mu.Lock()
	s.onClose = fn
	s.mu.Unlock()
}

// ClearPendingRequests 清理所有等待请求
func (s *Session) ClearPendingRequests() {
	s.pending.Range(func(key, value any) bool {
		pr := value.(*PendingRequest)
		// 发送错误响应
		errMsg := NewErrorMessage(pr.RequestID, 503, "session closed")
		select {
		case pr.ResponseChan <- errMsg:
		default:
		}
		pr.Close()
		s.pending.Delete(key)
		return true
	})
}
