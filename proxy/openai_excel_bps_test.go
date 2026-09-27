package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func testExcelBPSAccount() *auth.Account {
	return &auth.Account{DBID: 91, AccessToken: "synthetic-access-token", AccountID: "chatgpt-account", ExcelBPSEnabled: true}
}

func TestExecuteExcelBPSRequestUsesProviderHeadersAndDoesNotExposeHTTPBody(t *testing.T) {
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	called := false
	excelBPSDo = func(req *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
		called = true
		if got := req.Header.Get("Authorization"); got != "Bearer synthetic-access-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := req.Header.Get("Chatgpt-Account-Id"); got != "chatgpt-account" {
			t.Fatalf("Chatgpt-Account-Id = %q", got)
		}
		if got := req.Header.Get("X-Basispoints-Auth-Mode"); got != "chatgpt" {
			t.Fatalf("X-Basispoints-Auth-Mode = %q", got)
		}
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"secret upstream detail"}}`)), Header: make(http.Header)}, nil
	}
	_, err := ExecuteExcelBPSRequest(t.Context(), testExcelBPSAccount(), []byte(`{"model":"gpt-5.5","input":"hello"}`), "account:91/key:1/thread:1", "thread:1", "", false)
	if !called || err == nil {
		t.Fatalf("ExecuteExcelBPSRequest called=%t err=%v", called, err)
	}
	if strings.Contains(err.Error(), "secret upstream detail") {
		t.Fatalf("upstream error body leaked: %v", err)
	}
}

func TestForwardExcelBPSWritesOneTerminalFrame(t *testing.T) {
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	excelBPSDo = func(_ *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
		body := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"output\":[]}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	result, err := forwardExcelBPS(ctx, ctx, testExcelBPSAccount(), []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`), "account:91/key:1/thread:1", "thread:1", "", false, true)
	if err != nil {
		t.Fatalf("forwardExcelBPS: %v", err)
	}
	if result.Terminal != "response.completed" {
		t.Fatalf("terminal = %q", result.Terminal)
	}
	body := recorder.Body.String()
	if strings.Count(body, `"type":"response.completed"`) != 1 {
		t.Fatalf("completed event count = %d, body=%s", strings.Count(body, `"type":"response.completed"`), body)
	}
	if strings.Contains(body, "response.failed") {
		t.Fatalf("unexpected failure terminal: %s", body)
	}
}

func TestForwardExcelBPSSanitizesFailureWithoutAppendingTerminal(t *testing.T) {
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	excelBPSDo = func(_ *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
		body := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"provider detail\"}}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":true}`))
	result, err := forwardExcelBPS(ctx, ctx, testExcelBPSAccount(), []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`), "account:91/key:1/thread:1", "thread:1", "", false, true)
	if err != nil {
		t.Fatalf("forwardExcelBPS: %v", err)
	}
	if result.Terminal != "response.failed" {
		t.Fatalf("terminal = %q", result.Terminal)
	}
	body := recorder.Body.String()
	if strings.Count(body, `"type":"response.failed"`) != 1 {
		t.Fatalf("failure terminal count = %d, body=%s", strings.Count(body, `"type":"response.failed"`), body)
	}
	if strings.Contains(body, "provider detail") || strings.Contains(body, "response.completed") {
		t.Fatalf("failure body was not sanitized or gained a second terminal: %s", body)
	}
}

func TestForwardExcelBPSDoesNotTreatFailedCompletedAsSuccess(t *testing.T) {
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	excelBPSDo = func(_ *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
		body := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"provider detail\"}}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello"}`))
	result, err := forwardExcelBPS(ctx, ctx, testExcelBPSAccount(), []byte(`{"model":"gpt-5.5","input":"hello"}`), "account:91/key:1/thread:1", "thread:1", "", false, true)
	if err != nil {
		t.Fatalf("stream forwarding should preserve the terminal frame: %v", err)
	}
	if result.Terminal != "response.failed" {
		t.Fatalf("terminal = %q, want response.failed", result.Terminal)
	}
	if strings.Contains(recorder.Body.String(), "provider detail") {
		t.Fatalf("failed completed event leaked provider detail: %s", recorder.Body.String())
	}
}

func TestWriteExcelBPSFailureUsesJSONBeforeStreamingResponseIsCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	writeExcelBPSFailure(ctx, true, http.StatusBadRequest, "basispoints_request_invalid", "unsupported request")

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if body := recorder.Body.String(); strings.Contains(body, "response.failed") || !strings.Contains(body, "basispoints_request_invalid") {
		t.Fatalf("unexpected pre-commit failure body: %s", body)
	}
}

func TestWriteExcelBPSFailureUsesSSEAfterStreamingResponseIsCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	_, _ = ctx.Writer.Write([]byte("prefix"))

	writeExcelBPSFailure(ctx, true, http.StatusBadRequest, "basispoints_request_invalid", "unsupported request")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want committed status %d", recorder.Code, http.StatusOK)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "event: response.failed") || !strings.Contains(body, "basispoints_request_invalid") {
		t.Fatalf("unexpected committed failure body: %s", body)
	}
}
