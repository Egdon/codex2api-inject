package proxy

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/turnstate"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 凭据级 X-Codex-Turn-State 强制注入（配置见 auth/codex_turn_state.go）。
//
// 注入发生在 ExecuteRequest 内部、传输方式与模型定稿之后：HTTP 路径写在账号自定义头
// 之后（自定义头不该顶掉运维显式配的注入值），WebSocket 路径同时写握手头与
// response.create 帧体的 client_metadata——握手头逐连接冻结，复用连接只认帧体。
// 注入不回灌下游请求体，因此不影响入口处基于 turn-state 判定"活跃回合"的调度钉号。
//
// 决策只算一次并挂到 ctx：出站头、帧体与用量日志（upstream_trace）都从同一份取值，
// 两边不会对"注入了没有"给出不同答案。

// codexTurnStateMetadataKey 是 WS response.create 帧体里承载 turn state 的键，
// 与请求头同名（官方客户端把该头原样放进 client_metadata）。
const codexTurnStateMetadataKey = "x-codex-turn-state"

type codexTurnStateInjectionKey struct{}
type codexClientModelKey struct{}

// WithCodexClientModel 记录下游请求的原始模型名，供模型名单与上游模型名一并匹配：
// 映射改写之后两者常常不是同一个名字，而操作者填的通常是自己请求时用的那个。
func WithCodexClientModel(ctx context.Context, model string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return ctx
	}
	return context.WithValue(ctx, codexClientModelKey{}, model)
}

func codexClientModelFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	model, _ := ctx.Value(codexClientModelKey{}).(string)
	return model
}

func withCodexTurnStateInjection(ctx context.Context, value string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexTurnStateInjectionKey{}, value)
}

// CodexTurnStateInjectionFromContext 返回本次出站已决定注入的值，空串表示不注入。
func CodexTurnStateInjectionFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(codexTurnStateInjectionKey{}).(string)
	return value
}

// prepareCodexTurnStateInjection 决定并落定注入：返回携带决策的 ctx、（可能克隆的）
// 下游头与（WS 时改写了帧体的）请求体。未配置或名单未命中时全部原样返回。
func prepareCodexTurnStateInjection(ctx context.Context, account *auth.Account, requestBody []byte, headers http.Header, websocket bool) (context.Context, []byte, http.Header) {
	if account == nil || !turnstate.GetConfig().InjectEnabled {
		return ctx, requestBody, headers
	}
	upstreamModel := strings.TrimSpace(gjson.GetBytes(requestBody, "model").String())
	clientModel := codexClientModelFromContext(ctx)
	injected := ""
	if token := turnstate.Global().LookupInjectibleNow(account.ID(), firstNonEmptyModel(upstreamModel, clientModel)); token != "" {
		injected = token
	} else if token := turnstate.Global().LookupInjectibleNow(account.ID(), clientModel); token != "" {
		injected = token
	} else if token := turnstate.Global().LookupInjectibleNow(account.ID(), upstreamModel); token != "" {
		injected = token
	}
	if injected == "" {
		return ctx, requestBody, headers
	}
	ctx = withCodexTurnStateInjection(ctx, injected)
	if headers == nil {
		headers = make(http.Header)
	} else {
		headers = headers.Clone()
	}
	headers.Set(codexTurnStateHeader, injected)
	if websocket {
		// 帧体承载：WS 的握手头逐连接冻结，复用连接根本发不出新值，官方客户端因此把
		// turn state 放进 response.create 的 client_metadata——WS 路径必须写。
		if updated, err := sjson.SetBytes(requestBody, "client_metadata."+codexTurnStateMetadataKey, injected); err == nil {
			requestBody = updated
		}
	}
	return ctx, requestBody, headers
}

// applyCodexTurnStateInjectionHeader 在账号自定义头装配之后落定注入值：自定义头不该
// 把别的状态带回上游顶掉运维显式配的注入。未注入时是空操作。
func applyCodexTurnStateInjectionHeader(ctx context.Context, headers http.Header) {
	if headers == nil {
		return
	}
	if value := CodexTurnStateInjectionFromContext(ctx); value != "" {
		headers.Set(codexTurnStateHeader, value)
	}
}

// ApplyCodexTurnStateInjectionHeader 是 applyCodexTurnStateInjectionHeader 的导出形态，
// 供 wsrelay 在握手头装配末尾调用。
func ApplyCodexTurnStateInjectionHeader(ctx context.Context, headers http.Header) {
	applyCodexTurnStateInjectionHeader(ctx, headers)
}

// 观测到的 state 实测在 300 字符上下，留一个数量级的余量即可；超限的一律丢弃，
// 截断后的 state 既不能复用也会误导排查。
const maxObservedCodexTurnStateBytes = 4096

// observedCodexTurnState 规整一个观测值：只接受单行可见字符串，超限丢弃。
func observedCodexTurnState(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxObservedCodexTurnStateBytes || !utf8.ValidString(value) {
		return ""
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return value
}

var codexTurnStateFrameNeedles = [][]byte{[]byte("turn-state"), []byte("Turn-State")}

// codexTurnStateFromFrame 从 WS 事件帧里找上游回带的 turn state。官方契约里 WS 路径
// 的值来自握手响应头或 response.metadata 事件；这里按键名等值（大小写不敏感）在几个
// 已知承载位置上找，找不到返回空。先做一次零分配的子串预检，避免每帧都解析 JSON。
func codexTurnStateFromFrame(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	mentioned := false
	for _, needle := range codexTurnStateFrameNeedles {
		if bytes.Contains(payload, needle) {
			mentioned = true
			break
		}
	}
	if !mentioned {
		return ""
	}
	root := gjson.ParseBytes(payload)
	if !root.IsObject() {
		return ""
	}
	for _, path := range []string{"headers", "response.headers", "response.client_metadata", "client_metadata", "response.metadata", "metadata", "response", ""} {
		object := root
		if path != "" {
			object = root.Get(path)
		}
		if !object.IsObject() {
			continue
		}
		state := ""
		object.ForEach(func(key, value gjson.Result) bool {
			if strings.EqualFold(key.String(), codexTurnStateHeader) && value.Type == gjson.String {
				state = value.String()
				return false
			}
			return true
		})
		if state = observedCodexTurnState(state); state != "" {
			return state
		}
	}
	return ""
}

// ObserveCodexTurnStateFrame 供 WS 中继在逐帧转发时调用：发现上游回带的 turn state
// 就记到本次尝试的追踪里（用量日志据此显示"回带 Turn State"）。
func ObserveCodexTurnStateFrame(ctx context.Context, payload []byte) {
	if state := codexTurnStateFromFrame(payload); state != "" {
		noteUpstreamTurnState(ctx, state)
	}
}

func firstNonEmptyModel(models ...string) string {
	for _, model := range models {
		if model = strings.TrimSpace(model); model != "" {
			return model
		}
	}
	return ""
}
