package turnstate

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

type probeFailureKind uint8

const (
	probeProtocol probeFailureKind = iota
	probeNetwork
	probeHTTP
	probeOverload
	probeProxyAuth
	probeProxyConfig
	probeProxyConnect
)

type probeFailure struct {
	kind       probeFailureKind
	status     int
	proxyCode  int // Known Litport CONNECT codes only; never raw header text.
	retryAfter time.Duration
}

func (e *probeFailure) Error() string {
	if e.kind == probeOverload {
		return "overload"
	}
	return fmt.Sprintf("probe status %d", e.status)
}
func boundedRetryAfter(h http.Header) time.Duration {
	raw := h.Get("Retry-After")
	d := time.Duration(0)
	if n, err := strconv.Atoi(raw); err == nil {
		d = time.Duration(min(max(n, 0), 300)) * time.Second
	} else if at, err := http.ParseTime(raw); err == nil {
		d = time.Until(at)
	}
	return min(max(d, 0), 5*time.Minute)
}
func classifyRetry(err error, h http.Header) (probeFailureKind, time.Duration) {
	var failure *probeFailure
	if errors.As(err, &failure) {
		return failure.kind, max(failure.retryAfter, boundedRetryAfter(h))
	}
	var network net.Error
	if errors.As(err, &network) {
		return probeNetwork, boundedRetryAfter(h)
	}
	return probeProtocol, boundedRetryAfter(h)
}
func (r attemptResult) delay(attempt int) time.Duration {
	d := r.retryAfter
	if r.retryKind == probeNetwork || r.retryKind == probeOverload || r.retryKind == probeHTTP || r.retryKind == probeProxyConnect {
		d = max(d, retryDelay(attempt))
	}
	return d
}
