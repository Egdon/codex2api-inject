package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// bpsTestRateLimitedSSE is the shape Basispoints sends for its token rate
// limit: HTTP 200, lifecycle events, then an error and response.failed.
func bpsTestRateLimitedSSE() string {
	return bpsTestSSE(
		`{"type":"response.created","sequence_number":0,"response":{"id":"resp_bps_limited","status":"in_progress","output":[]}}`,
		`{"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_bps_limited","status":"in_progress","output":[]}}`,
		`{"type":"error","sequence_number":2,"error":{"type":"tokens","code":"rate_limit_exceeded","message":"PRIVATE_LIMIT_DETAIL"}}`,
		`{"type":"response.failed","sequence_number":3,"response":{"id":"resp_bps_limited","status":"failed","error":{"code":"rate_limit_exceeded","message":"PRIVATE_LIMIT_DETAIL"}}}`,
	)
}

type slowBody struct {
	io.Reader
	delay  time.Duration
	first  bool
	closed bool
}

func (s *slowBody) Read(p []byte) (int, error) {
	if !s.first {
		s.first = true
		time.Sleep(s.delay)
	}
	return s.Reader.Read(p)
}

func (s *slowBody) Close() error { s.closed = true; return nil }

func TestInspectExcelBPSStart(t *testing.T) {
	normal := bpsTestTextSSE("resp_bps_ok", "HELLO")
	unmapped := bpsTestSSE(
		`{"type":"response.created","response":{"id":"r","status":"in_progress","output":[]}}`,
		`{"type":"response.failed","response":{"id":"r","status":"failed","error":{"code":"server_error"}}}`,
	)
	for _, tc := range []struct {
		name       string
		payload    string
		delay      time.Duration
		wantStatus int
	}{
		{name: "token rate limit", payload: bpsTestRateLimitedSSE(), wantStatus: http.StatusTooManyRequests},
		{name: "overloaded", payload: bpsTestSSE(`{"type":"response.created","response":{"id":"r","status":"in_progress","output":[]}}`, `{"type":"error","error":{"code":"server_is_overloaded"}}`), wantStatus: http.StatusServiceUnavailable},
		{name: "normal output", payload: normal},
		{name: "unmapped failure reaches the bridge", payload: unmapped},
		{name: "slow first byte is not held back", payload: bpsTestRateLimitedSSE(), delay: 200 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &slowBody{Reader: strings.NewReader(tc.payload), delay: tc.delay}
			response := &http.Response{StatusCode: 200, Body: upstream}
			window := time.Second
			if tc.delay > 0 {
				window = 50 * time.Millisecond
			}
			rejected := inspectExcelBPSStart(context.Background(), response, window)
			if tc.wantStatus != 0 {
				if rejected == nil || rejected.status != tc.wantStatus {
					t.Fatalf("rejected = %+v, want status %d", rejected, tc.wantStatus)
				}
				if !upstream.closed {
					t.Fatal("rejected body was not closed")
				}
				return
			}
			if rejected != nil {
				t.Fatalf("unexpected rejection %+v", rejected)
			}
			replayed, err := io.ReadAll(response.Body)
			if err != nil || string(replayed) != tc.payload {
				t.Fatalf("replay = %q, %v; want the original stream", replayed, err)
			}
			_ = response.Body.Close()
			if !upstream.closed {
				t.Fatal("replay body did not close the upstream")
			}
		})
	}
}

func TestExcelBPSReplayBodyCloseStopsAPendingRead(t *testing.T) {
	reader, writer := io.Pipe()
	response := &http.Response{StatusCode: 200, Body: reader}
	go func() {
		_, _ = io.WriteString(writer, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
	}()
	if rejected := inspectExcelBPSStart(context.Background(), response, 50*time.Millisecond); rejected != nil {
		t.Fatalf("unexpected rejection %+v", rejected)
	}
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(response.Body)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	_ = response.Body.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not release a blocked read")
	}
}

func TestResponsesExcelBPSInStreamRateLimitFallsBackAndCools(t *testing.T) {
	setExcelBPSGlobalForTest(t, true, "")
	harness := newExcelBPSRouteHarness(t, func(int) (int, string) { return 200, bpsTestRateLimitedSSE() })
	raw := `{"model":"gpt-6-astra","input":[{"role":"user","content":"hi"}],"stream":true}`
	first := harness.post(t, raw)
	if first.Code != 200 || !strings.Contains(first.Body.String(), "native-ok") || strings.Contains(first.Body.String(), "PRIVATE_LIMIT_DETAIL") {
		t.Fatalf("first = %d %s", first.Code, first.Body.String())
	}
	if first.Header().Get("X-Codex2api-Upstream-Fallback") != "basispoints-to-codex" {
		t.Fatalf("fallback header = %q", first.Header().Get("X-Codex2api-Upstream-Fallback"))
	}
	second := harness.post(t, raw)
	if second.Code != 200 || !strings.Contains(second.Body.String(), "native-ok") {
		t.Fatalf("second = %d %s", second.Code, second.Body.String())
	}
	if len(harness.bpsBodies) != 1 || harness.nativeCalls != 2 {
		t.Fatalf("bps=%d native=%d, want 1/2 (the route cools after the rate limit)", len(harness.bpsBodies), harness.nativeCalls)
	}
}

func TestChatCompletionsExcelBPSInStreamRateLimitFallsBack(t *testing.T) {
	setExcelBPSGlobalForTest(t, true, "")
	harness := newExcelBPSRouteHarness(t, func(int) (int, string) { return 200, bpsTestRateLimitedSSE() })
	rec := harness.postPath(t, "/v1/chat/completions", `{"model":"gpt-6-astra","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "native-ok") || strings.Contains(rec.Body.String(), "PRIVATE_LIMIT_DETAIL") {
		t.Fatalf("chat = %d %s", rec.Code, rec.Body.String())
	}
	if len(harness.bpsBodies) != 1 || harness.nativeCalls != 1 {
		t.Fatalf("bps=%d native=%d, want 1/1", len(harness.bpsBodies), harness.nativeCalls)
	}
}
