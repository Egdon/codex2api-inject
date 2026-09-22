package auth

import (
	"fmt"
	"strings"
	"time"
)

// 凭据级 X-Codex-Turn-State 强制注入。运维把一个上游铸造的回合状态值粘到账号上，
// 网关在该账号的每个出站 Codex 请求上强制携带它（HTTP 头与 WebSocket 帧体两条路都
// 覆盖），优先于客户端回带值与账号自定义头。它不是身份：改它不影响在途请求归属，
// 也不进调度。与 proxy/codex_turn_state.go 的"跨账号回声剥离"互补——那边处理客户端
// 自己回带的值，这边处理运维显式配置的值。
const (
	CodexTurnStateProxyURLCredentialKey = "codex_turn_state_proxy_url"
	CodexTurnStateDisabledCredentialKey = "codex_turn_state_disabled"
	CodexTurnStateCredentialKey         = "codex_turn_state"
	CodexTurnStateModelsCredentialKey   = "codex_turn_state_models"
	// CodexTurnStateSetAtCredentialKey 记录注入值最后一次被换掉的时刻（RFC3339）。
	// 只服务于界面上的 1 小时时效倒计时：换值时重置，只改模型名单时保持不变。
	CodexTurnStateSetAtCredentialKey = "codex_turn_state_set_at"

	// maxCodexTurnStateBytes：实测值在 300 字符上下，留一个数量级余量即可。
	maxCodexTurnStateBytes       = 4096
	maxCodexTurnStateModelsBytes = 1024
)

// ValidateCodexTurnState 只放行能原样进 HTTP 头的单行 ASCII 可见字符串。不做截断——
// 截断后的 state 上游必然拒收，不如让操作者自己看见长度超限。
func ValidateCodexTurnState(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if len(value) > maxCodexTurnStateBytes {
		return fmt.Errorf("codex_turn_state 长度不能超过 %d 字节", maxCodexTurnStateBytes)
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return fmt.Errorf("codex_turn_state 只能包含单行 ASCII 可见字符")
		}
	}
	return nil
}

// NormalizeCodexTurnStateModels 把模型名单规整成"逗号+空格"分隔、小写、去重的形态；
// 空串表示不限模型。
func NormalizeCodexTurnStateModels(value string) string {
	seen := make(map[string]struct{})
	entries := make([]string, 0, 4)
	for _, entry := range strings.Split(value, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if _, dup := seen[entry]; dup {
			continue
		}
		seen[entry] = struct{}{}
		entries = append(entries, entry)
	}
	return strings.Join(entries, ", ")
}

func ValidateCodexTurnStateModels(value string) error {
	if len(value) > maxCodexTurnStateModelsBytes {
		return fmt.Errorf("codex_turn_state_models 长度不能超过 %d 字节", maxCodexTurnStateModelsBytes)
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return fmt.Errorf("codex_turn_state_models 只能包含 ASCII 可见字符")
		}
	}
	return nil
}

// CodexTurnStateModelsMatch 判定模型名单是否命中。名单为空表示不限模型；条目大小写
// 不敏感，结尾的 * 做前缀匹配。传入的多个模型名（客户端模型、上游模型）任一命中即
// 算命中——映射改写之后两者常常不是同一个名字，而操作者填的通常是自己请求时用的那个。
//
// 一个模型名都拿不到时按命中处理：名单是用来"缩小"注入范围的，筛不动的时候应该
// 放行而不是静默吞掉注入（补全/生图等不带 model 的出站请求会走到这里）。
func CodexTurnStateModelsMatch(scope string, models ...string) bool {
	entries := make([]string, 0, 4)
	for _, entry := range strings.Split(scope, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return true
	}
	known := false
	for _, model := range models {
		if strings.TrimSpace(model) != "" {
			known = true
			break
		}
	}
	if !known {
		return true
	}
	for _, entry := range entries {
		prefix, wildcard := strings.CutSuffix(entry, "*")
		for _, model := range models {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			if wildcard && strings.HasPrefix(strings.ToLower(model), strings.ToLower(prefix)) {
				return true
			}
			if !wildcard && strings.EqualFold(model, entry) {
				return true
			}
		}
	}
	return false
}

// ParseCodexTurnStateSetAt 解析凭据里的设置时刻；空或非法返回零值。
func ParseCodexTurnStateSetAt(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return ts
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts
	}
	return time.Time{}
}

// CodexTurnStateInjection 返回本次请求真正要注入的值，空串表示不注入（没配、或被
// 模型名单挡掉）。转发与用量日志都走这一个入口，两边不会对"注入了没有"给出不同答案。
func (a *Account) CodexTurnStateInjection(models ...string) string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	value, scope := a.CodexTurnState, a.CodexTurnStateModels
	disabled := a.CodexTurnStateDisabled
	a.mu.RUnlock()
	value = strings.TrimSpace(value)
	if disabled || value == "" || !CodexTurnStateModelsMatch(scope, models...) {
		return ""
	}
	return value
}

// CodexTurnStateConfig 返回配置快照（值、模型名单、设置时刻）。
func (a *Account) CodexTurnStateConfig() (value, models string, setAt time.Time) {
	if a == nil {
		return "", "", time.Time{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CodexTurnState, a.CodexTurnStateModels, a.CodexTurnStateSetAt
}

func (a *Account) setCodexTurnStateFromRowLocked(row interface {
	GetCredential(string) string
	GetCredentialBool(string) bool
}) {
	a.CodexTurnStateProxyURL = strings.TrimSpace(row.GetCredential(CodexTurnStateProxyURLCredentialKey))
	a.CodexTurnStateDisabled = row.GetCredentialBool(CodexTurnStateDisabledCredentialKey)
	a.CodexTurnState = strings.TrimSpace(row.GetCredential(CodexTurnStateCredentialKey))
	a.CodexTurnStateModels = NormalizeCodexTurnStateModels(row.GetCredential(CodexTurnStateModelsCredentialKey))
	a.CodexTurnStateSetAt = ParseCodexTurnStateSetAt(row.GetCredential(CodexTurnStateSetAtCredentialKey))
}

// ApplyAccountCodexTurnState 把管理端保存的注入配置立即发布到运行时账号。
func (s *Store) ApplyAccountCodexTurnState(id int64, value, models string, setAt time.Time) {
	if a := s.FindByID(id); a != nil {
		a.mu.Lock()
		a.CodexTurnState = strings.TrimSpace(value)
		a.CodexTurnStateModels = NormalizeCodexTurnStateModels(models)
		a.CodexTurnStateSetAt = setAt
		a.mu.Unlock()
	}
}

// Turn-State injection defaults to following the global switch; true opts out.
func (a *Account) IsCodexTurnStateDisabled() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CodexTurnStateDisabled
}

func (s *Store) ApplyAccountCodexTurnStateDisabled(id int64, disabled bool) {
	if a := s.FindByID(id); a != nil {
		a.mu.Lock()
		a.CodexTurnStateDisabled = disabled
		a.mu.Unlock()
	}
}

// CodexTurnStateProxy is only used by explicit template acquisition/validation.
func (a *Account) CodexTurnStateProxy() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CodexTurnStateProxyURL
}

func (s *Store) ApplyAccountCodexTurnStateProxyURL(id int64, proxyURL string) {
	if a := s.FindByID(id); a != nil {
		a.mu.Lock()
		a.CodexTurnStateProxyURL = strings.TrimSpace(proxyURL)
		a.mu.Unlock()
	}
}
