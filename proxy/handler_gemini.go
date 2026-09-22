package proxy

import (
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var geminiNativeGenerationMethods = []string{
	"generateContent",
	"streamGenerateContent",
	"countTokens",
}

// GeminiListModels handles GET /v1beta/models for native Gemini clients.
func (h *Handler) GeminiListModels(c *gin.Context) {
	if h.enforceAPIKeyLimitsAndReply(c, "") {
		return
	}
	ctx := context.Background()
	if c.Request != nil {
		ctx = c.Request.Context()
	}
	row := apiKeyRowFromContext(c)
	if row == nil {
		c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(""))
		return
	}
	models := geminiNativeModelEntries(h.scopedModels(ctx, row))
	c.JSON(http.StatusOK, gin.H{"models": models})
}

// GeminiGetModel handles GET /v1beta/models/{model} for native Gemini clients.
func (h *Handler) GeminiGetModel(c *gin.Context) {
	action := strings.TrimPrefix(strings.TrimSpace(c.Param("action")), "/")
	if action == "" || strings.Contains(action, ":") {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"code":    http.StatusNotFound,
				"message": "Not Found",
				"status":  "NOT_FOUND",
			},
		})
		return
	}
	if h.enforceAPIKeyLimitsAndReply(c, normalizeGeminiPublicModel(action)) {
		return
	}
	ctx := context.Background()
	if c.Request != nil {
		ctx = c.Request.Context()
	}
	row := apiKeyRowFromContext(c)
	if row == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"code":    http.StatusNotFound,
				"message": "Not Found",
				"status":  "NOT_FOUND",
			},
		})
		return
	}
	modelID := normalizeGeminiPublicModel(action)
	for _, entry := range geminiNativeModelEntries(h.scopedModels(ctx, row)) {
		name, _ := entry["name"].(string)
		if normalizeGeminiPublicModel(name) == modelID {
			c.JSON(http.StatusOK, entry)
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{
		"error": gin.H{
			"code":    http.StatusNotFound,
			"message": "Not Found",
			"status":  "NOT_FOUND",
		},
	})
}

func geminiNativeModelEntries(models []api.Model) []map[string]any {
	entries := make([]map[string]any, 0, len(models))
	seen := map[string]struct{}{}
	for _, model := range models {
		id := normalizeGeminiPublicModel(model.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		entries = append(entries, geminiNativeModelEntry(id))
	}
	return entries
}

func geminiNativeModelEntry(id string) map[string]any {
	id = normalizeGeminiPublicModel(id)
	return map[string]any{
		"name":                       "models/" + id,
		"displayName":                id,
		"description":                id,
		"supportedGenerationMethods": append([]string(nil), geminiNativeGenerationMethods...),
	}
}

// GeminiModelsAction handles native Gemini API POST /v1beta/models/*action requests
// and routes Antigravity OAuth accounts through the Cloud Code v1internal adapter.
func (h *Handler) GeminiModelsAction(c *gin.Context) {
	action := strings.TrimPrefix(strings.TrimSpace(c.Param("action")), "/")
	parts := strings.SplitN(action, ":", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"code":    http.StatusNotFound,
				"message": parts[0] + " not found.",
				"status":  "NOT_FOUND",
			},
		})
		return
	}
	model := normalizeGeminiPublicModel(parts[0])
	method := parts[1]
	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, int64(security.MaxRequestBodySize+1)))
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}
	if len(rawBody) > security.MaxRequestBodySize {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Request body too large", api.ErrorTypeInvalidRequest))
		return
	}
	if !gjson.ValidBytes(rawBody) {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Invalid JSON in request body", api.ErrorTypeInvalidRequest))
		return
	}

	switch method {
	case "generateContent":
		h.handleGeminiGenerateContent(c, model, rawBody, false)
	case "streamGenerateContent":
		h.handleGeminiGenerateContent(c, model, rawBody, true)
	case "countTokens":
		h.handleGeminiCountTokens(c, model, rawBody)
	default:
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"code":    http.StatusNotFound,
				"message": method + " not found.",
				"status":  "NOT_FOUND",
			},
		})
	}
}

func geminiCountTokensInboundEndpoint() string {
	return "/v1beta/models:countTokens"
}

func (h *Handler) handleGeminiCountTokens(c *gin.Context, model string, rawBody []byte) {
	if h.enforceAPIKeyLimitsAndReply(c, model) {
		return
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}

	accountFilter := antigravityOAuthChannelAccountFilter(model)
	accountFilter = h.withModelCooldownFilter(c.Request.Context(), model, accountFilter)
	accountFilter = h.applyUpstreamChannelFilter(c, model, accountFilter)
	accountFilter = h.applyScopeBudgetFilter(c, accountFilter)
	defer h.ReleaseAPIKeyScopeConcurrency(c)

	maxRetries := h.getMaxRetries()
	maxRateLimitRetries := h.getMaxRateLimitRetries()
	continuousRetryPolicy := continuousRetryPolicyForCall(nil)
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	retryExclusions := newRetryAccountExclusions()
	antigravityRefreshRetried := map[int64]bool{}
	apiKeyID := requestAPIKeyID(c)
	sessionIdentity := resolveRequestSessionIdentity(c.Request.Header, rawBody)
	accountFilter = applyAffinityGroupRouting(c, sessionIdentity, accountFilter)
	affinityKey := sessionAffinityKey(sessionIdentity.affinityID, apiKeyID)
	dispatchPolicy := dispatchPolicyForModel(model)
	inboundEndpoint := geminiCountTokensInboundEndpoint()
	upstreamEndpoint := "/v1internal:countTokens"

	var affinityGuard auth.SessionAffinityGuard
	var selectionErr error
	var account *auth.Account
	var stickyProxyURL string
	for attempt := 0; ; attempt++ {
		affinityGuard = auth.SessionAffinityGuard{}
		account, stickyProxyURL, affinityGuard, selectionErr = h.nextRetryAccountForSessionWithDispatchGuard(
			c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter, dispatchPolicy,
		)
		if account == nil {
			if writeSchedulerQueueError(c, selectionErr, continuousRetryProtocolOpenAI) {
				return
			}
			if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			if msg := scopeBudgetExhaustedMessage(c); msg != "" {
				SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg)
				return
			}
			c.JSON(http.StatusOK, gin.H{"totalTokens": estimateInputTokens(rawBody)})
			return
		}

		h.AcquireAPIKeyScopeConcurrency(c, account)
		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		if !bindContinuousRetrySessionAffinityWithGuard(c.Request.Context(), h.store, affinityKey, account, proxyURL, affinityGuard) {
			h.store.Release(account)
			return
		}

		upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
		resp, reqErr := ExecuteAntigravityGeminiCountTokensRequest(upstreamCtx, account, model, rawBody, proxyURL)
		upstreamCancel()
		durationMs := int(time.Since(start).Milliseconds())
		attemptMaxRateLimitRetries := h.effectiveMaxRateLimitRetries(account, maxRateLimitRetries)

		if reqErr != nil {
			if apiKeyModelRequestError(reqErr) != nil {
				h.store.Release(account)
				sendAPIKeyModelRequestQuotaError(c, reqErr)
				return
			}
			retryable := isRetryableRequestError(reqErr)
			if kind := classifyTransportFailure(reqErr); retryable && shouldPenalizeTransportKind(kind) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			if !retryable || !shouldRetryRequestError(reqErr, &generalRetries, maxRetries, continuousRetryPolicy) {
				c.JSON(http.StatusOK, gin.H{"totalTokens": estimateInputTokens(rawBody)})
				return
			}
			retryExclusions.MarkRequestFailure(account.ID(), reqErr, maxRetries, continuousRetryPolicy)
			if !h.waitBeforeRetryWithBudget(c.Request.Context(), generalRetries, maxRetries) {
				return
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			lastStatusCode = resp.StatusCode
			lastBody = errBody

			if resp.StatusCode == http.StatusNotFound {
				h.store.Release(account)
				c.JSON(http.StatusOK, gin.H{"totalTokens": estimateInputTokens(rawBody)})
				return
			}

			if resp.StatusCode == http.StatusUnauthorized && account.AntigravityAuthKind() == auth.AntigravityAuthKindOAuth && !antigravityRefreshRetried[account.ID()] {
				antigravityRefreshRetried[account.ID()] = true
				if refreshErr := h.store.RefreshAntigravityAccount(c.Request.Context(), account); refreshErr == nil {
					h.store.Release(account)
					h.store.UnbindSessionAffinity(affinityKey, account.ID())
					continue
				}
			}

			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" && !antigravityNonPenalizingUpstreamFailure(account, resp.StatusCode, errBody) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			retryExclusions.MarkHTTPFailure(account.ID(), resp.StatusCode, errBody, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
			log.Printf("Gemini native countTokens upstream error (attempt %d, status %d): %s", attempt+1, resp.StatusCode, upstreamErrorConsoleBody(errBody))
			if shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy) {
				retryOrdinal, retryLimit := retryStateForHTTPStatusWithBody(resp.StatusCode, errBody, generalRetries, rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
				if h.waitBeforeRetryWithBudget(c.Request.Context(), retryOrdinal, retryLimit, resp) {
					continue
				}
				return
			}
			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		out, readErr := io.ReadAll(io.LimitReader(resp.Body, antigravityResponseBodyLimit))
		_ = resp.Body.Close()
		h.store.Release(account)
		if readErr != nil {
			ErrorToGinResponse(c, readErr)
			return
		}
		c.Data(http.StatusOK, "application/json", out)
		h.logUsageForRequest(c, &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         inboundEndpoint,
			Model:            model,
			EffectiveModel:   model,
			StatusCode:       http.StatusOK,
			DurationMs:       durationMs,
			InboundEndpoint:  inboundEndpoint,
			UpstreamEndpoint: upstreamEndpoint,
			AttemptIndex:     attempt + 1,
		})
		return
	}
}

func geminiInboundEndpoint(stream bool) string {
	if stream {
		return "/v1beta/models:streamGenerateContent"
	}
	return "/v1beta/models:generateContent"
}

func (h *Handler) handleGeminiGenerateContent(c *gin.Context, model string, rawBody []byte, stream bool) {
	if h.enforceAPIKeyLimitsAndReply(c, model) {
		return
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}

	accountFilter := antigravityOAuthChannelAccountFilter(model)
	accountFilter = h.withModelCooldownFilter(c.Request.Context(), model, accountFilter)
	accountFilter = h.applyUpstreamChannelFilter(c, model, accountFilter)
	accountFilter = h.applyScopeBudgetFilter(c, accountFilter)
	defer h.ReleaseAPIKeyScopeConcurrency(c)

	continuousRetryPolicy := continuousRetryPolicyForCall(nil)
	rememberContinuousRetryPolicyForRequest(c, continuousRetryPolicy)
	stopRetryDeadline := installContinuousRetryHTTPDeadline(c, continuousRetryPolicy, continuousRetryProtocolResponses)
	defer stopRetryDeadline()
	stopRetryKeepalive := installContinuousRetrySSEKeepalive(c, stream, "text/event-stream; charset=utf-8")
	defer stopRetryKeepalive()
	if continuousRetryBuffersAttempts(continuousRetryPolicy) {
		activateContinuousRetryKeepalive(c.Request.Context())
	}

	maxRetries := h.getMaxRetries()
	maxRateLimitRetries := h.getMaxRateLimitRetries()
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	retryExclusions := newRetryAccountExclusions()
	antigravityRefreshRetried := map[int64]bool{}
	apiKeyID := requestAPIKeyID(c)
	sessionIdentity := resolveRequestSessionIdentity(c.Request.Header, rawBody)
	accountFilter = applyAffinityGroupRouting(c, sessionIdentity, accountFilter)
	affinityKey := sessionAffinityKey(sessionIdentity.affinityID, apiKeyID)
	dispatchPolicy := dispatchPolicyForModel(model)
	inboundEndpoint := geminiInboundEndpoint(stream)
	upstreamEndpoint := antigravityUpstreamEndpoint(stream)

	var affinityGuard auth.SessionAffinityGuard
	var selectionErr error
	var account *auth.Account
	var stickyProxyURL string
	for attempt := 0; ; attempt++ {
		affinityGuard = auth.SessionAffinityGuard{}
		account, stickyProxyURL, affinityGuard, selectionErr = h.nextRetryAccountForSessionWithDispatchGuard(
			c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter, dispatchPolicy,
		)
		if account == nil {
			if writeSchedulerQueueError(c, selectionErr, continuousRetryProtocolResponses) {
				return
			}
			if !claimContinuousRetryTerminal(c, continuousRetryProtocolResponses) {
				return
			}
			if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			if msg := scopeBudgetExhaustedMessage(c); msg != "" {
				SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg)
				return
			}
			c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(model))
			return
		}

		h.AcquireAPIKeyScopeConcurrency(c, account)
		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		if !continuousRetryBuffersAttempts(continuousRetryPolicy) {
			if !bindContinuousRetrySessionAffinityWithGuard(c.Request.Context(), h.store, affinityKey, account, proxyURL, affinityGuard) {
				h.store.Release(account)
				return
			}
		}

		upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
		resp, reqErr := executeHTTPWithContinuousRetryKeepalive(upstreamCtx, func() (*http.Response, error) {
			return ExecuteAntigravityGeminiRequest(upstreamCtx, account, model, rawBody, stream, proxyURL)
		})
		upstreamCancel()
		durationMs := int(time.Since(start).Milliseconds())
		attemptMaxRateLimitRetries := h.effectiveMaxRateLimitRetries(account, maxRateLimitRetries)

		if reqErr != nil {
			if apiKeyModelRequestError(reqErr) != nil {
				h.store.Release(account)
				sendAPIKeyModelRequestQuotaError(c, reqErr)
				return
			}
			retryable := isRetryableRequestErrorForContext(c.Request.Context(), reqErr, continuousRetryPolicy)
			if kind := classifyTransportFailure(reqErr); retryable && shouldPenalizeTransportKind(kind) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			if !retryable || !shouldRetryRequestError(reqErr, &generalRetries, maxRetries, continuousRetryPolicy) {
				ErrorToGinResponse(c, reqErr)
				return
			}
			retryExclusions.MarkRequestFailure(account.ID(), reqErr, maxRetries, continuousRetryPolicy)
			if !h.waitBeforeRetryWithBudget(c.Request.Context(), generalRetries, maxRetries) {
				return
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(resp.Body)
			rememberContinuousRetryHTTPFailure(c.Request.Context(), resp, errBody)
			_ = resp.Body.Close()
			if continuousRetryCommitExpired(c, continuousRetryProtocolResponses) {
				h.store.Release(account)
				return
			}
			lastStatusCode = resp.StatusCode
			lastBody = errBody

			if resp.StatusCode == http.StatusUnauthorized && account.AntigravityAuthKind() == auth.AntigravityAuthKindOAuth && !antigravityRefreshRetried[account.ID()] {
				antigravityRefreshRetried[account.ID()] = true
				refreshErr := h.store.RefreshAntigravityAccount(c.Request.Context(), account)
				if refreshErr == nil {
					h.store.Release(account)
					h.store.UnbindSessionAffinity(affinityKey, account.ID())
					log.Printf("Antigravity OAuth token refreshed after upstream 401 (account=%d, endpoint=%s)", account.ID(), inboundEndpoint)
					continue
				}
				log.Printf("Antigravity OAuth refresh failed after upstream 401 (account=%d, endpoint=%s): %v", account.ID(), inboundEndpoint, refreshErr)
			}

			antigravityRefreshFailed := resp.StatusCode == http.StatusUnauthorized && antigravityRefreshRetried[account.ID()]
			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" && !antigravityRefreshFailed && !antigravityNonPenalizingUpstreamFailure(account, resp.StatusCode, errBody) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			retryExclusions.MarkHTTPFailure(account.ID(), resp.StatusCode, errBody, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)

			log.Printf("Gemini native upstream error (attempt %d, status %d, %s): %s", attempt+1, resp.StatusCode, inboundEndpoint, upstreamErrorConsoleBody(errBody))
			logUpstreamError(inboundEndpoint, resp.StatusCode, model, account.ID(), errBody)
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, model)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:         account.ID(),
				Endpoint:          inboundEndpoint,
				Model:             model,
				EffectiveModel:    model,
				StatusCode:        resp.StatusCode,
				DurationMs:        durationMs,
				InboundEndpoint:   inboundEndpoint,
				UpstreamEndpoint:  upstreamEndpoint,
				Stream:            stream,
				IsRetryAttempt:    shouldRetry,
				AttemptIndex:      attempt + 1,
				UpstreamErrorKind: upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:      usageLogErrorMessage(resp.StatusCode, errBody),
			})
			if shouldRetry {
				retryOrdinal, retryLimit := retryStateForHTTPStatusWithBody(resp.StatusCode, errBody, generalRetries, rateLimitRetries, maxRetries, attemptMaxRateLimitRetries, continuousRetryPolicy)
				if !h.waitBeforeRetryWithBudget(c.Request.Context(), retryOrdinal, retryLimit, resp) {
					return
				}
				continue
			}
			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		if stream {
			c.Header("Content-Type", "text/event-stream; charset=utf-8")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Status(resp.StatusCode)
			_, copyErr := io.Copy(c.Writer, resp.Body)
			_ = resp.Body.Close()
			h.store.Release(account)
			if copyErr != nil && c.Request.Context().Err() != nil {
				return
			}
			if !claimContinuousRetrySuccess(c, continuousRetryProtocolResponses) {
				return
			}
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:        account.ID(),
				Endpoint:         inboundEndpoint,
				Model:            model,
				EffectiveModel:   model,
				StatusCode:       http.StatusOK,
				DurationMs:       durationMs,
				InboundEndpoint:  inboundEndpoint,
				UpstreamEndpoint: upstreamEndpoint,
				Stream:           true,
				AttemptIndex:     attempt + 1,
			})
			return
		}

		out, readErr := readAllWithContinuousRetryKeepalive(c.Request.Context(), io.LimitReader(resp.Body, antigravityResponseBodyLimit))
		_ = resp.Body.Close()
		if readErr != nil {
			h.store.ReportRequestFailure(account, "upstream_read_error", time.Duration(durationMs)*time.Millisecond)
			h.store.Release(account)
			ErrorToGinResponse(c, readErr)
			return
		}
		inputTokens, outputTokens, reasoningTokens, totalTokens := geminiNativeUsageFromBody(out)
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.Status(resp.StatusCode)
		_, _ = c.Writer.Write(out)
		h.store.Release(account)
		if !claimContinuousRetrySuccess(c, continuousRetryProtocolResponses) {
			return
		}
		h.logUsageForRequest(c, &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         inboundEndpoint,
			Model:            model,
			EffectiveModel:   model,
			StatusCode:       http.StatusOK,
			DurationMs:       durationMs,
			InputTokens:      inputTokens,
			OutputTokens:     outputTokens,
			ReasoningTokens:  reasoningTokens,
			TotalTokens:      totalTokens,
			InboundEndpoint:  inboundEndpoint,
			UpstreamEndpoint: upstreamEndpoint,
			Stream:           false,
			AttemptIndex:     attempt + 1,
		})
		return
	}
}
