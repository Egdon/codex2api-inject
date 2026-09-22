package proxy

import (
	"context"
	"errors"
	"net/http"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

const schedulerQueueFullMessage = "Scheduler wait queue is full. Retry after 1 second."
const schedulerSelectionTimeoutMessage = "Account selection timed out. Retry after 1 second."

func schedulerQueueFullAPIError() *api.APIError {
	return api.NewAPIError(api.ErrCodeServiceUnavailable, schedulerQueueFullMessage, api.ErrorTypeServer)
}

func schedulerSelectionAPIError(err error) *api.APIError {
	switch {
	case errors.Is(err, auth.ErrSchedulerQueueFull):
		return schedulerQueueFullAPIError()
	case errors.Is(err, context.DeadlineExceeded):
		return api.NewAPIError(api.ErrCodeServiceUnavailable, schedulerSelectionTimeoutMessage, api.ErrorTypeServer)
	default:
		return nil
	}
}

// Queue overload and selection timeout are local retryable capacity errors.
// Neither should start another quota scan or overwrite committed SSE.
func writeSchedulerQueueError(c *gin.Context, err error, protocol continuousRetryHTTPProtocol) bool {
	apiErr := schedulerSelectionAPIError(err)
	if apiErr == nil {
		return false
	}
	if !claimContinuousRetryTerminal(c, protocol) || c.Request.Context().Err() != nil {
		return true
	}
	if !c.Writer.Written() {
		c.Header("Retry-After", "1")
	}
	message := apiErr.Message
	switch protocol {
	case continuousRetryProtocolAnthropic:
		if !writeCommittedAnthropicRetryError(c, "overloaded_error", message) {
			sendAnthropicError(c, http.StatusServiceUnavailable, "overloaded_error", message)
		}
	case continuousRetryProtocolChat:
		if !writeCommittedChatRetryError(c, message) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": apiErr})
		}
	default:
		if !writeCommittedResponsesRetryError(c, message) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": apiErr})
		}
	}
	return true
}
