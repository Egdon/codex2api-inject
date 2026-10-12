package wsrelay

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/security"
	"github.com/gorilla/websocket"
)

// ==================== 连接池管理器 ====================

// ConnectionState 连接状态
type ConnectionState int32

const (
	StateDisconnected ConnectionState = 0
	StateConnecting   ConnectionState = 1
	StateConnected    ConnectionState = 2
	StateClosing      ConnectionState = 3
)

// WsConnection WebSocket 连接包装
type WsConnection struct {
	// WebSocket 连接
	conn *websocket.Conn

	// 握手时实际发送给上游的 User-Agent。连接复用时每个请求沿用该值，
	// 不能用当前配置重新推导，否则设置变更后会记录并未发送的 UA。
	upstreamUserAgent      string
	upstreamUserAgentKnown bool
	upstreamClientIdentity string

	// 创建/复用该连接的账号。请求并发由调度层独立管理，
	// response_id 复用按共享连接槽位预算收敛无绑定的空闲连接。
	account *auth.Account

	// 会话
	session *Session

	// 连接 URL
	URL string

	// 连接池键
	PoolKey string

	// 连接状态
	state atomic.Int32

	capacitySlot atomic.Pointer[connectionCapacitySlot]
	closeCause   atomic.Pointer[connectionCloseCause]

	// 最后业务使用时间，心跳和空闲元数据不续期。
	lastUsed atomic.Int64

	// 最近入站活动时间（数据帧/对端 Ping/Pong 回执，UnixNano）。仅供 probe
	// 免往返判断：近期有入站即 TCP 双向可证活。0 表示尚无入站，probe 走完整
	// 往返。刻意不并入 lastUsed，避免对端 Ping 顺带延长空闲逐出。
	lastInbound atomic.Int64

	// 创建时间（UnixNano），用于连接年龄判断（上游有 60 分钟连接寿命上限）。
	// 构造后不再修改；为 0 表示未知（测试用字面量构造），视为未到龄。
	createdAt int64

	// 写操作锁
	writeMu sync.Mutex

	// 永久 reader、业务帧 lease 与探活状态。读取状态按需初始化，兼容测试中
	// 通过字面量构造且没有底层 socket 的 WsConnection。
	readStateOnce       sync.Once
	readPumpOnce        sync.Once
	readFailureOnce     sync.Once
	controlHandlersOnce sync.Once
	readState           *wsReadState

	probeGateOnce sync.Once
	probeGate     chan struct{}
	probeStateMu  sync.Mutex
	probePayload  string
	probeResult   chan struct{}

	// 底层 socket 与断开回调只关闭/调用一次。
	closeOnce          sync.Once
	closeErr           error
	disconnectNotified atomic.Bool

	// HTTP 握手响应
	httpResp *http.Response

	// 连接关闭回调
	onDisconnected func(accountID int64)

	// 永久 reader 失败回调。Manager 使用指针级 CompareAndDelete 精确移除
	// 当前连接，避免误删同 PoolKey 下已经重建的连接。
	onReadFailure func(wc *WsConnection)

	// Protected by the owning Manager's respConnMu. Once removed from the pool,
	// late completion callbacks must never retain this connection again.
	responseBindingsDisabled bool
}

func effectiveProxyURL(account *auth.Account, proxyOverride string) string {
	proxyURL := ""
	if account != nil {
		account.Mu().RLock()
		proxyURL = account.ProxyURL
		account.Mu().RUnlock()
	}
	if strings.TrimSpace(proxyOverride) != "" {
		proxyURL = proxyOverride
	}
	return strings.TrimSpace(proxyURL)
}

// configureWebsocketDialerProxy applies the configured proxy to a Gorilla
// WebSocket dialer. Gorilla accepts "socks5" but not the curl-style
// "socks5h" alias. Its SOCKS5 implementation already sends domain names to
// the proxy, so normalizing the alias preserves remote DNS resolution.
func configureWebsocketDialerProxy(dialer *websocket.Dialer, rawProxyURL string) error {
	parsed, err := security.ParseProxyURL(rawProxyURL)
	if err != nil {
		return fmt.Errorf("parse proxy URL failed: %w", err)
	}

	parsed.Scheme = strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if parsed.Scheme == "socks5h" {
		parsed.Scheme = "socks5"
	}
	dialer.Proxy = http.ProxyURL(parsed)
	return nil
}

// NewWsConnection 创建 WebSocket 连接
func NewWsConnection(conn *websocket.Conn, session *Session, wsURL string) *WsConnection {
	wc := &WsConnection{
		conn:      conn,
		session:   session,
		URL:       wsURL,
		createdAt: time.Now().UnixNano(),
	}
	wc.lastUsed.Store(time.Now().UnixNano())
	wc.state.Store(int32(StateConnected))
	return wc
}

// Touch 同步更新连接和会话的最后业务使用时间。
func (wc *WsConnection) Touch() {
	wc.lastUsed.Store(time.Now().UnixNano())
	if wc.session != nil {
		slot := wc.capacitySlot.Load()
		wc.session.touchActivity(slot == nil || connectionCapacityKind(slot.kind.Load()) == capacityChat)
	}
}

// touchInbound 记录一次入站活动（数据帧/对端 Ping/我方 Ping 的 Pong 回执）。
func (wc *WsConnection) touchInbound() {
	wc.lastInbound.Store(time.Now().UnixNano())
}

// recentInboundWithin 最近 window 内是否有入站活动。
func (wc *WsConnection) recentInboundWithin(window time.Duration) bool {
	ts := wc.lastInbound.Load()
	if ts == 0 {
		return false
	}
	return time.Since(time.Unix(0, ts)) <= window
}

// IsExpired 只对曾用于聊天的连接执行业务空闲回收；空白槽位正常保活。
func (wc *WsConnection) IsExpired() bool {
	if !weakNetworkModeEnabled() && (wc.session == nil || !wc.session.hasUserContext()) {
		return false
	}
	lastUsed := time.Unix(0, wc.lastUsed.Load())
	return time.Since(lastUsed) >= connectionIdleTimeout()
}

// IsOverAge 检查连接是否超过当前模式的最大寿命。到龄连接不能再接新请求：
// 默认模式提前规避上游 60 分钟硬限制；弱网模式使用更短窗口主动轮换。
func (wc *WsConnection) IsOverAge() bool {
	if wc.createdAt == 0 {
		return false
	}
	return time.Since(time.Unix(0, wc.createdAt)) > connectionMaxLifetime()
}

// IsConnected 检查是否已连接
func (wc *WsConnection) IsConnected() bool {
	return wc.state.Load() == int32(StateConnected)
}

// Close 安全关闭连接
func (wc *WsConnection) Close() error {
	if wc == nil {
		return nil
	}
	wc.closeOnce.Do(func() {
		wc.recordDirectClose()
		wc.state.Store(int32(StateClosing))
		if wc.conn != nil {
			wc.closeErr = wc.conn.Close()
		}
		wc.state.Store(int32(StateDisconnected))
		wc.capacitySlot.Load().release()
		wc.logClose()
	})
	if wc.onDisconnected != nil && wc.session != nil && wc.disconnectNotified.CompareAndSwap(false, true) {
		wc.onDisconnected(wc.session.AccountID)
	}
	return wc.closeErr
}

// SetState 设置连接状态
func (wc *WsConnection) SetState(state ConnectionState) {
	wc.state.Store(int32(state))
}

// WriteMessage 安全写入消息
func (wc *WsConnection) WriteMessage(messageType int, data []byte) error {
	wc.writeMu.Lock()
	defer wc.writeMu.Unlock()

	if !wc.IsConnected() || wc.conn == nil {
		return fmt.Errorf("websocket connection is not connected")
	}
	leaseID, tracksLease, err := wc.beginReadLeaseWrite(messageType)
	if err != nil {
		return err
	}

	wc.conn.SetWriteDeadline(time.Now().Add(WriteTimeout))
	defer wc.conn.SetWriteDeadline(time.Time{})

	writeErr := wc.conn.WriteMessage(messageType, data)
	if writeErr == nil && (messageType == websocket.TextMessage || messageType == websocket.BinaryMessage) {
		wc.Touch()
	}
	if tracksLease {
		return wc.completeReadLeaseWrite(leaseID, writeErr)
	}
	return writeErr
}

// HTTPResponse 返回 HTTP 握手响应
func (wc *WsConnection) HTTPResponse() *http.Response {
	return wc.httpResp
}

// ==================== 连接池管理器 ====================

// Manager WebSocket 连接池管理器
type Manager struct {
	// 连接池（accountID -> *WsConnection）
	connections sync.Map

	// 会话池（accountID -> *Session）
	sessions sync.Map

	// 拨号器配置
	dialer *websocket.Dialer

	// 清理定时器
	cleanupTicker *time.Ticker
	stopCleanup   chan struct{}
	stopOnce      sync.Once

	// 连接回调
	onConnected    func(accountID int64, session *Session)
	onDisconnected func(accountID int64)

	// 读写锁保护回调设置
	mu sync.RWMutex

	// pool key 级别串行化，避免同一逻辑 session 在 acquire 阶段竞争同一条连接
	keyLocks refCountedLockRegistry[string]
	// 账号级串行化连接获取，确保跨 session/pool key 创建连接时仍能严格执行
	// 每账号连接上限，避免大量短会话各留一条空闲连接。
	accountLocks    refCountedLockRegistry[int64]
	capacityMu      sync.Mutex
	connectionSlots map[int64]map[*connectionCapacitySlot]*WsConnection

	// 账号级等待唤醒：busy/容量等待者在同账号连接释放、销毁、新建或在途请求结束时
	// 立即被唤醒重新选连，不再空等整个退避间隔（最多 AcquireMaxBackoff）。
	// 每次 notify 关闭并替换当前 channel；只有等待者存在时才有条目。
	waitNotifyMu sync.Mutex
	waitNotify   map[int64]chan struct{}

	// response_id -> 连接 绑定（续链亲和）。上游 chatgpt backend 无服务端存储时，
	// previous_response_id 的上下文只存活在产生该响应的那条 WS 连接里；带续链 ID
	// 的请求必须回到原连接，落到别的槽位会得到 "previous response not found"。
	// 参考 sub2api openai_ws_state_store 的 BindResponseConn/GetResponseConn。
	respConnMu       sync.Mutex
	respConnBindings map[string]responseConnBinding

	// 可选的探活函数（用于测试替换），nil 时使用默认 probeConnection
	probeFunc func(wc *WsConnection) bool

	// 可选的保活 Ping 函数（用于测试替换），nil 时使用默认 SendHeartbeat
	keepalivePingFunc func(wc *WsConnection) error

	// 测试钩子：连接写入池后、首个 pending/read lease 建立前触发。
	afterConnectionStored func(wc *WsConnection)
}

// responseConnBinding 记录某个 response_id 由哪条连接产出。
// conn 指针同时用作身份校验：同 poolKey 下连接被重建后旧绑定自动失效。
// apiKey 为产出该响应的下游 API Key（明文，仅存内存），lookup 时要求匹配，
// 防止跨 Key 用他人 response_id 定向挤上他人连接（与 response cache 的
// owner 隔离同一原则）。
type responseConnBinding struct {
	conn       *WsConnection
	sessionKey string
	accountID  int64
	apiKey     string
	model      string
	expiresAt  time.Time
}

const (
	// responseConnBindingTTL 续链绑定的存活时间。上游空闲连接本身在 IdleTimeout
	// (30min) 后被清理，绑定活得再久也无意义，与其对齐。
	responseConnBindingTTL = IdleTimeout
	// responseConnBindingMaxEntries 绑定表上限，防止内存膨胀。
	responseConnBindingMaxEntries = 4096
)

// wsWriteBufferPool 在所有上游 WS 连接间共享写缓冲，降低高并发下的内存占用。
var wsWriteBufferPool = &sync.Pool{}

// NewManager 创建连接池管理器
func NewManager() *Manager {
	m := &Manager{
		dialer: &websocket.Dialer{
			HandshakeTimeout:  HandshakeTimeout,
			EnableCompression: true,
			// 上游 Codex WS 帧可达 48-91KB，默认 4KB 缓冲会导致单帧多轮 syscall；
			// 调大到 64KB 减少读写循环次数，写缓冲走共享池复用。
			ReadBufferSize:  64 * 1024,
			WriteBufferSize: 64 * 1024,
			WriteBufferPool: wsWriteBufferPool,
			NetDialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
		stopCleanup: make(chan struct{}),
	}

	// 启动后台清理
	m.cleanupTicker = time.NewTicker(30 * time.Second)
	go m.cleanupLoop()

	return m
}

// cleanupLoop 定期清理过期连接
func (m *Manager) cleanupLoop() {
	for {
		select {
		case <-m.cleanupTicker.C:
			m.evictExpired()
		case <-m.stopCleanup:
			m.cleanupTicker.Stop()
			return
		}
	}
}

// evictExpired 清理过期连接和会话（含到龄且空闲的连接，主动轮转避免撞上游寿命上限）。
// 有在途请求的连接/会话一律跳过：IsExpired 只看 lastUsed/LastActiveAt，上游长思考
// 或 pong 丢失时会把活跃对象误判为空闲，直接 Close 会把在途流同秒批量截断
// （issue #436）；等在途收尾（读路径业务帧静默上限 ActiveReadMaxTurnSilence 兜底）后下一轮再清。
func (m *Manager) evictExpired() {
	m.connections.Range(func(key, value any) bool {
		wc := value.(*WsConnection)
		if wc.session != nil && wc.session.PendingCount() > 0 {
			return true
		}
		if wc.IsExpired() || !wc.IsConnected() || isRotatableOverAge(wc) {
			m.discardConnectionFor(wc, connectionExpiryReason(wc))
			if wc.session != nil {
				m.notifyAccountWaiters(wc.session.AccountID)
			}
		}
		return true
	})

	m.sessions.Range(func(key, value any) bool {
		s := value.(*Session)
		if s.PendingCount() > 0 {
			return true
		}
		if s.IsExpired() || !s.IsConnected() {
			m.sessions.CompareAndDelete(key, s)
			s.Close()
		}
		return true
	})
	m.evictResponseConnBindings(time.Now())
	m.trimAllIdleChatConnections()
}

// Stop 停止管理器
func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		close(m.stopCleanup)
		m.closeAll()
	})
}

// closeAll 关闭所有连接
func (m *Manager) closeAll() {
	m.closeCapacityConnections()
	m.connections.Range(func(key, value any) bool {
		wc := value.(*WsConnection)
		m.discardConnectionFor(wc, closeManagerShutdown)
		return true
	})
	m.respConnMu.Lock()
	m.respConnBindings = nil
	m.respConnMu.Unlock()

	m.sessions.Range(func(key, value any) bool {
		s := value.(*Session)
		m.sessions.Delete(key)
		s.Close()
		return true
	})
}

// SetOnConnected 设置连接回调
func (m *Manager) SetOnConnected(fn func(accountID int64, session *Session)) {
	m.mu.Lock()
	m.onConnected = fn
	m.mu.Unlock()
}

// SetOnDisconnected 设置断开回调
func (m *Manager) SetOnDisconnected(fn func(accountID int64)) {
	m.mu.Lock()
	m.onDisconnected = fn
	m.mu.Unlock()
}

// getOnDisconnected 获取断开回调
func (m *Manager) getOnDisconnected() func(accountID int64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.onDisconnected
}

// getOnConnected 获取连接回调
func (m *Manager) getOnConnected() func(accountID int64, session *Session) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.onConnected
}

type referencedMutex struct {
	mu   sync.Mutex
	refs int
}

// A reference covers the caller's entire use of the mutex, including waiting
// for it and temporarily unlocking it. Deleting locks when a connection closes
// would let a new caller race an existing waiter using a different mutex.
type refCountedLockRegistry[K comparable] struct {
	mu    sync.Mutex
	locks map[K]*referencedMutex
}

func (r *refCountedLockRegistry[K]) retain(key K) (*sync.Mutex, func()) {
	r.mu.Lock()
	if r.locks == nil {
		r.locks = make(map[K]*referencedMutex)
	}
	entry := r.locks[key]
	if entry == nil {
		entry = &referencedMutex{}
		r.locks[key] = entry
	}
	entry.refs++
	r.mu.Unlock()
	return &entry.mu, func() {
		r.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(r.locks, key)
		}
		r.mu.Unlock()
	}
}

func (m *Manager) keyLock(key string) (*sync.Mutex, func()) {
	return m.keyLocks.retain(key)
}

func (m *Manager) accountLock(accountID int64) (*sync.Mutex, func()) {
	return m.accountLocks.retain(accountID)
}

func (m *Manager) accountWaitSignal(accountID int64) <-chan struct{} {
	m.waitNotifyMu.Lock()
	defer m.waitNotifyMu.Unlock()
	if m.waitNotify == nil {
		m.waitNotify = make(map[int64]chan struct{})
	}
	ch, ok := m.waitNotify[accountID]
	if !ok {
		ch = make(chan struct{})
		m.waitNotify[accountID] = ch
	}
	return ch
}

// notifyAccountWaiters 唤醒该账号所有等待中的 acquire：连接释放/销毁/新建、在途请求结束、
// 拨号占位归还时调用。没有等待者时是一次 map 查找。
func (m *Manager) notifyAccountWaiters(accountID int64) {
	if m == nil {
		return
	}
	m.waitNotifyMu.Lock()
	ch, ok := m.waitNotify[accountID]
	if ok {
		delete(m.waitNotify, accountID)
	}
	m.waitNotifyMu.Unlock()
	if ok {
		close(ch)
	}
}

// waitForAccountChange 等待账号池变化、退避到期或上下文取消，返回实际等待时长。
func waitForAccountChange(ctx context.Context, wake <-chan struct{}, backoff time.Duration) (time.Duration, error) {
	start := time.Now()
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return time.Since(start), ctx.Err()
	case <-wake:
	case <-timer.C:
	}
	return time.Since(start), nil
}

// AcquireConnection 获取或创建连接
// 仅在同一逻辑 session 且连接空闲时复用，避免不同会话共用一条已握手连接。
func (m *Manager) AcquireConnection(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	sessionKey string,
	headers http.Header,
	proxyOverride string,
) (*WsConnection, *PendingRequest, error) {
	kind := requestConnectionCapacityKind(ctx)
	key := m.poolKey(account.ID(), wsURL, sessionKey, effectiveProxyURL(account, proxyOverride))
	lock, releaseKeyLock := m.keyLock(key)
	defer releaseKeyLock()
	accountLock, releaseAccountLock := m.accountLock(account.ID())
	defer releaseAccountLock()
	wait := AcquireInitialBackoff
	var waited time.Duration
	var createLeaseFailures int
	var busyOverflowAttempted bool

	for {
		// 先取唤醒信号再检查池状态：检查之后发生的释放/销毁一定会关闭这个 channel。
		wake := m.accountWaitSignal(account.ID())
		lock.Lock()
		if v, ok := m.connections.Load(key); ok {
			wc := v.(*WsConnection)
			if canReuseConnection(wc) {
				// 发送 Ping 探活，确认连接真正存活
				if m.probe(wc) {
					// 网络 probe 不持有账号锁。同账号其它 pool key 可以并行探活；
					// probe 期间连接可能被账号容量裁剪，因此拿锁后必须复验。
					accountLock.Lock()
					current, exists := m.connections.Load(key)
					if !exists || current != wc || !canReuseConnection(wc) {
						accountLock.Unlock()
						lock.Unlock()
						continue
					}
					pr, leaseErr := m.addPendingAndBeginReadLease(wc, sessionKey, kind)
					if leaseErr == nil {
						wc.account = account
						wc.Touch()
						m.trimIdleAccountConnections(account.ID(), connectionLimit(connectionBudgetKind(wc)), wc)
						accountLock.Unlock()
						lock.Unlock()
						return wc, pr, nil
					}
					m.discardAcquisitionFailure(wc, leaseErr)
					accountLock.Unlock()
					lock.Unlock()
					if isConnectionCapacityError(leaseErr) {
						return nil, nil, leaseErr
					}
					continue
				}
				// 探活失败，清理死连接
				m.discardConnectionFor(wc, closeProbeFailed)
				lock.Unlock()
				continue
			}
			if wc.session != nil && wc.session.PendingCount() > 0 {
				lock.Unlock()
				// 连接被同 session 的前一个请求占用：指数退避轮询等待其空闲，
				// 累计等待超过上限则返回错误，避免无界阻塞与固定间隔空转抢锁。
				// 到龄连接也会走到这里等在途请求结束，结束后下一轮循环轮转重建。
				//
				// 短等待(patience)后可溢出到同账号的兄弟槽位（issue #413，默认关闭）：
				// 前一请求长时间流式输出时，同会话的并发请求不再等满整个上限。
				// 只尝试一次；失败（容量满/拨号失败）回落到继续等待，最坏情况与关闭时一致。
				if !busyOverflowAttempted && busyOverflowEnabled() && waited >= busyOverflowPatience() && !isBusyOverflowSessionKey(sessionKey) {
					busyOverflowAttempted = true
					if owc, opr, ok := m.tryAcquireBusyOverflow(ctx, account, wsURL, sessionKey, headers, proxyOverride); ok {
						log.Printf("[WS] busy session 溢出到同账号兄弟连接 (account=%d, waited=%s)", account.ID(), waited.Round(time.Millisecond))
						return owc, opr, nil
					}
				}
				if maxWait := busyAcquireMaxWait(); waited >= maxWait {
					return nil, nil, fmt.Errorf("acquire websocket connection timed out after %s waiting for busy session", maxWait)
				}
				// 在途请求结束（RemovePendingRequest）或连接被销毁时立即唤醒，退避只是兜底。
				slept, waitErr := waitForAccountChange(ctx, wake, wait)
				if waitErr != nil {
					return nil, nil, waitErr
				}
				waited += slept
				if wait < AcquireMaxBackoff {
					wait *= 2
					if wait > AcquireMaxBackoff {
						wait = AcquireMaxBackoff
					}
				}
				continue
			}
			m.DiscardConnection(wc)
		}
		accountLock.Lock()
		capacity := m.reserveAccountConnectionCapacity(connectionCapacityRequest{accountID: account.ID(), limit: connectionLimit(kind), protectedKey: key, kind: kind})
		if capacity == nil {
			accountLock.Unlock()
			lock.Unlock()
			return nil, nil, m.connectionCapacityError(account.ID(), kind)
		}
		// 实体连接已登记，拨号期间释放账号锁，其他 session 可继续取连。
		// 空白连接的拨号登记占用预算；聊天推理不占空闲保留额度。
		accountLock.Unlock()

		wc, err := m.createConnection(ctx, connectionDialRequest{account: account, url: wsURL, sessionKey: sessionKey, headers: headers, proxyOverride: proxyOverride, capacity: capacity})
		if err != nil {
			lock.Unlock()
			return nil, nil, err
		}

		// 存储新连接并立即占位 pending request，避免返回后才记账产生竞态
		m.connections.Store(key, wc)
		if m.afterConnectionStored != nil {
			m.afterConnectionStored(wc)
		}
		pr, leaseErr := m.addPendingAndBeginReadLease(wc, sessionKey, kind)
		if leaseErr == nil {
			if earlyErr := wc.waitForEarlyReadFailure(ctx, newConnectionReadFailureGrace); earlyErr != nil {
				wc.session.RemovePendingRequest(pr.RequestID)
				leaseErr = earlyErr
			}
		}
		if leaseErr != nil {
			m.DiscardConnection(wc)
			lock.Unlock()
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			createLeaseFailures++
			if createLeaseFailures >= maxCreateLeaseAttempts {
				return nil, nil, fmt.Errorf("reserve new websocket connection after %d attempts: %w", createLeaseFailures, leaseErr)
			}
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			default:
			}
			continue
		}
		lock.Unlock()

		if fn := m.getOnConnected(); fn != nil {
			fn(account.ID(), wc.session)
		}

		return wc, pr, nil
	}
}

// tryAcquireBusyOverflow 在 busy session 等待超过 patience 后，尝试在同账号的有界
// overflow 槽位（<sessionKey>#ovf-N）上复用空闲兄弟连接或新建一条（issue #413）。
// 单遍、尽力而为：槽位也在忙则换下一个；账号容量满或拨号/租约失败即放弃，调用方
// 回落到继续等待原连接——失败路径不会比不开启 overflow 更差。
// 兄弟连接正常入池，由既有的 IdleTimeout/容量裁剪回收。
func (m *Manager) tryAcquireBusyOverflow(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	baseSessionKey string,
	headers http.Header,
	proxyOverride string,
) (*WsConnection, *PendingRequest, bool) {
	proxyURL := effectiveProxyURL(account, proxyOverride)
	kind := requestConnectionCapacityKind(ctx)
	accountLimit := connectionLimit(kind)
	accountLock, releaseAccountLock := m.accountLock(account.ID())
	defer releaseAccountLock()
	for i := 1; i <= BusyOverflowSlots; i++ {
		slotSession := fmt.Sprintf("%s%s%d", baseSessionKey, busyOverflowKeyInfix, i)
		key := m.poolKey(account.ID(), wsURL, slotSession, proxyURL)
		lock, releaseKeyLock := m.keyLock(key)
		defer releaseKeyLock()
		lock.Lock()
		if v, ok := m.connections.Load(key); ok {
			wc := v.(*WsConnection)
			if canReuseConnection(wc) {
				if m.hasLiveResponseBinding(wc) {
					lock.Unlock()
					continue
				}
				if m.probe(wc) {
					accountLock.Lock()
					current, exists := m.connections.Load(key)
					if exists && current == wc && canReuseConnection(wc) && !m.hasLiveResponseBinding(wc) {
						pr, leaseErr := m.addPendingAndBeginReadLease(wc, slotSession, kind)
						if leaseErr == nil {
							wc.account = account
							wc.Touch()
							m.trimIdleAccountConnections(account.ID(), connectionLimit(connectionBudgetKind(wc)), wc)
							accountLock.Unlock()
							lock.Unlock()
							return wc, pr, true
						}
						m.discardAcquisitionFailure(wc, leaseErr)
					}
					accountLock.Unlock()
					lock.Unlock()
					continue
				}
				m.discardConnectionFor(wc, closeProbeFailed)
			} else if wc.session != nil && wc.session.PendingCount() > 0 {
				// 兄弟槽位也在忙：换下一个槽位
				lock.Unlock()
				continue
			} else {
				// 死/到龄/过期连接：清掉腾出槽位，下方直接新建
				m.discardConnectionFor(wc, connectionExpiryReason(wc))
			}
		}
		accountLock.Lock()
		if _, ok := m.connections.Load(key); ok {
			accountLock.Unlock()
			lock.Unlock()
			continue
		}
		capacity := m.reserveAccountConnectionCapacity(connectionCapacityRequest{accountID: account.ID(), limit: accountLimit, protectedKey: key, kind: kind})
		if capacity == nil {
			// 账号连接容量已满：不为 overflow 挤占更多连接，放弃降级回到等待
			accountLock.Unlock()
			lock.Unlock()
			return nil, nil, false
		}
		accountLock.Unlock()
		wc, err := m.createConnection(ctx, connectionDialRequest{account: account, url: wsURL, sessionKey: slotSession, headers: headers, proxyOverride: proxyOverride, capacity: capacity})
		if err != nil {
			lock.Unlock()
			log.Printf("[WS] busy overflow 新建连接失败，回落等待原连接 (account=%d): %v", account.ID(), err)
			return nil, nil, false
		}
		m.connections.Store(key, wc)
		if m.afterConnectionStored != nil {
			m.afterConnectionStored(wc)
		}
		pr, leaseErr := m.addPendingAndBeginReadLease(wc, slotSession, kind)
		if leaseErr == nil {
			if earlyErr := wc.waitForEarlyReadFailure(ctx, newConnectionReadFailureGrace); earlyErr != nil {
				wc.session.RemovePendingRequest(pr.RequestID)
				leaseErr = earlyErr
			}
		}
		if leaseErr != nil {
			m.DiscardConnection(wc)
			lock.Unlock()
			return nil, nil, false
		}
		lock.Unlock()
		if fn := m.getOnConnected(); fn != nil {
			fn(account.ID(), wc.session)
		}
		return wc, pr, true
	}
	return nil, nil, false
}

// StatelessConnectionSlots 无显式会话的请求在每个 (account, cacheKey) 维度下
// 复用的持久连接槽位数。槽位内空闲连接直接复用,避免每个请求都重新握手——
// 持续高 RPM 下逐请求握手会触发上游 WS 握手限流（bad handshake → 503）。
const StatelessConnectionSlots = 8

// maxCreateLeaseAttempts bounds retries when a freshly completed handshake is
// already rejected by its permanent reader before the first request lease can
// be reserved (for example, an immediately queued peer Close frame).
const maxCreateLeaseAttempts = 3

// Give the permanent reader a small, bounded window to surface a Close/error
// already queued with the handshake before returning a newly reserved lease.
const newConnectionReadFailureGrace = 5 * time.Millisecond

// AcquireReusableConnection 在固定槽位内复用或创建连接，返回实际使用的 session key。
// 第一遍只复用已存在且空闲的连接；第二遍在空槽位新建持久连接；槽位全忙时回退到
// fallbackKey 的临时连接。空白预热共用账号空白预算；聊天收尾后按空闲上限保留。
func (m *Manager) AcquireReusableConnection(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	baseKey string,
	fallbackKey string,
	slots int,
	headers http.Header,
	proxyOverride string,
) (*WsConnection, *PendingRequest, string, error) {
	proxyURL := effectiveProxyURL(account, proxyOverride)
	kind := requestConnectionCapacityKind(ctx)
	accountLimit := connectionLimit(kind)
	if slots < 1 || slots > statelessConnectionSlots() {
		slots = statelessConnectionSlots()
	}
	accountLock, releaseAccountLock := m.accountLock(account.ID())
	defer releaseAccountLock()
	// 第一遍：复用空闲连接（探活失败或已断开的顺手清理，让第二遍可以补位）
	for i := 0; i < slots; i++ {
		slotSession := fmt.Sprintf("%s#%d", baseKey, i)
		key := m.poolKey(account.ID(), wsURL, slotSession, proxyURL)
		lock, releaseKeyLock := m.keyLock(key)
		defer releaseKeyLock()
		lock.Lock()
		if v, ok := m.connections.Load(key); ok {
			wc := v.(*WsConnection)
			if canReuseConnection(wc) {
				if m.hasLiveResponseBinding(wc) {
					lock.Unlock()
					continue
				}
				if m.probe(wc) {
					accountLock.Lock()
					current, exists := m.connections.Load(key)
					if !exists || current != wc || !canReuseConnection(wc) || m.hasLiveResponseBinding(wc) {
						accountLock.Unlock()
						lock.Unlock()
						continue
					}
					pr, leaseErr := m.addPendingAndBeginReadLease(wc, slotSession, kind)
					if leaseErr == nil {
						wc.account = account
						wc.Touch()
						m.trimIdleAccountConnections(account.ID(), connectionLimit(connectionBudgetKind(wc)), wc)
						accountLock.Unlock()
						lock.Unlock()
						return wc, pr, slotSession, nil
					}
					m.discardAcquisitionFailure(wc, leaseErr)
					accountLock.Unlock()
					lock.Unlock()
					continue
				}
				m.discardConnectionFor(wc, closeProbeFailed)
			} else if wc.session == nil || wc.session.PendingCount() == 0 {
				m.discardConnectionFor(wc, connectionExpiryReason(wc))
			}
		}
		lock.Unlock()
	}
	// 第二遍：在空槽位新建持久连接
	for i := 0; i < slots; i++ {
		slotSession := fmt.Sprintf("%s#%d", baseKey, i)
		key := m.poolKey(account.ID(), wsURL, slotSession, proxyURL)
		lock, releaseKeyLock := m.keyLock(key)
		defer releaseKeyLock()
		lock.Lock()
		if _, ok := m.connections.Load(key); ok {
			lock.Unlock()
			continue
		}
		accountLock.Lock()
		if _, ok := m.connections.Load(key); ok {
			accountLock.Unlock()
			lock.Unlock()
			continue
		}
		capacity := m.reserveAccountConnectionCapacity(connectionCapacityRequest{accountID: account.ID(), limit: accountLimit, protectedKey: key, kind: kind})
		if capacity == nil {
			accountLock.Unlock()
			lock.Unlock()
			continue
		}
		accountLock.Unlock()
		wc, err := m.createConnection(ctx, connectionDialRequest{account: account, url: wsURL, sessionKey: slotSession, headers: headers, proxyOverride: proxyOverride, capacity: capacity})
		if err != nil {
			lock.Unlock()
			return nil, nil, "", err
		}
		m.connections.Store(key, wc)
		if m.afterConnectionStored != nil {
			m.afterConnectionStored(wc)
		}
		pr, leaseErr := m.addPendingAndBeginReadLease(wc, slotSession, kind)
		if leaseErr == nil {
			if earlyErr := wc.waitForEarlyReadFailure(ctx, newConnectionReadFailureGrace); earlyErr != nil {
				wc.session.RemovePendingRequest(pr.RequestID)
				leaseErr = earlyErr
			}
		}
		if leaseErr != nil {
			m.DiscardConnection(wc)
			lock.Unlock()
			if ctx.Err() != nil {
				return nil, nil, "", ctx.Err()
			}
			continue
		}
		lock.Unlock()
		if fn := m.getOnConnected(); fn != nil {
			fn(account.ID(), wc.session)
		}
		return wc, pr, slotSession, nil
	}
	// 槽位全忙：回退一次性连接
	wc, pr, err := m.AcquireConnection(ctx, account, wsURL, fallbackKey, headers, proxyOverride)
	return wc, pr, fallbackKey, err
}

// addPendingAndBeginReadLease keeps the Session reservation and the pump lease
// atomic from an acquire caller's perspective. On failure it rolls the pending
// request back; the caller discards the unusable connection while holding its
// pool-key acquisition lock.
func (m *Manager) addPendingAndBeginReadLease(wc *WsConnection, sessionKey string, kind connectionCapacityKind) (*PendingRequest, error) {
	if wc == nil || wc.session == nil {
		return nil, fmt.Errorf("begin websocket read lease: connection has no session")
	}
	if err := m.ensureConnectionBudget(wc, kind); err != nil {
		return nil, err
	}
	pr := wc.session.AddPendingRequest(sessionKey)
	if err := wc.BeginReadLease(pr.RequestID); err != nil {
		wc.session.RemovePendingRequest(pr.RequestID)
		return nil, fmt.Errorf("reserve websocket connection: %w", err)
	}
	return pr, nil
}

func canReuseConnection(wc *WsConnection) bool {
	if wc == nil {
		return false
	}
	if !wc.IsConnected() || wc.IsExpired() || wc.IsOverAge() {
		return false
	}
	if wc.session == nil {
		return false
	}
	return wc.session.PendingCount() == 0 && wc.readPumpReusable()
}

// isRotatableOverAge 连接已到龄且当前无在途请求，可安全轮转（销毁重建）。
// 到龄但仍有在途请求的连接不动：50 分钟阈值留了 10 分钟余量，在途流仍能正常
// 收完，等其结束后再轮转，避免掐断在途响应。
func isRotatableOverAge(wc *WsConnection) bool {
	if wc == nil || !wc.IsOverAge() {
		return false
	}
	return wc.session == nil || wc.session.PendingCount() == 0
}

// probeConnection 发送 Ping 检测连接是否真正存活
func probeConnection(wc *WsConnection) bool {
	return probeConnectionWithTimeout(wc, defaultProbeTimeout)
}

// probeRecencyWindow 内有入站活动（数据帧/对端 Ping/Pong 回执）的连接免
// Ping-Pong 往返探活。往返探活在 keyLock 内串行、每次复用叠加一个上游 RTT，
// 请求刚完成后的热复用（最常见路径）不该为此买单；近期入站已证明 TCP 双向
// 存活，且 lease/队列干净由 readPumpReusable 另行把关。窗口取心跳间隔：
// 半开连接最坏在窗口过期后的下一次 probe 或 send 失败重试中被识别。
const probeRecencyWindow = HeartbeatPingInterval

// probe 调用探活函数（支持测试替换）
func (m *Manager) probe(wc *WsConnection) bool {
	m.mu.RLock()
	fn := m.probeFunc
	m.mu.RUnlock()
	if fn != nil {
		return fn(wc)
	}
	if !weakNetworkModeEnabled() && wc != nil && wc.IsConnected() && wc.recentInboundWithin(probeRecencyWindow) && wc.readPumpReusable() {
		return true
	}
	return probeConnection(wc)
}

// wsCompressionSeen 记录本进程已上报过的 permessage-deflate 协商结果
// (bit0=已见协商成功,bit1=已见未协商)。拨号器一直在 offer 压缩,但协商是否
// 成功此前没有任何可见信号;结果由上游部署与出站链路(直连/Resin)决定,按
// 结果去重、每种只报一次,混合链路下也不会逐连接刷日志。
var wsCompressionSeen atomic.Int32

// logCompressionNegotiation 上报本次握手的 permessage-deflate 协商结果。
func logCompressionNegotiation(resp *http.Response, accountID int64) {
	if resp == nil {
		return
	}
	extensions := resp.Header.Get("Sec-Websocket-Extensions")
	bit := int32(2)
	if strings.Contains(strings.ToLower(extensions), "permessage-deflate") {
		bit = 1
	}
	for {
		seen := wsCompressionSeen.Load()
		if seen&bit != 0 {
			return
		}
		if wsCompressionSeen.CompareAndSwap(seen, seen|bit) {
			break
		}
	}
	if bit == 1 {
		log.Printf("[WS] 上游已协商 permessage-deflate,帧压缩生效 (account=%d, extensions=%q)", accountID, extensions)
	} else {
		log.Printf("[WS] 上游未协商 permessage-deflate,帧走明文 (account=%d)", accountID)
	}
}

// ReleaseConnection 释放连接（归还池）
func (m *Manager) ReleaseConnection(wc *WsConnection) {
	if wc == nil {
		return
	}
	if wc.session != nil {
		m.retainReleasedConnection(wc)
		return
	}
	wc.Touch()
}

// RemoveConnection 移除连接
func (m *Manager) RemoveConnection(accountID int64, wsURL string, sessionKey string, proxyURL string) {
	key := m.poolKey(accountID, wsURL, sessionKey, proxyURL)
	if v, ok := m.connections.LoadAndDelete(key); ok {
		wc := v.(*WsConnection)
		wc.recordCloseCause(connectionCloseCause{source: "local", reason: closePoolRemoved, bound: m.hasLiveResponseBinding(wc)})
		m.removeResponseConnBindings(wc)
		wc.Close()
		if wc.session != nil {
			m.sessions.CompareAndDelete(key, wc.session)
		}
	}
	m.notifyAccountWaiters(accountID)
}

// DiscardConnection 关闭并从连接池移除一条坏连接。
// 用于上游 WS 异常路径(read error / close 1006/1009/1011 / broken pipe / unexpected EOF)：
// 关闭底层 socket 解决 CLOSE_WAIT 滞留，并把连接从 connections/sessions 移除，
// 避免坏连接被 ReleaseConnection 归还后又被 canReuseConnection 误判为可复用。
// 使用 CompareAndDelete 按本连接精确删除，防止误删同 PoolKey 下已重建的新连接。
func (m *Manager) DiscardConnection(wc *WsConnection) {
	if wc == nil {
		return
	}
	wc.recordCloseCause(connectionCloseCause{source: "local", reason: closeUnusable, bound: m.hasLiveResponseBinding(wc)})
	if wc.PoolKey != "" {
		m.connections.CompareAndDelete(wc.PoolKey, wc)
		if wc.session != nil {
			m.sessions.CompareAndDelete(wc.PoolKey, wc.session)
		}
	}
	m.removeResponseConnBindings(wc)
	if wc.session != nil {
		wc.session.StopHeartbeat()
		wc.session.SetConnected(false)
	}
	_ = wc.Close()
	if wc.session != nil {
		m.notifyAccountWaiters(wc.session.AccountID)
	}
}

// removeResponseConnBindings releases the strong references to an unusable
// connection. Keep its reader queue intact: an active consumer may still need
// the already received frames and terminal read error after an upstream close.
func (m *Manager) removeResponseConnBindings(wc *WsConnection) {
	m.respConnMu.Lock()
	defer m.respConnMu.Unlock()
	wc.responseBindingsDisabled = true
	for responseID, binding := range m.respConnBindings {
		if binding.conn == wc {
			delete(m.respConnBindings, responseID)
		}
	}
}

// responseConnBindingLiveLocked requires respConnMu. Pool identity prevents a
// late completion for a replaced socket from keeping the old socket alive.
func (m *Manager) responseConnBindingLiveLocked(wc *WsConnection) bool {
	if wc == nil || wc.responseBindingsDisabled || !wc.IsConnected() {
		return false
	}
	current, ok := m.connections.Load(wc.PoolKey)
	return ok && current == wc
}

func (m *Manager) evictResponseConnBindings(now time.Time) {
	m.respConnMu.Lock()
	defer m.respConnMu.Unlock()
	m.evictResponseConnBindingsLocked(now)
}

func (m *Manager) evictResponseConnBindingsLocked(now time.Time) {
	for responseID, binding := range m.respConnBindings {
		if !binding.expiresAt.After(now) || !m.responseConnBindingLiveLocked(binding.conn) {
			delete(m.respConnBindings, responseID)
		}
	}
}

// BindResponseConn 记录 response_id 由哪条连接产出（续链亲和）。
func (m *Manager) BindResponseConn(responseID string, wc *WsConnection, sessionKey string, accountID int64, apiKey string) {
	m.bindResponseConn(responseID, responseConnBinding{conn: wc, sessionKey: sessionKey, accountID: accountID, apiKey: apiKey})
}

func (m *Manager) bindResponseConn(responseID string, binding responseConnBinding) {
	wc := binding.conn
	responseID = strings.TrimSpace(responseID)
	if m == nil || responseID == "" || wc == nil {
		return
	}
	now := time.Now()
	m.respConnMu.Lock()
	defer m.respConnMu.Unlock()
	select {
	case <-m.stopCleanup:
		return
	default:
	}
	if !m.responseConnBindingLiveLocked(wc) {
		return
	}
	if wc.session != nil && connectionBudgetKind(wc) == capacityChat {
		wc.session.markUserContext()
	}
	if m.respConnBindings == nil {
		m.respConnBindings = make(map[string]responseConnBinding, 64)
	}
	// store:false 只承诺连接上的最新响应可续链，旧绑定必须失效。
	for id, binding := range m.respConnBindings {
		if binding.conn == wc {
			delete(m.respConnBindings, id)
		}
	}
	// 有界保护：先清一轮过期项，仍超限则拒绝新增（旧绑定比新绑定更可能被续链）。
	if len(m.respConnBindings) >= responseConnBindingMaxEntries {
		m.evictResponseConnBindingsLocked(now)
	}
	if len(m.respConnBindings) < responseConnBindingMaxEntries {
		binding.expiresAt = now.Add(responseConnBindingTTL)
		m.respConnBindings[responseID] = binding
	}
}

// lookupResponseConn 返回 response_id 绑定的连接及其池内 sessionKey。
// 绑定过期、账号/API Key 不匹配、连接已断开/被重建（池内同 key 已非同一指针）
// 时返回 nil。
func (m *Manager) lookupResponseConn(responseID string, accountID int64, apiKey string) (*WsConnection, string) {
	responseID = strings.TrimSpace(responseID)
	if m == nil || responseID == "" {
		return nil, ""
	}
	now := time.Now()
	m.respConnMu.Lock()
	defer m.respConnMu.Unlock()
	binding, ok := m.respConnBindings[responseID]
	if !ok {
		return nil, ""
	}
	if !binding.expiresAt.After(now) || !m.responseConnBindingLiveLocked(binding.conn) || binding.conn.IsExpired() || binding.conn.IsOverAge() {
		delete(m.respConnBindings, responseID)
		return nil, ""
	}
	if binding.accountID != accountID || binding.apiKey != apiKey {
		return nil, ""
	}
	return binding.conn, binding.sessionKey
}

// AcquirePreferredConnection 尝试独占 response_id 绑定的原连接（续链亲和）。
// 成功返回 (连接, pendingRequest, 池内 sessionKey)；绑定失效或连接忙时返回 nil，
// 此单次尝试保留兼容入口；生产续链通过 acquireContinuation 有界等待。
func (m *Manager) AcquirePreferredConnection(responseID string, accountID int64, apiKey string) (*WsConnection, *PendingRequest, string) {
	return m.acquirePreferredConnection(websocketContinuation{responseID: responseID, accountID: accountID, apiKey: apiKey})
}

func (m *Manager) acquirePreferredConnection(input websocketContinuation) (*WsConnection, *PendingRequest, string) {
	accountID := input.accountID
	wc, sessionKey := m.lookupContinuationConn(input)
	if wc == nil {
		return nil, nil, ""
	}
	accountLock, releaseAccountLock := m.accountLock(accountID)
	defer releaseAccountLock()
	lock, releaseKeyLock := m.keyLock(wc.PoolKey)
	defer releaseKeyLock()
	lock.Lock()
	defer lock.Unlock()
	// pool-key 加锁后复验：期间可能被其他请求占用或销毁。
	if v, exists := m.connections.Load(wc.PoolKey); !exists || v != wc {
		return nil, nil, ""
	}
	if !canReuseConnection(wc) {
		if wc.session == nil || wc.session.PendingCount() == 0 {
			m.DiscardConnection(wc)
		}
		return nil, nil, ""
	}
	if !m.probe(wc) {
		m.DiscardConnection(wc)
		return nil, nil, ""
	}
	// probe 可能等待网络，不能占用账号锁。拿到账号锁后再次复验，防止
	// probe 期间连接被其它 pool key 的容量裁剪安全回收。
	accountLock.Lock()
	defer accountLock.Unlock()
	current, _ := m.lookupContinuationConn(input)
	if current != wc || !canReuseConnection(wc) {
		return nil, nil, ""
	}
	pr, err := m.addPendingAndBeginReadLease(wc, sessionKey, capacityChat)
	if err != nil {
		m.discardAcquisitionFailure(wc, err)
		return nil, nil, ""
	}
	wc.Touch()
	if wc.account != nil {
		m.trimIdleAccountConnections(accountID, connectionLimit(connectionBudgetKind(wc)), wc)
	}
	return wc, pr, sessionKey
}

// poolKey 生成连接池键
func (m *Manager) poolKey(accountID int64, wsURL string, sessionKey string, proxyURL string) string {
	return fmt.Sprintf("%d|%s|%s|%s", accountID, wsURL, strings.TrimSpace(sessionKey), strings.TrimSpace(proxyURL))
}

// GetSession 获取会话
func (m *Manager) GetSession(accountID int64, wsURL string, sessionKey string, proxyURL string) (*Session, bool) {
	if v, ok := m.sessions.Load(m.poolKey(accountID, wsURL, sessionKey, proxyURL)); ok {
		return v.(*Session), true
	}
	return nil, false
}

// ConnectionCount 获取连接数量
func (m *Manager) ConnectionCount() int {
	count := 0
	m.connections.Range(func(key, value any) bool {
		count++
		return true
	})
	return count
}

// SessionCount 获取会话数量
func (m *Manager) SessionCount() int {
	count := 0
	m.sessions.Range(func(key, value any) bool {
		count++
		return true
	})
	return count
}

// ReplaceConnection 替换连接（用于重连）
func (m *Manager) ReplaceConnection(
	ctx context.Context,
	account *auth.Account,
	wsURL string,
	sessionKey string,
	headers http.Header,
	proxyOverride string,
) (*WsConnection, *PendingRequest, error) {
	// 先移除旧连接
	m.RemoveConnection(account.ID(), wsURL, sessionKey, effectiveProxyURL(account, proxyOverride))

	// 创建新连接
	return m.AcquireConnection(ctx, account, wsURL, sessionKey, headers, proxyOverride)
}

// SendHeartbeat 发送心跳 Ping
func (m *Manager) SendHeartbeat(wc *WsConnection) error {
	wc.writeMu.Lock()
	defer wc.writeMu.Unlock()

	if !wc.IsConnected() {
		return fmt.Errorf("connection is not connected")
	}

	deadline := time.Now().Add(10 * time.Second)
	err := wc.conn.WriteControl(websocket.PingMessage, []byte{}, deadline)
	if err != nil {
		// 写路径故障 ≠ 读路径已死：有在途请求时只摘池禁止新复用，不关 socket、
		// 不动 session——强关会把连接上全部在途流同秒截断（issue #436）。socket 的
		// 最终关闭由读路径兜底（pump 读错误时 onReadFailure→DiscardConnection，
		// 或上游 60 分钟连接寿命到期关闭）。
		if wc.session != nil && wc.session.PendingCount() > 0 {
			log.Printf("WebSocket Ping 失败 (account %d)，连接摘出池子，在途请求留给读路径裁决: %v", wc.session.AccountID, err)
			m.removeConnectionFromPool(wc)
			return err
		}
		log.Printf("WebSocket Ping 失败 (account %d): %v", wc.session.AccountID, err)
		m.DiscardConnection(wc)
		return err
	}
	return nil
}

// removeConnectionFromPool 只把连接从池中摘除（阻止后续复用），不关闭底层 socket、
// 不停在途请求。CompareAndDelete 按连接指针精确删除，防止误删同 PoolKey 下已重建的新连接。
func (m *Manager) removeConnectionFromPool(wc *WsConnection) {
	if wc == nil || wc.PoolKey == "" {
		return
	}
	m.connections.CompareAndDelete(wc.PoolKey, wc)
	if wc.session != nil {
		m.sessions.CompareAndDelete(wc.PoolKey, wc.session)
	}
	m.removeResponseConnBindings(wc)
}

// StartHeartbeat 启动连接心跳
func (m *Manager) StartHeartbeat(wc *WsConnection) {
	if wc == nil || wc.session == nil || !wc.IsConnected() || !wc.session.IsConnected() {
		return
	}
	wc.session.StartHeartbeat(func() error {
		return m.SendHeartbeat(wc)
	})
}

// 全局管理器实例
var globalManager *Manager
var managerOnce sync.Once

// GetManager 获取全局管理器实例
func GetManager() *Manager {
	managerOnce.Do(func() {
		globalManager = NewManager()
	})
	return globalManager
}

// ShutdownManager 关闭全局管理器
func ShutdownManager() {
	if globalManager != nil {
		globalManager.Stop()
	}
}
