package proxy

import (
	"fmt"
	"net/http"
)

const ErrorCodeUpstreamWSConnectionCapacity = "upstream_ws_connection_capacity_exceeded"

// UpstreamWSConnectionCapacityError 是本地连接容量拒绝，不是上游账号故障。
type UpstreamWSConnectionCapacityError struct {
	Limit int
}

func (err *UpstreamWSConnectionCapacityError) Error() string {
	return fmt.Sprintf("Upstream WebSocket connection capacity reached (max %d); existing continuations are protected, please retry later", err.Limit)
}

func (err *UpstreamWSConnectionCapacityError) Unwrap() error {
	return &Error{Code: ErrorCodeUpstreamWSConnectionCapacity, Type: ErrorTypeServerError,
		HTTPStatus: http.StatusServiceUnavailable, Message: err.Error()}
}
