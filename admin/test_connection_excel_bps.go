package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (h *Handler) runExcelBPSBatchTest(ctx context.Context, account *auth.Account) (string, string) {
	if h == nil || h.store == nil || account == nil {
		return "failed", "Basispoints account is unavailable"
	}
	model, err := h.connectionTestModelForAccount(ctx, account, "")
	if err != nil {
		return "failed", err.Error()
	}
	if !account.IsExcelBPSAvailableForModel(model) {
		return "failed", "Basispoints is not enabled for the selected model"
	}
	payload := h.buildAccountConnectionTestPayload(ctx, account, model, h.store.ClaudeSecurityConfig())
	if mapped, ok := proxy.ResolveAccountModelMapping(account, model); ok && mapped != "" {
		if next, setErr := sjson.SetBytes(payload, "model", mapped); setErr == nil {
			payload = next
		}
	}
	thread := fmt.Sprintf("account-test:%d", account.ID())
	request, err := proxy.ExecuteExcelBPSRequest(ctx, account, payload, thread, thread, h.store.ResolveProxyForAccount(account), false)
	if err != nil || request == nil || request.Response == nil || request.Bridge == nil {
		return "failed", "Basispoints connection failed"
	}
	defer request.Response.Body.Close()
	if request.Response.StatusCode < 200 || request.Response.StatusCode >= 300 {
		return "failed", "Basispoints upstream rejected the connection test"
	}
	stream := request.Bridge.Stream(request.Response.Body)
	defer stream.Close()
	start := time.Now()
	hasContent := false
	terminal := ""
	var last []byte
	readErr := proxy.ReadSSEStream(stream, func(data []byte) bool {
		last = append(last[:0], data...)
		kind := gjson.GetBytes(data, "type").String()
		if kind == "response.output_text.delta" && strings.TrimSpace(gjson.GetBytes(data, "delta").String()) != "" {
			hasContent = true
		}
		switch kind {
		case "response.completed":
			terminal = kind
			if !hasContent && strings.TrimSpace(extractCompletedOutputText(data)) != "" {
				hasContent = true
			}
			return false
		case "response.failed", "response.incomplete", "error":
			terminal = kind
			return false
		default:
			return true
		}
	})
	if readErr != nil {
		if ctx.Err() != nil {
			return "failed", "Basispoints connection test timed out"
		}
		return "failed", "Basispoints stream failed"
	}
	if terminal != "response.completed" {
		return "failed", "Basispoints returned an unsuccessful terminal event"
	}
	if !hasContent {
		return "failed", "Basispoints returned no assistant content"
	}
	if h.store != nil {
		h.store.RecordManualTestSuccess(account, time.Since(start))
	}
	return "success", "测试通过"
}

func (h *Handler) runExcelBPSInteractiveTest(
	c *gin.Context,
	account *auth.Account,
	payload []byte,
	testModel string,
	start time.Time,
	isTransient bool,
	restoreOnSuccess bool,
	transientOutcome *string,
	id int64,
	quality bool,
	usageReason string,
	usageEndpoint string,
	usageEffort string,
) {
	if h == nil || h.store == nil || account == nil {
		sendTestEvent(c, testEvent{Type: "error", Error: "Basispoints account is unavailable"})
		return
	}
	thread := fmt.Sprintf("account-test:%d", account.ID())
	request, err := proxy.ExecuteExcelBPSRequest(c.Request.Context(), account, payload, thread, thread, h.store.ResolveProxyForAccount(account), false)
	if err != nil || request == nil || request.Response == nil || request.Bridge == nil {
		message := "Basispoints connection failed"
		failed := newCodexTestRecorder(nil, testModel, account, start)
		diagnostics := failed.finish()
		sendTestEvent(c, testEvent{Type: "diagnostics", CodexDiagnostics: diagnostics})
		usage := connectionTestUsageFromCodex(diagnostics, testModel)
		usage.Reason, usage.Endpoint, usage.ReasoningEffort = usageReason, usageEndpoint, usageEffort
		usage.StatusCode = connectionTestTransportStatus
		usage.DurationMs = int(max(int64(0), time.Since(start).Milliseconds()))
		usage.ErrorMessage, usage.UpstreamErrorKind = message, "basispoints_transport"
		h.logConnectionTestUsage(c, account, usage)
		sendTestEvent(c, testEvent{Type: "error", Error: message})
		return
	}

	response := request.Response
	defer response.Body.Close()
	recorder := newCodexTestRecorder(response, testModel, account, start)
	defer func() {
		diagnostics := recorder.finish()
		sendTestEvent(c, testEvent{Type: "diagnostics", CodexDiagnostics: diagnostics})
		usage := connectionTestUsageFromCodex(diagnostics, testModel)
		usage.Reason, usage.Endpoint, usage.ReasoningEffort = usageReason, usageEndpoint, usageEffort
		h.logConnectionTestUsage(c, account, usage)
	}()
	sendTestEvent(c, testEvent{Type: "diagnostics", CodexDiagnostics: recorder.details})
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		sendTestEvent(c, testEvent{Type: "error", Error: "Basispoints upstream rejected the connection test"})
		return
	}

	stream := request.Bridge.Stream(response.Body)
	defer stream.Close()
	hasContent := false
	gotTerminal := false
	lastEvent := ""
	emitContent := func(text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		hasContent = true
		recorder.contentReceived()
		sendTestEvent(c, testEvent{Type: "content", Text: text})
	}
	readErr := proxy.ReadSSEStream(stream, func(data []byte) bool {
		recorder.observe(data)
		kind := gjson.GetBytes(data, "type").String()
		lastEvent = kind
		switch kind {
		case "response.output_text.delta":
			emitContent(gjson.GetBytes(data, "delta").String())
		case "response.output_text.done":
			if !hasContent {
				emitContent(gjson.GetBytes(data, "text").String())
			}
		case "response.content_part.done":
			if !hasContent {
				emitContent(gjson.GetBytes(data, "part.text").String())
			}
		case "response.output_item.done":
			if !hasContent {
				emitContent(extractOutputItemText(gjson.GetBytes(data, "item")))
			}
		case "response.completed":
			gotTerminal = true
			status := strings.ToLower(strings.TrimSpace(gjson.GetBytes(data, "response.status").String()))
			if status == "failed" || status == "incomplete" {
				sendTestEvent(c, testEvent{Type: "error", Error: "Basispoints returned an unsuccessful terminal event"})
				return false
			}
			if !hasContent {
				emitContent(extractCompletedOutputText(data))
			}
			if !hasContent {
				sendTestEvent(c, testEvent{Type: "error", Error: "Basispoints returned no assistant content"})
				return false
			}
			if isTransient {
				if transientOutcome != nil {
					*transientOutcome = "success"
				}
				if restoreOnSuccess {
					restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 5*time.Second)
					restoreErr := h.restoreAccountByID(restoreCtx, id)
					restoreCancel()
					if restoreErr != nil {
						sendTestEvent(c, testEvent{Type: "content", Text: "\n\n--- 自动恢复失败 ---"})
					}
				}
			} else {
				h.store.RecordManualTestSuccess(account, time.Since(start))
			}
			if !quality {
				sendTestEvent(c, testEvent{Type: "content", Text: fmt.Sprintf("\n\n--- 耗时 %dms ---", time.Since(start).Milliseconds())})
			}
			sendTestEvent(c, testEvent{Type: "test_complete", Success: true})
			return false
		case "response.failed", "response.incomplete", "error":
			gotTerminal = true
			sendTestEvent(c, testEvent{Type: "error", Error: "Basispoints returned an unsuccessful terminal event"})
			return false
		}
		return true
	})
	if readErr != nil {
		if c.Request.Context().Err() != nil {
			sendTestEvent(c, testEvent{Type: "error", Error: "Basispoints connection test timed out"})
		} else if !gotTerminal {
			sendTestEvent(c, testEvent{Type: "error", Error: "Basispoints stream failed"})
		}
		return
	}
	if !gotTerminal {
		if lastEvent == "" {
			lastEvent = "unknown"
		}
		sendTestEvent(c, testEvent{Type: "error", Error: "Basispoints response ended before completion"})
	}
}
