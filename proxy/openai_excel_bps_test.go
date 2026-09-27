package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy/basispoints"
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
		if req.Header.Get("X-Openai-Internal-Basispoints-Office-Host") != "Excel" || req.Header.Get("X-Stainless-Runtime") != "browser:chrome" {
			t.Fatalf("Excel client headers missing: %v", req.Header)
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

func TestExcelBPSFailureInfoExplainsRejectedRequest(t *testing.T) {
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	excelBPSDo = func(*http.Request, *auth.Account, string) (*http.Response, error) {
		t.Fatal("rejected request reached the upstream")
		return nil, nil
	}
	_, err := ExecuteExcelBPSRequest(t.Context(), testExcelBPSAccount(), []byte(`{"model":"gpt-5.5","input":"hi","previous_response_id":"resp_1"}`), "account:91/key:1/thread:1", "thread:1", "", false)
	status, code, message := excelBPSFailureInfo(err)
	if status != http.StatusBadRequest || code != "basispoints_request_invalid" {
		t.Fatalf("excelBPSFailureInfo = %d %q", status, code)
	}
	if !strings.Contains(message, "requires expanded history instead of previous_response_id") {
		t.Fatalf("message does not explain the rejection: %q", message)
	}
}

type excelBPSImageStub struct {
	t          *testing.T
	uploads    int
	refuse     int
	refusal    int
	refusalMsg string
	bodies     []string
	uploadID   func(n int) string
}

func (s *excelBPSImageStub) do(req *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
	switch req.URL.String() {
	case basispoints.AttachmentsURL:
		s.uploads++
		reader, err := req.MultipartReader()
		if err != nil {
			s.t.Fatalf("attachment upload is not multipart: %v", err)
		}
		part, err := reader.NextPart()
		if err != nil || part.FormName() != "file" || part.Header.Get("Content-Type") != "image/png" {
			s.t.Fatalf("attachment part = %v, %v", part, err)
		}
		if req.Header.Get("Authorization") != "Bearer synthetic-access-token" {
			s.t.Fatalf("attachment upload is missing account auth")
		}
		body := `{"openai_file_id":"` + s.uploadID(s.uploads) + `"}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	case basispoints.ResponsesURL:
		body, _ := io.ReadAll(req.Body)
		s.bodies = append(s.bodies, string(body))
		if s.refuse > 0 {
			s.refuse--
			status, message := http.StatusUnprocessableEntity, "422: Invalid request body."
			if s.refusal != 0 {
				status, message = s.refusal, s.refusalMsg
			}
			body := `{"error":{"message":"` + message + `"}}`
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	}
	s.t.Fatalf("unexpected upstream URL %s", req.URL)
	return nil, nil
}

func excelBPSImageRequest(payload string) []byte {
	return []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"what is this"},{"type":"input_image","image_url":"data:image/png;base64,` + payload + `"}]}]}`)
}

// useExcelBPSImageStub installs stub as the upstream and isolates the
// process-wide attachment cache so tests do not depend on run order or -count.
func useExcelBPSImageStub(t *testing.T, stub *excelBPSImageStub) {
	t.Helper()
	previous := excelBPSDo
	excelBPSAttachments.reset()
	excelBPSDo = stub.do
	t.Cleanup(func() {
		excelBPSDo = previous
		excelBPSAttachments.reset()
	})
}

func TestExcelBPSUploadsMessageImagesAndFallsBackToNote(t *testing.T) {
	stub := &excelBPSImageStub{t: t, refuse: 1, uploadID: func(n int) string { return fmt.Sprintf("file-note-%d", n) }}
	useExcelBPSImageStub(t, stub)

	upstream, err := ExecuteExcelBPSRequest(t.Context(), testExcelBPSAccount(), excelBPSImageRequest("iVBORw0KGgoAAAABbm90ZQ=="), "account:91/key:1/thread:1", "thread:1", "", false)
	if err != nil {
		t.Fatalf("ExecuteExcelBPSRequest: %v", err)
	}
	upstream.Response.Body.Close()
	if stub.uploads != 1 || len(stub.bodies) != 2 {
		t.Fatalf("uploads=%d sends=%d", stub.uploads, len(stub.bodies))
	}
	if strings.Contains(stub.bodies[0], "data:image/") || !strings.Contains(stub.bodies[0], `"file_id":"file-note-1"`) {
		t.Fatalf("first attempt did not name the uploaded file: %s", stub.bodies[0])
	}
	if strings.Contains(stub.bodies[1], "file-note-1") || !strings.Contains(stub.bodies[1], "image content omitted") {
		t.Fatalf("refused image was not replaced by a note: %s", stub.bodies[1])
	}
}

func TestExcelBPSReusesUploadsAndReuploadsStaleFileIDs(t *testing.T) {
	stub := &excelBPSImageStub{t: t, uploadID: func(n int) string { return fmt.Sprintf("file-reuse-%d", n) }}
	useExcelBPSImageStub(t, stub)
	raw := excelBPSImageRequest("iVBORw0KGgoAAAABcmV1c2U=")

	for attempt := 1; attempt <= 2; attempt++ {
		upstream, err := ExecuteExcelBPSRequest(t.Context(), testExcelBPSAccount(), raw, "account:91/key:1/thread:1", "thread:1", "", false)
		if err != nil {
			t.Fatalf("request %d: %v", attempt, err)
		}
		upstream.Response.Body.Close()
	}
	if stub.uploads != 1 || !strings.Contains(stub.bodies[1], "file-reuse-1") {
		t.Fatalf("repeated image was uploaded again: uploads=%d body=%s", stub.uploads, stub.bodies[1])
	}

	stub.refuse = 1
	stub.bodies = nil
	upstream, err := ExecuteExcelBPSRequest(t.Context(), testExcelBPSAccount(), raw, "account:91/key:1/thread:1", "thread:1", "", false)
	if err != nil {
		t.Fatalf("stale request: %v", err)
	}
	upstream.Response.Body.Close()
	if stub.uploads != 2 || len(stub.bodies) != 2 || !strings.Contains(stub.bodies[0], "file-reuse-1") || !strings.Contains(stub.bodies[1], "file-reuse-2") {
		t.Fatalf("stale file ID was not re-uploaded: uploads=%d bodies=%v", stub.uploads, stub.bodies)
	}
}

func TestExcelBPSReplayBackingRoundTripsThroughRuntimeCache(t *testing.T) {
	backing := excelBPSReplayBacking{cache: cache.NewMemory(1)}
	if err := backing.Store("account:1\x00call-1", []byte(`{"raw":{"type":"function_call"}}`)); err != nil {
		t.Fatalf("Store: %v", err)
	}
	raw, ok, err := backing.Load("account:1\x00call-1")
	if err != nil || !ok || string(raw) != `{"raw":{"type":"function_call"}}` {
		t.Fatalf("Load = %s, %t, %v", raw, ok, err)
	}
	if _, ok, _ := backing.Load("account:2\x00call-1"); ok {
		t.Fatal("replay backing crossed scope")
	}
}

func TestExcelBPSDoesNotRetryImagesForUnrelatedBadRequest(t *testing.T) {
	stub := &excelBPSImageStub{t: t, refuse: 3, refusal: http.StatusBadRequest, refusalMsg: "context length exceeded", uploadID: func(n int) string { return fmt.Sprintf("file-unrelated-%d", n) }}
	useExcelBPSImageStub(t, stub)
	_, err := ExecuteExcelBPSRequest(t.Context(), testExcelBPSAccount(), excelBPSImageRequest("iVBORw0KGgoAAAABdW5yZWw="), "account:91/key:1/thread:1", "thread:1", "", false)
	status, _, _ := excelBPSFailureInfo(err)
	if status != http.StatusBadRequest || len(stub.bodies) != 1 || stub.uploads != 1 {
		t.Fatalf("unrelated 400 was retried: status=%d sends=%d uploads=%d", status, len(stub.bodies), stub.uploads)
	}
}

func TestExcelBPSErrorShapeOmitsProviderMessage(t *testing.T) {
	shape := excelBPSErrorShape([]byte(`{"error":{"message":"the user's secret prompt","code":"bad_image"}}`))
	if strings.Contains(shape, "secret") || !strings.Contains(shape, "bad_image") {
		t.Fatalf("excelBPSErrorShape = %s", shape)
	}
}

func TestForwardExcelBPSMarksSynthesizedCompletion(t *testing.T) {
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	excelBPSDo = func(_ *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
		body := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	result, err := forwardExcelBPS(ctx, ctx, testExcelBPSAccount(), []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`), "account:91/key:1/thread:1", "thread:1", "", false, true)
	if err != nil || result.Terminal != "response.completed" || !result.Synthesized {
		t.Fatalf("forwardExcelBPS = %+v, %v", result, err)
	}
}
