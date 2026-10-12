package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	// codexSubagentHeader 是 Codex 内部子请求（guardian 评分器等）携带的子代理标识头。
	codexSubagentHeader = "X-Openai-Subagent"

	// Codex 放在 x-codex-turn-metadata 里的 request_kind 取值（CLI 0.154 二进制实证）：
	//   turn        用户轮次的采样请求，含工具调用后的每次续采样
	//   prewarm     预热请求：预开的连接会被首个 turn 复用
	//   compaction  上下文压缩：在两次采样之间顺序执行，复用当前 turn 的会话
	//   memory      记忆整理：独立 root turn，后台并发发起，却沿用会话的 thread_id
	// 前三种与用户 turn 是同一条连接或同一段顺序动作，必须同道；未声明的视同 turn。
	// 其余取值各自成道：宁可只在同类间互相替换，也不让后台副请求切断用户在飞轮次。
	codexRequestKindTurn       = "turn"
	codexRequestKindPrewarm    = "prewarm"
	codexRequestKindCompaction = "compaction"
)

// IsCodexWebsocketPrewarmRequest 识别客户端明确声明的预热请求。
func IsCodexWebsocketPrewarmRequest(headers http.Header, body []byte) bool {
	return extractCodexRequestKind(headers, body) == codexRequestKindPrewarm
}

// ResolveCodexWebsocketTransportSessionKey returns the local-only connection
// pool lane for one explicit Codex session. A Codex session tree shares
// session-id while child agents have independent thread-id values. The shared
// upstream session remains responsible for account affinity and prompt cache;
// only a genuine child/alternate thread gets a separate transport lane.
//
// The lane is derived from raw downstream identity before any account
// fingerprint convergence. Fingerprint policy controls what the upstream sees,
// not whether independent local streams must serialize on one connection.
func ResolveCodexWebsocketTransportSessionKey(upstreamSessionID string, downstreamHeaders http.Header) string {
	return ResolveCodexWebsocketTransportSessionKeyWithBody(upstreamSessionID, downstreamHeaders, nil)
}

// ResolveCodexWebsocketTransportSessionKeyWithBody 与 ResolveCodexWebsocketTransportSessionKey
// 相同，另外按 request_kind / 子代理标识分道：同线程上与用户 turn 并发的后台请求
// （memory 整理、guardian 评分）不再与用户轮次同键互相抢占或排队。头里没有 turn
// 元数据时（Desktop 走 HTTP）回退读请求体 client_metadata 内嵌的同名 JSON。
func ResolveCodexWebsocketTransportSessionKeyWithBody(upstreamSessionID string, downstreamHeaders http.Header, body []byte) string {
	upstreamSessionID = strings.TrimSpace(upstreamSessionID)
	if upstreamSessionID == "" || IsStatelessWebsocketSessionID(upstreamSessionID) {
		return upstreamSessionID
	}
	clientSessionID, clientThreadID := extractClientCodexIdentity(downstreamHeaders)
	lane := resolveCodexExecutionLane(downstreamHeaders, body, clientThreadID)
	childThread := clientThreadID != "" && !(clientSessionID != "" && clientThreadID == clientSessionID)
	if !childThread && lane == "" {
		return upstreamSessionID
	}
	seed := "codex2api:ws-transport-lane:v1\x00" + upstreamSessionID + "\x00" + clientSessionID + "\x00" + clientThreadID
	if lane != "" {
		seed += "\x00" + lane
	}
	sum := sha256.Sum256([]byte(seed))
	return "ws-thread-" + hex.EncodeToString(sum[:16])
}

// resolveCodexExecutionLane 派生执行道后缀：主道（turn / prewarm / compaction / 未声明）
// 返回空串；其余 request_kind 各自成道；没有线程标识但声明了子代理的按子代理值成道。
func resolveCodexExecutionLane(headers http.Header, body []byte, clientThreadID string) string {
	if kind := extractCodexRequestKind(headers, body); kind != "" {
		switch kind {
		case codexRequestKindTurn, codexRequestKindPrewarm, codexRequestKindCompaction:
		default:
			return "kind=" + kind
		}
	}
	if clientThreadID == "" {
		if subagent := extractCodexSubagent(headers, body); subagent != "" {
			return "subagent=" + subagent
		}
	}
	return ""
}

// extractCodexRequestKind 取 x-codex-turn-metadata 里的 request_kind：头优先，
// 头缺失或不是合法 JSON 时回退请求体 client_metadata 内嵌的同名字符串。
func extractCodexRequestKind(headers http.Header, body []byte) string {
	if headers != nil {
		if raw := strings.TrimSpace(headers.Get(codexTurnMetadataHeader)); raw != "" && gjson.Valid(raw) {
			return normalizeCodexRequestKind(gjson.Get(raw, "request_kind"))
		}
	}
	if len(body) == 0 {
		return ""
	}
	embedded := gjson.GetBytes(body, "client_metadata."+strings.ToLower(codexTurnMetadataHeader))
	if embedded.Type != gjson.String || !gjson.Valid(embedded.String()) {
		return ""
	}
	return normalizeCodexRequestKind(gjson.Get(embedded.String(), "request_kind"))
}

func normalizeCodexRequestKind(value gjson.Result) string {
	if value.Type != gjson.String {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(value.String()))
}

func extractCodexSubagent(headers http.Header, body []byte) string {
	if headers != nil {
		if value := strings.TrimSpace(headers.Get(codexSubagentHeader)); value != "" {
			return strings.ToLower(value)
		}
	}
	if len(body) == 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "client_metadata."+strings.ToLower(codexSubagentHeader)).String()))
}
