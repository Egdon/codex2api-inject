package admin

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var codexTurnStateRefreshRunning sync.Map

// Scope is independent of whether a manual injection value has been configured.
func (h *Handler) codexTurnStateRefreshModels(ctx context.Context, account *auth.Account) []string {
	_, scope, _ := account.CodexTurnStateConfig()
	if strings.TrimSpace(scope) == "" {
		scope = "gpt-6-astra, gpt-5.6-*"
	}
	candidates := proxy.TextTestModelIDs(ctx, h.db)
	candidates = append(candidates, account.CodexModels()...)
	seen := make(map[string]bool)
	models := []string{}
	for _, model := range candidates {
		model = strings.TrimSpace(model)
		if seen[model] || !proxy.CodexTurnStateModelAllowed(account, model) || strings.HasPrefix(strings.ToLower(model), "claude-") || !isTextConnectionModel(model) || strings.ContainsAny(model, "()*") || !auth.CodexTurnStateModelsMatch(scope, model) {
			continue
		}
		seen[model] = true
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

type codexTurnStateRefreshResult struct {
	Model string `json:"model"`
	Saved bool   `json:"saved"`
	Error string `json:"error,omitempty"`
}

// RefreshCodexTurnStateTemplates acquires and validates scoped models with SSE progress.
func (h *Handler) RefreshCodexTurnStateTemplates(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	account := h.store.FindByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "账号不在运行时池中")
		return
	}
	if account.IsRelayStyle() {
		writeError(c, http.StatusBadRequest, "仅 Codex 账号支持模板获取")
		return
	}
	if !proxy.CodexTurnStateInjectionEnabled(account) {
		writeError(c, http.StatusBadRequest, "请开启全局 Turn-State 模板缓存，并将账号注入设为跟随全局")
		return
	}
	if _, running := codexTurnStateRefreshRunning.LoadOrStore(id, true); running {
		writeError(c, http.StatusConflict, "该账号正在获取模板")
		return
	}
	defer codexTurnStateRefreshRunning.Delete(id)
	models := h.codexTurnStateRefreshModels(c.Request.Context(), account)
	if len(models) == 0 {
		writeError(c, http.StatusBadRequest, "限定范围内没有可获取模板的模型")
		return
	}
	// Stream progress so each model is visible and reverse proxies do not time out.
	setupSSE(c)
	sendSSEJSON(c, gin.H{"type": "start", "models": models})
	saved := 0
	for _, model := range models {
		if c.Request.Context().Err() != nil {
			return
		}
		sendSSEJSON(c, gin.H{"type": "testing", "model": model})
		result := codexTurnStateRefreshResult{Model: model}
		result.Saved, err = h.refreshCodexTurnStateModel(c.Request.Context(), account, model)
		if result.Saved {
			saved++
		} else if err != nil {
			result.Error = err.Error()
		} else {
			result.Error = "未获取到通过验证的有效模板"
		}
		sendSSEJSON(c, gin.H{"type": "result", "result": result})
	}
	h.invalidateAccountSnapshotCaches()
	sendSSEJSON(c, gin.H{"type": "done", "saved": saved, "total": len(models), "status": proxy.GetCodexTurnStateStatus(account)})
}

// Both manual acquisition and background renewal use the same verified flow.
func (h *Handler) refreshCodexTurnStateModel(parent context.Context, account *auth.Account, model string, proxyOverride ...string) (bool, error) {
	if !proxy.CodexTurnStateModelAllowed(account, model) {
		return false, fmt.Errorf("该模型不支持 Turn-State 模板获取")
	}
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	proxyURL := account.CodexTurnStateProxy()
	if len(proxyOverride) > 0 {
		proxyURL = proxyOverride[0]
	}
	acquireCtx, candidate := proxy.NewCodexTurnStateRefresh(ctx, account.ID(), model, nil, proxyURL)
	if err := h.runCodexTurnStateRefreshProbe(acquireCtx, account, model); err != nil {
		return false, err
	}
	if !candidate.HasCandidate() {
		return false, nil
	}
	verifyCtx, verified := proxy.NewCodexTurnStateRefresh(ctx, account.ID(), model, candidate)
	if err := h.runCodexTurnStateRefreshProbe(verifyCtx, account, model); err != nil {
		return false, err
	}
	return verified.SaveVerified(account), nil
}

func (h *Handler) runCodexTurnStateRefreshProbe(ctx context.Context, account *auth.Account, model string) error {
	payload := h.buildAccountConnectionTestPayload(ctx, account, model, h.store.ClaudeSecurityConfig())
	start := time.Now()
	response, err := proxy.ExecuteRequest(ctx, account, payload, "", h.store.ResolveProxyForAccount(account), "", nil, nil)
	if err != nil {
		return fmt.Errorf("请求未完成，请重试")
	}
	defer response.Body.Close()
	recorder := newCodexTestRecorder(response, model, account, start)
	defer func() {
		input := connectionTestUsageFromCodex(recorder.finish(), model)
		input.Reason = "turn_state_refresh"
		input.Endpoint = "/v1/responses"
		h.logConnectionTestUsage(nil, account, input)
	}()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("上游返回 HTTP %d", response.StatusCode)
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		if state := proxy.ObserveCodexTurnStateFrame(ctx, []byte(data)); state != "" {
			proxy.CaptureCodexTurnStateTemplate(ctx, account, model, http.Header{"X-Codex-Turn-State": []string{state}})
		}
		recorder.observe([]byte(data))
		event := gjson.Parse(data)
		switch event.Get("type").String() {
		case "error", "response.failed", "response.incomplete":
			return fmt.Errorf("上游未成功完成验证请求")
		case "response.completed":
			if state := event.Get("response.status").String(); state != "" && state != "completed" {
				return fmt.Errorf("上游返回未完成状态")
			}
			// response.completed is terminal; do not wait for upstream to close
			// a keep-alive stream after the validation has already succeeded.
			return ctx.Err()
		}
	}
	return fmt.Errorf("验证请求超时或响应不完整")
}
