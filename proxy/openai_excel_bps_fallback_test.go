package proxy

import (
	"context"
	"errors"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandleExcelBPSNativeFallback(t *testing.T) {
	cases := []struct {
		name, raw               string
		status                  int
		body                    string
		transport, wantFallback bool
		wantCalls               int
	}{
		{name: "agent encrypted context", raw: `{"model":"gpt-5.5","input":[{"type":"agent_message","author":"parent","recipient":"child","content":[{"type":"input_text","text":"synthetic task"},{"type":"encrypted_content","encrypted_content":"opaque-synthetic"}]}]}`, wantFallback: true},
		{name: "opaque message context", raw: `{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"encrypted_content","encrypted_content":"opaque-synthetic"}]}]}`, wantFallback: true},
		{name: "previous response", raw: `{"model":"gpt-5.5","input":"hi","previous_response_id":"resp_test"}`, wantFallback: true},
		{name: "upstream 500", status: 500, wantFallback: true, wantCalls: 1},
		{name: "upstream 502", status: 502, wantFallback: true, wantCalls: 1},
		{name: "upstream 503", status: 503, wantFallback: true, wantCalls: 1},
		{name: "upstream 504", status: 504, wantFallback: true, wantCalls: 1},
		{name: "model unavailable", status: 403, body: `{"error":{"code":"basispoints_model_access_changed"}}`, wantFallback: true, wantCalls: 1},
		{name: "transport error", transport: true, wantFallback: true, wantCalls: 1},
		{name: "invalid token stays visible", status: 401, wantCalls: 1},
		{name: "generic forbidden stays visible", status: 403, wantCalls: 1},
		{name: "ordinary BPS success", status: 200, body: "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-ok\",\"status\":\"completed\",\"output\":[]}}\n\n", wantCalls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := excelBPSDo
			t.Cleanup(func() { excelBPSDo = old })
			calls := 0
			excelBPSDo = func(*http.Request, *auth.Account, string) (*http.Response, error) {
				calls++
				if tc.transport {
					return nil, errors.New("synthetic transport failure")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			}
			raw := tc.raw
			if raw == "" {
				raw = `{"model":"gpt-5.5","input":"synthetic probe"}`
			}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
			var h *Handler
			h.handleExcelBPS(c, testExcelBPSAccount(), []byte(raw), t.Name(), t.Name(), "", false, false, false, "/v1/responses", "gpt-5.5", "gpt-5.5", "medium", "", auth.SessionAffinityGuard{}, time.Now())
			reason := c.GetString("codex2api.excel_bps_native_fallback")
			if (reason != "") != tc.wantFallback {
				t.Fatalf("fallback=%q want=%t; status=%d body=%s", reason, tc.wantFallback, rec.Code, rec.Body.String())
			}
			if tc.wantFallback && (c.Writer.Written() || rec.Body.Len() != 0) {
				t.Fatalf("fallback committed a response: %s", rec.Body.String())
			}
			if calls != tc.wantCalls {
				t.Fatalf("BPS calls=%d want=%d", calls, tc.wantCalls)
			}
		})
	}
}
func TestHandleExcelBPSDoesNotFallbackAfterOutput(t *testing.T) {
	old := excelBPSDo
	t.Cleanup(func() { excelBPSDo = old })
	excelBPSDo = func(*http.Request, *auth.Account, string) (*http.Response, error) {
		body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"already sent\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"synthetic failure\"}}}\n\n"
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	var h *Handler
	h.handleExcelBPS(c, testExcelBPSAccount(), []byte(`{"model":"gpt-5.5","input":"probe","stream":true}`), t.Name(), t.Name(), "", false, true, false, "/v1/responses", "gpt-5.5", "gpt-5.5", "medium", "", auth.SessionAffinityGuard{}, time.Now())
	if c.GetString("codex2api.excel_bps_native_fallback") != "" {
		t.Fatal("must not replay after output")
	}
	if strings.Count(rec.Body.String(), `"type":"response.failed"`) != 1 {
		t.Fatalf("unexpected terminal: %s", rec.Body.String())
	}
}
func TestHandleExcelBPSCanceledRequestDoesNotFallback(t *testing.T) {
	old := excelBPSDo
	t.Cleanup(func() { excelBPSDo = old })
	excelBPSDo = func(*http.Request, *auth.Account, string) (*http.Response, error) { return nil, context.Canceled }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	var h *Handler
	h.handleExcelBPS(c, testExcelBPSAccount(), []byte(`{"model":"gpt-5.5","input":"probe"}`), t.Name(), t.Name(), "", false, true, false, "/v1/responses", "gpt-5.5", "gpt-5.5", "medium", "", auth.SessionAffinityGuard{}, time.Now())
	if c.GetString("codex2api.excel_bps_native_fallback") != "" {
		t.Fatal("canceled request retried")
	}
}
