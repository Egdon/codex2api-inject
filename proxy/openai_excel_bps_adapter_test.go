package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func bpsTestTextSSE(responseID, text string) string {
	message := `{"type":"message","id":"msg_` + responseID + `","role":"assistant","status":"completed","content":[{"type":"output_text","text":"` + text + `","annotations":[]}]}`
	return bpsTestSSE(
		`{"type":"response.created","response":{"id":"`+responseID+`","status":"in_progress","output":[],"instructions":"excel server prompt","tools":[{"type":"function","name":"run_officejs"}]}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_`+responseID+`","role":"assistant","status":"in_progress","content":[]}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_`+responseID+`","delta":"`+text+`"}`,
		`{"type":"response.output_item.done","output_index":0,"item":`+message+`}`,
		`{"type":"response.completed","response":{"id":"`+responseID+`","status":"completed","output":[`+message+`],"instructions":"excel server prompt","tools":[{"type":"function","name":"run_officejs"}],"usage":{"input_tokens":20,"output_tokens":2,"total_tokens":22}}}`,
	)
}

func (h *excelBPSRouteHarness) postPath(t *testing.T, path, raw string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	h.router.ServeHTTP(rec, req)
	return rec
}

func TestChatCompletionsUsesExcelBPSAdapter(t *testing.T) {
	setExcelBPSGlobalForTest(t, true, "")
	harness := newExcelBPSRouteHarness(t, func(int) (int, string) { return 200, bpsTestTextSSE("resp_bps_chat", "BPS-CHAT") })
	rec := harness.postPath(t, "/v1/chat/completions", `{"model":"gpt-6-astra","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "BPS-CHAT") || !strings.Contains(rec.Body.String(), "chat.completion.chunk") {
		t.Fatalf("chat response = %d %s", rec.Code, rec.Body.String())
	}
	if len(harness.bpsBodies) != 1 || harness.nativeCalls != 0 {
		t.Fatalf("bps=%d native=%d, want 1/0", len(harness.bpsBodies), harness.nativeCalls)
	}
	if strings.Contains(rec.Body.String(), "excel server prompt") || strings.Contains(rec.Body.String(), "run_officejs") {
		t.Fatalf("Excel server configuration leaked: %s", rec.Body.String())
	}
}

func TestChatCompletionsExcelBPSFallsBackToNativeOnce(t *testing.T) {
	setExcelBPSGlobalForTest(t, true, "")
	harness := newExcelBPSRouteHarness(t, func(int) (int, string) { return 502, `{"error":{"type":"server_error"}}` })
	rec := harness.postPath(t, "/v1/chat/completions", `{"model":"gpt-6-astra","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "native-ok") {
		t.Fatalf("chat response = %d %s", rec.Code, rec.Body.String())
	}
	if len(harness.bpsBodies) != 1 || harness.nativeCalls != 1 {
		t.Fatalf("bps=%d native=%d, want 1/1", len(harness.bpsBodies), harness.nativeCalls)
	}
}

func TestChatCompletionsExcelBPSVisibleFailureDoesNotReachNative(t *testing.T) {
	setExcelBPSGlobalForTest(t, true, "")
	harness := newExcelBPSRouteHarness(t, func(int) (int, string) {
		return 403, `{"error":{"code":"policy","message":"blocked by synthetic policy"}}`
	})
	rec := harness.postPath(t, "/v1/chat/completions", `{"model":"gpt-6-astra","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "basispoints_upstream_error") {
		t.Fatalf("chat response = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "blocked by synthetic policy") {
		t.Fatalf("upstream body leaked: %s", rec.Body.String())
	}
	if len(harness.bpsBodies) != 1 || harness.nativeCalls != 0 {
		t.Fatalf("bps=%d native=%d, want 1/0", len(harness.bpsBodies), harness.nativeCalls)
	}
}

func TestMessagesUsesExcelBPSAdapter(t *testing.T) {
	setExcelBPSGlobalForTest(t, true, "")
	harness := newExcelBPSRouteHarness(t, func(int) (int, string) { return 200, bpsTestTextSSE("resp_bps_msg", "BPS-MSG") })
	rec := harness.postPath(t, "/v1/messages", `{"model":"gpt-6-astra","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "BPS-MSG") {
		t.Fatalf("messages response = %d %s", rec.Code, rec.Body.String())
	}
	if len(harness.bpsBodies) != 1 || harness.nativeCalls != 0 {
		t.Fatalf("bps=%d native=%d, want 1/0", len(harness.bpsBodies), harness.nativeCalls)
	}
}

func TestResponsesWebSocketUsesExcelBPSAndExpandsContinuation(t *testing.T) {
	setExcelBPSGlobalForTest(t, true, "")
	call := bpsTestTransportCall("call_ws_1", "get_weather", `{"city":"Paris"}`)
	harness := newExcelBPSRouteHarness(t, func(n int) (int, string) {
		if n == 1 {
			return 200, bpsTestSSE(
				`{"type":"response.created","response":{"id":"resp_bps_ws1","status":"in_progress","output":[]}}`,
				`{"type":"response.output_item.done","output_index":0,"item":`+call+`}`,
				`{"type":"response.completed","response":{"id":"resp_bps_ws1","status":"completed","output":[`+call+`],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}`,
			)
		}
		return 200, bpsTestTextSSE("resp_bps_ws2", "SUNNY")
	})
	server := httptest.NewServer(harness.router)
	defer server.Close()
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial websocket: %v status=%d", err, response.StatusCode)
		}
		t.Fatal(err)
	}
	defer conn.Close()
	readUntilTerminal := func() string {
		t.Helper()
		var frames strings.Builder
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("read websocket: %v (frames=%s)", err, frames.String())
			}
			frames.Write(message)
			frames.WriteByte('\n')
			switch gjson.GetBytes(message, "type").String() {
			case "response.completed", "response.failed", "error":
				return frames.String()
			}
		}
	}
	tools := `[{"type":"function","name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"weather in Paris?"}]}],"tools":`+tools+`}`)); err != nil {
		t.Fatal(err)
	}
	if frames := readUntilTerminal(); !strings.Contains(frames, `"get_weather"`) || !strings.Contains(frames, "response.completed") {
		t.Fatalf("first turn frames = %s", frames)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra","previous_response_id":"resp_bps_ws1","input":[{"type":"function_call_output","call_id":"call_ws_1","output":"sunny"}],"tools":`+tools+`}`)); err != nil {
		t.Fatal(err)
	}
	if frames := readUntilTerminal(); !strings.Contains(frames, "SUNNY") {
		t.Fatalf("second turn frames = %s", frames)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.nativeCalls != 0 || len(harness.bpsBodies) != 2 {
		t.Fatalf("native=%d bps=%d, want 0/2", harness.nativeCalls, len(harness.bpsBodies))
	}
	second := gjson.GetBytes(harness.bpsBodies[1], "input").Raw
	if !strings.Contains(second, "weather in Paris?") || !strings.Contains(second, `"call_id":"call_ws_1"`) {
		t.Fatalf("continuation was not expanded before Basispoints: %s", second)
	}
}
