package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"unicode/utf8"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// adminImageRecorder keeps informational keepalives separate from the final
// response. httptest.ResponseRecorder otherwise commits the first 102 forever.
// This adapter is used only for in-process Studio calls, never public traffic.
// Deliberately no Unwrap: keepalive must reach this adapter, not the recorder.
type adminImageRecorder struct {
	*httptest.ResponseRecorder
	finalCode int
}

func newAdminImageRecorder() *adminImageRecorder {
	return &adminImageRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (r *adminImageRecorder) WriteHeader(code int) {
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		return
	}
	if r.finalCode != 0 {
		return
	}
	r.ResponseRecorder.WriteHeader(code)
	r.finalCode = code
}

func (r *adminImageRecorder) Write(body []byte) (int, error) {
	if r.finalCode == 0 {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseRecorder.Write(body)
}

func (r *adminImageRecorder) WriteString(body string) (int, error) {
	if r.finalCode == 0 {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseRecorder.WriteString(body)
}

func (r *adminImageRecorder) Flush() {
	if r.finalCode == 0 {
		r.WriteHeader(http.StatusOK)
	}
	r.ResponseRecorder.Flush()
}

func (r *adminImageRecorder) imageResult(operation string) ([]byte, int, error) {
	body := r.Body.Bytes()
	if r.finalCode == 0 {
		return body, http.StatusBadGateway, fmt.Errorf("image %s ended without a final HTTP response", operation)
	}
	if r.finalCode < 200 || r.finalCode >= 300 {
		message := extractAdminImageErrorMessage(body)
		if message == "" {
			return body, r.finalCode, fmt.Errorf("image %s failed with HTTP %d", operation, r.finalCode)
		}
		return body, r.finalCode, fmt.Errorf("image %s failed with HTTP %d: %s", operation, r.finalCode, message)
	}
	return body, r.finalCode, nil
}

// GenerateImageOnceForAdmin executes the existing Images API handler in-process.
// It keeps model aliasing, account dispatch, usage logging, and image parsing in one code path.
func (h *Handler) GenerateImageOnceForAdmin(ctx context.Context, rawBody []byte, apiKey *database.APIKeyRow, sharedAPIKeyConcurrency bool) ([]byte, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if h == nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("image proxy handler is not initialized")
	}

	recorder := newAdminImageRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(rawBody)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	if apiKey != nil && strings.TrimSpace(apiKey.Key) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey.Key)
		ginCtx.Set(contextAPIKeyRow, apiKey)
		ginCtx.Set(contextAPIKeyID, apiKey.ID)
		ginCtx.Set(contextAPIKeyName, strings.TrimSpace(apiKey.Name))
		ginCtx.Set(contextAPIKeyMasked, security.MaskAPIKey(apiKey.Key))
	}
	ginCtx.Request = req

	// 每个批量输出都是一次真实上游请求，因此复用标准 Images handler 的
	// API Key 限额。并发槽由调用方决定：入队时已经为整个任务占过槽的路径
	// 传 true，否则这里会为同一个 Key 再占一次，把上限打对折。
	if sharedAPIKeyConcurrency {
		ginCtx.Set(contextAPIKeyConcurrencyInherited, true)
	}
	if status, msg := h.applyAdminScopeBudget(ctx, ginCtx, apiKey); status != 0 {
		return nil, status, fmt.Errorf("%s", msg)
	}

	h.ImagesGenerations(ginCtx)

	return recorder.imageResult("generation")
}

const maxAdminImageErrorBytes = 2048
const maxAdminImageErrorInspectBytes = 64 * 1024

func extractAdminImageErrorMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	// Do not stringify/parse multi-megabyte image payloads on the error path.
	if len(body) > maxAdminImageErrorInspectBytes {
		return "large response body omitted"
	}
	if bytes.Contains(body, []byte(`"b64_json"`)) || bytes.Contains(bytes.ToLower(body), []byte("data:image/")) {
		return "image response body omitted"
	}
	message := strings.TrimSpace(gjsonGetString(body, "error.message"))
	if message == "" {
		value := gjson.GetBytes(body, "error")
		if value.Type == gjson.String {
			message = strings.TrimSpace(value.String())
		}
	}
	if message == "" {
		// Structured non-error JSON may contain image data in another field.
		// Never echo the whole JSON document as a diagnostic.
		if gjson.ValidBytes(body) {
			return "JSON response contained no error message"
		}
		message = strings.TrimSpace(string(body))
	}
	if len(message) > maxAdminImageErrorBytes {
		message = message[:maxAdminImageErrorBytes]
		for !utf8.ValidString(message) && len(message) > 0 {
			message = message[:len(message)-1]
		}
		message += "…"
	}
	return message
}

// GenerateImageEditForAdmin executes the ImagesEdits handler in-process for
// image-to-image (edit) jobs from the admin image studio.
func (h *Handler) GenerateImageEditForAdmin(ctx context.Context, rawBody []byte, apiKey *database.APIKeyRow, sharedAPIKeyConcurrency bool) ([]byte, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if h == nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("image proxy handler is not initialized")
	}

	recorder := newAdminImageRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(rawBody)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	if apiKey != nil && strings.TrimSpace(apiKey.Key) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey.Key)
		ginCtx.Set(contextAPIKeyRow, apiKey)
		ginCtx.Set(contextAPIKeyID, apiKey.ID)
		ginCtx.Set(contextAPIKeyName, strings.TrimSpace(apiKey.Name))
		ginCtx.Set(contextAPIKeyMasked, security.MaskAPIKey(apiKey.Key))
	}
	ginCtx.Request = req

	if sharedAPIKeyConcurrency {
		ginCtx.Set(contextAPIKeyConcurrencyInherited, true)
	}
	if status, msg := h.applyAdminScopeBudget(ctx, ginCtx, apiKey); status != 0 {
		return nil, status, fmt.Errorf("%s", msg)
	}

	h.ImagesEdits(ginCtx)

	return recorder.imageResult("edit")
}

func gjsonGetString(body []byte, path string) string {
	return gjson.GetBytes(body, path).String()
}

// Unlike a network ResponseWriter, httptest.ResponseRecorder commits the first
// informational response. Image keepalives send 102 before the final response;
// do not let that heartbeat turn a successful image into a failed job.
type adminImageResponseRecorder struct {
	*httptest.ResponseRecorder
}

func newAdminImageResponseRecorder() *adminImageResponseRecorder {
	return &adminImageResponseRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (w *adminImageResponseRecorder) WriteHeader(code int) {
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		return
	}
	w.ResponseRecorder.WriteHeader(code)
}

// applyAdminScopeBudget 为 in-process 的管理端生图任务计算 scope 预算闸门：
// reject 类超额直接返回 429，skip 类挂到该 gin context 上供账号过滤链剔除候选。
func (h *Handler) applyAdminScopeBudget(ctx context.Context, ginCtx *gin.Context, apiKey *database.APIKeyRow) (int, string) {
	if apiKey == nil || len(apiKey.Limits.ScopeLimits) == 0 {
		return 0, ""
	}
	gate, rejectMsg := h.evaluateAPIKeyScopeBudgets(ctx, apiKey)
	if rejectMsg != "" {
		return http.StatusTooManyRequests, rejectMsg
	}
	if gate != nil {
		ginCtx.Set(contextScopeBudgetGate, gate)
	}
	return 0, ""
}
