package proxy

import (
	"fmt"
	"net/http"

	"github.com/codex2api/api"
)

// ResponsesContinuationLostError 表示连接内续链状态已失效；不能携带旧 ID 重拨。
type ResponsesContinuationLostError struct {
	Reason string
}

type ResponsesContinuationBusyError struct{}

func (*ResponsesContinuationBusyError) Error() string {
	return "original websocket connection remained busy until the wait deadline"
}

func (*ResponsesContinuationBusyError) Unwrap() error {
	return &Error{Code: ErrorCodeAccountPoolConcurrencySaturated, Type: ErrorTypeServerError,
		HTTPStatus: http.StatusServiceUnavailable, Message: "Original WebSocket connection is busy, please retry later"}
}

func (err *ResponsesContinuationLostError) Error() string {
	return fmt.Sprintf("websocket continuation unavailable: %s", err.Reason)
}

func (err *ResponsesContinuationLostError) Unwrap() error {
	return &Error{Code: "previous_response_not_found", Type: ErrorTypeInvalidRequest,
		HTTPStatus: http.StatusConflict, Message: "Previous response context is unavailable"}
}

func nativeResponsesWSContextError(err *api.APIError) *api.APIError {
	if err == nil || err.Code == api.ErrCodeServiceUnavailable {
		return err
	}
	return api.NewAPIErrorWithDetails(api.ErrorCode("previous_response_not_found"),
		err.Message, api.ErrorTypeInvalidRequest, err.Details)
}
