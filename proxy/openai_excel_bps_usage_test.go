package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// bpsCacheWriteUsage is the usage shape Basispoints reports on the first turn
// of a cache window: input_tokens already includes the cache write.
const bpsCacheWriteUsage = `{"input_tokens":22568,"input_tokens_details":{"cache_write_tokens":22500,"cached_tokens":0},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":22570}`

func bpsCacheWriteSSE(responseID string) string {
	message := `{"type":"message","id":"msg_` + responseID + `","role":"assistant","status":"completed","content":[{"type":"output_text","text":"BPS-USAGE","annotations":[]}]}`
	return bpsTestSSE(
		`{"type":"response.created","response":{"id":"`+responseID+`","status":"in_progress","output":[],"usage":null}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_`+responseID+`","role":"assistant","status":"in_progress","content":[]}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_`+responseID+`","delta":"BPS-USAGE"}`,
		`{"type":"response.output_item.done","output_index":0,"item":`+message+`}`,
		`{"type":"response.completed","response":{"id":"`+responseID+`","status":"completed","output":[`+message+`],"usage":`+bpsCacheWriteUsage+`}}`,
	)
}

func setExcelBPSCacheWriteAsInputForTest(t *testing.T, asInput bool) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
		s.CodexBasispointsCacheWriteAsInput = asInput
		return s
	})
}

// completedUsage returns the usage of the last terminal response in an SSE or
// JSON body, as a client would read it.
func completedUsage(t *testing.T, body string) gjson.Result {
	t.Helper()
	if gjson.Valid(body) {
		if usage := gjson.Get(body, "usage"); usage.Exists() {
			return usage
		}
	}
	var usage gjson.Result
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if gjson.Get(data, "type").String() == "response.completed" {
			usage = gjson.Get(data, "response.usage")
		}
	}
	if !usage.Exists() {
		t.Fatalf("no completed usage in body: %s", body)
	}
	return usage
}

func assertBPSUsageAsInput(t *testing.T, usage gjson.Result, keep bool) {
	t.Helper()
	if usage.Get("input_tokens").Int() != 22568 || usage.Get("input_tokens_details.cached_tokens").Int() != 0 ||
		usage.Get("output_tokens").Int() != 2 || usage.Get("total_tokens").Int() != 22570 {
		t.Fatalf("totals changed: %s", usage.Raw)
	}
	want := int64(0)
	if keep {
		want = 22500
	}
	if got := usage.Get("input_tokens_details.cache_write_tokens"); !got.Exists() || got.Int() != want {
		t.Fatalf("cache_write_tokens = %s, want %d (usage=%s)", got.Raw, want, usage.Raw)
	}
}

func TestResponsesExcelBPSPassesCacheWritesThroughByDefault(t *testing.T) {
	setExcelBPSGlobalForTest(t, true, "")
	if CurrentRuntimeSettings().CodexBasispointsCacheWriteAsInput {
		t.Fatal("cache creation is billed as input without being enabled")
	}
	harness := newExcelBPSRouteHarness(t, func(int) (int, string) { return 200, bpsCacheWriteSSE("resp_bps_usage_default") })
	rec := harness.postPath(t, "/v1/responses", `{"model":"gpt-6-astra","input":"hello","stream":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	assertBPSUsageAsInput(t, completedUsage(t, rec.Body.String()), true)
}

func TestResponsesExcelBPSReportsCacheWritesAsInput(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
	}{
		{name: "stream", path: "/v1/responses", body: `{"model":"gpt-6-astra","input":"hello","stream":true}`},
		{name: "non-stream", path: "/v1/responses", body: `{"model":"gpt-6-astra","input":"hello","stream":false}`},
		{name: "compact", path: "/v1/responses/compact", body: `{"model":"gpt-6-astra","input":"hello"}`},
	} {
		for _, keep := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/as-input", true: "/keep"}[keep], func(t *testing.T) {
				setExcelBPSGlobalForTest(t, true, "")
				setExcelBPSCacheWriteAsInputForTest(t, !keep)
				harness := newExcelBPSRouteHarness(t, func(int) (int, string) { return 200, bpsCacheWriteSSE("resp_bps_usage") })
				rec := harness.postPath(t, tc.path, tc.body)
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "BPS-USAGE") {
					t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
				}
				if len(harness.bpsBodies) != 1 || harness.nativeCalls != 0 {
					t.Fatalf("bps=%d native=%d, want 1/0", len(harness.bpsBodies), harness.nativeCalls)
				}
				assertBPSUsageAsInput(t, completedUsage(t, rec.Body.String()), keep)
			})
		}
	}
}

func TestChatAndMessagesExcelBPSBillCacheWritesAsInput(t *testing.T) {
	setExcelBPSGlobalForTest(t, true, "")
	setExcelBPSCacheWriteAsInputForTest(t, true)
	harness := newExcelBPSRouteHarness(t, func(int) (int, string) { return 200, bpsCacheWriteSSE("resp_bps_usage") })

	chat := harness.postPath(t, "/v1/chat/completions", `{"model":"gpt-6-astra","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	if chat.Code != http.StatusOK {
		t.Fatalf("chat = %d %s", chat.Code, chat.Body.String())
	}
	usage := gjson.Get(chat.Body.String(), "usage")
	if usage.Get("prompt_tokens").Int() != 22568 || usage.Get("prompt_tokens_details.cached_tokens").Int() != 0 || strings.Contains(usage.Raw, "cache_write") || strings.Contains(usage.Raw, "cache_creation") {
		t.Fatalf("chat usage = %s", usage.Raw)
	}

	messages := harness.postPath(t, "/v1/messages", `{"model":"gpt-6-astra","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	if messages.Code != http.StatusOK {
		t.Fatalf("messages = %d %s", messages.Code, messages.Body.String())
	}
	usage = gjson.Get(messages.Body.String(), "usage")
	if usage.Get("input_tokens").Int() != 22568 || usage.Get("cache_creation_input_tokens").Int() != 0 || usage.Get("cache_read_input_tokens").Int() != 0 {
		t.Fatalf("messages usage = %s", usage.Raw)
	}
}

func TestResponsesWebSocketExcelBPSReportsCacheWritesAsInput(t *testing.T) {
	setExcelBPSGlobalForTest(t, true, "")
	setExcelBPSCacheWriteAsInputForTest(t, true)
	harness := newExcelBPSRouteHarness(t, func(int) (int, string) { return 200, bpsCacheWriteSSE("resp_bps_ws_usage") })
	server := httptest.NewServer(harness.router)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read websocket: %v", err)
		}
		switch gjson.GetBytes(message, "type").String() {
		case "response.completed":
			assertBPSUsageAsInput(t, gjson.GetBytes(message, "response.usage"), false)
			harness.mu.Lock()
			defer harness.mu.Unlock()
			if harness.nativeCalls != 0 || len(harness.bpsBodies) != 1 {
				t.Fatalf("native=%d bps=%d, want 0/1", harness.nativeCalls, len(harness.bpsBodies))
			}
			return
		case "response.failed", "error":
			t.Fatalf("websocket turn failed: %s", message)
		}
	}
}

// The gateway's own usage row already bills cache creation as ordinary input
// (input minus cached), so the client-facing change leaves it untouched.
func TestForwardExcelBPSKeepsInternalUsageCounts(t *testing.T) {
	setExcelBPSCacheWriteAsInputForTest(t, true)
	previous := excelBPSDo
	t.Cleanup(func() { excelBPSDo = previous })
	excelBPSDo = func(_ *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Type", "text/event-stream")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(bpsCacheWriteSSE("resp_internal"))), Header: header}, nil
	}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	raw := `{"model":"gpt-6-astra","input":"hello","stream":true}`
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
	result, err := forwardExcelBPS(ctx, ctx, testExcelBPSAccount(), []byte(raw), "account:91/key:1/thread:1", "thread:1", "", false, true, false)
	if err != nil {
		t.Fatalf("forwardExcelBPS: %v", err)
	}
	if result.PromptTokens != 22568 || result.CachedTokens != 0 || result.CompletionTokens != 2 || result.TotalTokens != 22570 {
		t.Fatalf("internal usage = %+v", result)
	}
	assertBPSUsageAsInput(t, completedUsage(t, recorder.Body.String()), false)
}
