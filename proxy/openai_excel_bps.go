package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var excelBPSReplay basispoints.ReplayCache

// excelBPSDo is kept as a narrow seam for focused adapter tests. Production
// requests use the account-isolated Codex transport and the account proxy.
var excelBPSDo = func(req *http.Request, account *auth.Account, proxyURL string) (*http.Response, error) {
	client := *getPooledClient(account, proxyURL)
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client.Do(req)
}

type excelBPSHTTPError struct {
	status int
}

func (e *excelBPSHTTPError) Error() string {
	if e == nil {
		return "Basispoints upstream request failed"
	}
	return fmt.Sprintf("Basispoints upstream returned HTTP %d", e.status)
}

type excelBPSFailure struct {
	status int
	code   string
}

func (e *excelBPSFailure) Error() string {
	if e == nil || e.code == "" {
		return "Basispoints request failed"
	}
	return "Basispoints request failed: " + e.code
}

type excelBPSUpstream struct {
	response *http.Response
	bridge   *basispoints.Bridge
	model    string
}

// ExcelBPSResponse is the response returned by ExecuteExcelBPSRequest. The
// caller owns Response.Body and must close it after consuming Bridge.Stream.
type ExcelBPSResponse struct {
	Response *http.Response
	Bridge   *basispoints.Bridge
	Model    string
}

func newExcelBPSRequest(ctx context.Context, body []byte, token, accountID string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, basispoints.ResponsesURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Chatgpt-Account-Id", accountID)
	req.Header.Set("X-Openai-Account-Id", accountID)
	req.Header.Set("X-Basispoints-Auth-Mode", "chatgpt")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Origin", "https://bps.openai.com")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("X-Openai-Internal-Basispoints-Client-Product", "basispoints-excel-plugin")
	req.Header.Set("X-Openai-Internal-Basispoints-Client-Agent-Profile", "excel")
	return req, nil
}

func excelBPSAccountID(account *auth.Account, token string) string {
	if account != nil {
		if id := strings.TrimSpace(account.EffectiveAccountID()); id != "" {
			return id
		}
	}
	if claims := auth.ParseAccessToken(token); claims != nil {
		return strings.TrimSpace(claims.ChatGPTAccountID)
	}
	return ""
}

func setExcelBPSPromptCacheKey(raw []byte, threadKey string, compact bool) ([]byte, error) {
	var source map[string]any
	if err := json.Unmarshal(raw, &source); err != nil || source == nil {
		return nil, errors.New("invalid Responses request")
	}
	if compact {
		var input []any
		switch value := source["input"].(type) {
		case []any:
			input = append(input, value...)
		case string:
			input = []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": value}}}}
		default:
			return nil, errors.New("compact request input is missing")
		}
		input = append(input, map[string]any{"type": "compaction_trigger"})
		source["input"] = input
		source["tool_choice"] = "none"
	}
	if strings.TrimSpace(threadKey) != "" {
		source["prompt_cache_key"] = threadKey
	}
	return json.Marshal(source)
}

func prepareExcelBPSUpstream(ctx context.Context, account *auth.Account, raw []byte, scope, threadKey, proxyURL string, compact bool) (*excelBPSUpstream, error) {
	if account == nil || !account.IsExcelBPSEnabled() {
		return nil, &excelBPSFailure{status: http.StatusBadRequest, code: "disabled"}
	}
	token := account.GetAccessToken()
	if token == "" {
		return nil, &excelBPSFailure{status: http.StatusBadGateway, code: "auth_unavailable"}
	}
	accountID := excelBPSAccountID(account, token)
	if accountID == "" {
		return nil, &excelBPSFailure{status: http.StatusBadRequest, code: "account_identity_missing"}
	}
	preparedInput, err := setExcelBPSPromptCacheKey(raw, threadKey, compact)
	if err != nil {
		return nil, &excelBPSFailure{status: http.StatusBadRequest, code: "request_invalid"}
	}
	prepared, bridge, err := basispoints.Prepare(preparedInput, scope, &excelBPSReplay)
	if err != nil {
		return nil, &excelBPSFailure{status: http.StatusBadRequest, code: "request_unsupported"}
	}
	request, err := newExcelBPSRequest(ctx, prepared, token, accountID)
	if err != nil {
		return nil, &excelBPSFailure{status: http.StatusBadGateway, code: "request_build_failed"}
	}
	response, err := excelBPSDo(request, account, proxyURL)
	if err != nil {
		return nil, &excelBPSFailure{status: http.StatusBadGateway, code: "transport_error"}
	}
	if response == nil {
		return nil, &excelBPSFailure{status: http.StatusBadGateway, code: "empty_response"}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_ = response.Body.Close()
		return nil, &excelBPSHTTPError{status: response.StatusCode}
	}
	return &excelBPSUpstream{response: response, bridge: bridge, model: gjson.GetBytes(preparedInput, "model").String()}, nil
}

// ExecuteExcelBPSRequest sends one prepared BPS request for account-test code
// and other non-handler callers. It intentionally exposes only the response
// stream and bridge; credentials and wire construction remain private.
func ExecuteExcelBPSRequest(ctx context.Context, account *auth.Account, raw []byte, scope, threadKey, proxyURL string, compact bool) (*ExcelBPSResponse, error) {
	upstream, err := prepareExcelBPSUpstream(ctx, account, raw, scope, threadKey, proxyURL, compact)
	if err != nil {
		return nil, err
	}
	return &ExcelBPSResponse{Response: upstream.response, Bridge: upstream.bridge, Model: upstream.model}, nil
}

type excelBPSResult struct {
	StatusCode       int
	Terminal         string
	ResponseID       string
	Model            string
	UpstreamModel    string
	RequestID        string
	DurationMs       int
	FirstTokenMs     int
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	ReasoningTokens  int
	CachedTokens     int
	ClientDisconnect bool
}

func (r *excelBPSResult) usageFrom(payload []byte) {
	response := gjson.GetBytes(payload, "response")
	if !response.Exists() {
		return
	}
	r.ResponseID = response.Get("id").String()
	r.UpstreamModel = response.Get("model").String()
	usage := response.Get("usage")
	r.PromptTokens = int(usage.Get("input_tokens").Int())
	r.CompletionTokens = int(usage.Get("output_tokens").Int())
	r.TotalTokens = int(usage.Get("total_tokens").Int())
	r.ReasoningTokens = int(usage.Get("output_tokens_details.reasoning_tokens").Int())
	r.CachedTokens = int(usage.Get("input_tokens_details.cached_tokens").Int())
}

func excelBPSTerminal(kind string) bool {
	switch kind {
	case "response.completed", "response.incomplete", "response.failed", "error":
		return true
	default:
		return false
	}
}

func writeExcelBPSFrame(w io.Writer, event string, data []byte) error {
	if event == "" {
		event = gjson.GetBytes(data, "type").String()
	}
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

// forwardExcelBPS writes exactly one terminal outcome. BPS is always requested
// upstream as SSE, while non-stream Responses callers receive the completed
// response object after the terminal event is validated.
func forwardExcelBPS(ctx context.Context, c *gin.Context, account *auth.Account, raw []byte, scope, threadKey, proxyURL string, compact bool, stream bool) (excelBPSResult, error) {
	start := time.Now()
	result := excelBPSResult{}
	upstream, err := prepareExcelBPSUpstream(ctx, account, raw, scope, threadKey, proxyURL, compact)
	if err != nil {
		return result, err
	}
	defer upstream.response.Body.Close()
	result.StatusCode = upstream.response.StatusCode
	result.Model = upstream.model
	result.RequestID = upstream.response.Header.Get("x-request-id")
	converted := upstream.bridge.Stream(upstream.response.Body)
	defer converted.Close()
	if stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
	}
	flusher, _ := c.Writer.(http.Flusher)
	var completed []byte
	var writeErr error
	terminalSeen := false
	readErr := ReadSSEStreamWithEvent(converted, func(event string, data []byte) bool {
		if terminalSeen {
			return false
		}
		kind := gjson.GetBytes(data, "type").String()
		if result.FirstTokenMs == 0 && (kind == "response.output_text.delta" || kind == "response.output_item.added") {
			result.FirstTokenMs = int(time.Since(start).Milliseconds())
		}
		if excelBPSTerminal(kind) {
			terminalSeen = true
			result.Terminal = kind
			if kind == "response.completed" {
				status := strings.ToLower(strings.TrimSpace(gjson.GetBytes(data, "response.status").String()))
				if status == "failed" || status == "incomplete" {
					result.Terminal = "response." + status
				}
			}
			result.usageFrom(data)
			if !stream {
				completed = append(completed[:0], data...)
				return false
			}
		}
		if stream {
			writeErr = writeExcelBPSFrame(c.Writer, event, data)
			if writeErr == nil && flusher != nil {
				flusher.Flush()
			}
			if writeErr != nil {
				result.ClientDisconnect = true
				return false
			}
		}
		return !terminalSeen
	})
	result.DurationMs = int(time.Since(start).Milliseconds())
	if writeErr != nil {
		return result, writeErr
	}
	if ctx.Err() != nil {
		result.ClientDisconnect = true
		return result, ctx.Err()
	}
	if readErr != nil {
		return result, readErr
	}
	if !terminalSeen {
		return result, errors.New("Basispoints stream ended before a terminal event")
	}
	if !stream {
		if result.Terminal != "response.completed" {
			return result, errors.New("Basispoints response did not complete")
		}
		response := gjson.GetBytes(completed, "response")
		if !response.Exists() {
			return result, errors.New("Basispoints response did not contain a completed response")
		}
		c.Data(http.StatusOK, "application/json", []byte(response.Raw))
	}
	return result, nil
}

func excelBPSFailureInfo(err error) (int, string, string) {
	var upstream *excelBPSHTTPError
	if errors.As(err, &upstream) {
		status := upstream.status
		if status >= http.StatusInternalServerError {
			status = http.StatusBadGateway
		}
		return status, "basispoints_upstream_error", "Basispoints upstream rejected the request"
	}
	var failure *excelBPSFailure
	if errors.As(err, &failure) && failure != nil {
		status := failure.status
		if status == 0 {
			status = http.StatusBadGateway
		}
		switch failure.code {
		case "request_invalid", "request_unsupported":
			return status, "basispoints_request_invalid", "Basispoints does not support this request"
		case "account_identity_missing":
			return status, "basispoints_account_id_missing", "The selected account has no Basispoints identity"
		case "auth_unavailable":
			return status, "basispoints_auth_unavailable", "The selected account OAuth credential is unavailable"
		default:
			return status, "basispoints_transport_error", "Basispoints connection failed"
		}
	}
	return http.StatusBadGateway, "basispoints_transport_error", "Basispoints connection failed"
}

func writeExcelBPSFailure(c *gin.Context, stream bool, status int, code, message string) {
	if c == nil {
		return
	}
	if !stream || !c.Writer.Written() {
		c.JSON(status, gin.H{"error": gin.H{"type": "upstream_error", "code": code, "message": message}})
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"status": "failed", "output": []any{},
			"error": map[string]string{"code": code, "message": message},
		},
	})
	_ = writeExcelBPSFrame(c.Writer, "response.failed", payload)
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

// handleExcelBPS is the single handler branch shared by Responses and compact.
// It deliberately does not report provider failures to the account scheduler:
// BPS is an opt-in alternate provider surface, not a Codex health probe.
func (h *Handler) handleExcelBPS(c *gin.Context, account *auth.Account, raw []byte, scope, threadKey, proxyURL string, compact, stream bool, endpoint, logModel, effectiveModel, reasoningEffort string, affinityKey string, affinityGuard auth.SessionAffinityGuard, start time.Time) {
	result, err := forwardExcelBPS(c.Request.Context(), c, account, raw, scope, threadKey, proxyURL, compact, stream)
	if result.DurationMs == 0 && !start.IsZero() {
		result.DurationMs = int(max(int64(0), time.Since(start).Milliseconds()))
	}
	statusCode := result.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	logInput := &database.UsageLogInput{
		AccountID: account.ID(), Model: logModel, EffectiveModel: effectiveModel,
		Endpoint: endpoint, InboundEndpoint: endpoint, UpstreamEndpoint: basispoints.ResponsesURL,
		StatusCode: statusCode, DurationMs: result.DurationMs, FirstTokenMs: result.FirstTokenMs,
		PromptTokens: result.PromptTokens, CompletionTokens: result.CompletionTokens,
		TotalTokens: result.TotalTokens, InputTokens: result.PromptTokens, OutputTokens: result.CompletionTokens,
		ReasoningTokens: result.ReasoningTokens, CachedTokens: result.CachedTokens,
		ReasoningEffort: reasoningEffort, Stream: stream, Compact: compact,
		RequestID: result.RequestID, UpstreamResponseModel: result.UpstreamModel,
	}
	if err != nil {
		status, code, message := excelBPSFailureInfo(err)
		logInput.StatusCode = status
		logInput.UpstreamErrorKind = code
		logInput.ErrorMessage = message
		if !result.ClientDisconnect {
			writeExcelBPSFailure(c, stream, status, code, message)
		}
	}
	if result.Terminal != "response.completed" && result.Terminal != "" && logInput.ErrorMessage == "" {
		logInput.UpstreamErrorKind = "basispoints_terminal_" + strings.TrimPrefix(result.Terminal, "response.")
		logInput.ErrorMessage = "Basispoints returned a non-completed terminal event"
	}
	if h != nil {
		h.logUsageForRequest(c, logInput)
	}
	if err == nil && result.Terminal == "response.completed" {
		if h != nil && h.store != nil {
			h.store.ReleaseForSessionWithGuard(account, affinityKey, affinityGuard)
		}
		return
	}
	if h != nil && h.store != nil {
		h.store.UnbindSessionAffinity(affinityKey, account.ID())
		h.store.Release(account)
	}
}
