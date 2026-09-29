package proxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/basispoints"
	"github.com/gin-gonic/gin"
)

// excelBPSResponseHeader marks the synthesized Responses SSE returned by
// openExcelBPSStream. It never reaches clients; handlers use it to label usage.
const excelBPSResponseHeader = "X-Codex2api-Upstream-Adapter"

// excelBPSUpstreamURL labels usage rows served by the Basispoints adapter.
const excelBPSUpstreamURL = basispoints.ResponsesURL

// excelBPSIngress describes one attempt of an ingress other than HTTP
// /v1/responses (Chat Completions, Messages, Responses WebSocket) that may be
// served by the Excel Basispoints adapter. Those handlers already consume
// Responses SSE from ExecuteRequest, so Basispoints is offered to them in the
// same shape instead of duplicating their stream translation.
type excelBPSIngress struct {
	// Endpoint is the inbound endpoint used for usage rows.
	Endpoint        string
	LogModel        string
	EffectiveModel  string
	ReasoningEffort string
	Scope           string
	ThreadKey       string
	ProxyURL        string
	PersistReplay   bool
	// Fallback is owned by the caller for one request (or one WebSocket turn).
	// Once set, later attempts stay on native Codex, like handleExcelBPS.
	Fallback *string
}

func excelBPSIngressScope(account *auth.Account, apiKeyID int64, affinityKey string) string {
	return fmt.Sprintf("account:%d:key:%d:thread:%s", account.ID(), apiKeyID, affinityKey)
}

// isExcelBPSResponse reports whether resp was synthesized by openExcelBPSStream.
func isExcelBPSResponse(resp *http.Response) bool {
	return resp != nil && resp.Header.Get(excelBPSResponseHeader) == "basispoints"
}

// upstreamEndpointForResponse labels usage with the Basispoints endpoint when
// the adapter served the response.
func upstreamEndpointForResponse(resp *http.Response, native string) string {
	if isExcelBPSResponse(resp) {
		return basispoints.ResponsesURL
	}
	return native
}

type excelBPSStreamBody struct {
	io.ReadCloser
	upstream io.Closer
}

func (b *excelBPSStreamBody) Close() error {
	err := b.ReadCloser.Close()
	if upstreamErr := b.upstream.Close(); err == nil {
		err = upstreamErr
	}
	return err
}

// openExcelBPSStream tries Basispoints for one ingress attempt. It returns
// served=false when the caller must continue with native Codex on the same
// account: the request shape needs native context, or Basispoints failed in a
// way the existing fallback policy allows before any output. Other Basispoints
// failures are returned as non-retryable structured errors so the caller's
// normal error path reports them without touching Codex account health.
func (h *Handler) openExcelBPSStream(ctx context.Context, c *gin.Context, account *auth.Account, body []byte, in excelBPSIngress) (*http.Response, bool, error) {
	if in.Fallback != nil && *in.Fallback != "" {
		return nil, false, nil
	}
	fallback := func(reason string) (*http.Response, bool, error) {
		if in.Fallback != nil {
			*in.Fallback = reason
		}
		log.Printf("[excel-bps] account=%d native fallback reason=%s before_output=true endpoint=%s", account.ID(), reason, in.Endpoint)
		return nil, false, nil
	}
	if reason := excelBPSNativeRequestReason(body); reason != "" {
		return fallback(reason)
	}
	effectiveModel := in.EffectiveModel
	if h != nil {
		if mappedBody, mappedModel, ok := h.applyAccountModelMappingToBodyForModels(body, account, in.LogModel, in.EffectiveModel); ok {
			body = mappedBody
			effectiveModel = mappedModel
		}
	}
	start := time.Now()
	upstream, err := prepareExcelBPSUpstream(ctx, account, body, in.Scope, in.ThreadKey, in.ProxyURL, false, in.PersistReplay)
	if err != nil {
		status, code, message := excelBPSFailureInfo(err)
		if reason := excelBPSNativeFailureReason(err); reason != "" && ctx.Err() == nil && (c == nil || c.Request.Context().Err() == nil) {
			if h != nil && c != nil {
				h.logUsageForRequest(c, &database.UsageLogInput{
					AccountID: account.ID(), Model: in.LogModel, EffectiveModel: effectiveModel,
					Endpoint: in.Endpoint, InboundEndpoint: in.Endpoint, UpstreamEndpoint: basispoints.ResponsesURL,
					StatusCode: status, DurationMs: int(time.Since(start).Milliseconds()), Stream: true,
					ReasoningEffort: in.ReasoningEffort, UpstreamErrorKind: code, ErrorMessage: message,
					IsRetryAttempt: true,
				})
			}
			return fallback(reason)
		}
		return nil, true, &Error{Code: code, Message: message, Type: "upstream_error", HTTPStatus: status}
	}
	header := make(http.Header)
	header.Set("Content-Type", "text/event-stream")
	header.Set(excelBPSResponseHeader, "basispoints")
	if requestID := upstream.response.Header.Get("x-request-id"); requestID != "" {
		header.Set("x-request-id", requestID)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     header,
		Body:       &excelBPSStreamBody{ReadCloser: upstream.bridge.Stream(upstream.response.Body), upstream: upstream.response.Body},
		Request:    upstream.response.Request,
	}, true, nil
}
