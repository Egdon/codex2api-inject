package wsrelay

import (
	"strings"
	"time"

	"github.com/codex2api/proxy"
)

// issue #413：一个逻辑 session 独占一条 WS 连接，同会话的并发请求会在 busy 轮询里
// 排队；前一请求长时间流式输出时，后来者最长会等满整个上限才报错换号。以下三个
// 管理界面设置项（系统设置，热更新生效）控制分级降级：
//
//	codex_ws_busy_acquire_max_wait_sec  busy/账号容量等待的累计上限秒数，
//	                                    默认 30，范围 1~300。
//	codex_ws_busy_overflow_enabled      开启后 busy 等待超过 patience 仍未空闲时，
//	                                    在同账号的有界 overflow 槽位上复用或新建
//	                                    兄弟连接（默认关闭）。请求 body 里的
//	                                    prompt_cache_key 不变，上游缓存身份不受
//	                                    连接切换影响；仅 previous_response_id
//	                                    续链依赖原连接，其走 AcquirePreferredConnection
//	                                    路径，不进入本降级。
//	codex_ws_busy_patience_sec          触发 overflow 前的短等待秒数，默认 2，
//	                                    范围 0~max wait。短请求间隙内空闲的原连接
//	                                    仍被优先复用，保住连接局部性。

// BusyOverflowSlots 每个 busy session 允许的 overflow 兄弟槽位数。有界且入池复用，
// 避免同会话高并发时逐请求握手触发上游握手限流。
const BusyOverflowSlots = 2

// busyOverflowKeyInfix 标记 overflow 槽位的 session key；带该标记的 key 自身不再
// 二次 overflow，防止槽位递归膨胀。
const busyOverflowKeyInfix = "#ovf-"

// busyAcquireMaxWait 返回 busy/容量等待的累计上限（RuntimeSettings 已做 1~300s 钳位）。
func busyAcquireMaxWait() time.Duration {
	return time.Duration(proxy.CurrentRuntimeSettings().CodexWSBusyMaxWaitSec) * time.Second
}

func busyOverflowEnabled() bool {
	return proxy.CurrentRuntimeSettings().CodexWSBusyOverflow
}

// busyOverflowPatience 返回触发 overflow 前的短等待；不会超过 busyAcquireMaxWait。
func busyOverflowPatience() time.Duration {
	settings := proxy.CurrentRuntimeSettings()
	patience := time.Duration(settings.CodexWSBusyPatienceSec) * time.Second
	if maxWait := time.Duration(settings.CodexWSBusyMaxWaitSec) * time.Second; patience > maxWait {
		return maxWait
	}
	return patience
}

func isBusyOverflowSessionKey(sessionKey string) bool {
	return strings.Contains(sessionKey, busyOverflowKeyInfix)
}

// statelessConnectionSlots 返回账号空白连接预算及通用池扫描窗口（系统设置 codex_ws_stateless_slots，
// 热更新生效，范围 0~32；0 表示不保留空白连接、不扫描通用池）。
func statelessConnectionSlots() int {
	return proxy.CurrentRuntimeSettings().CodexWSStatelessSlots
}
