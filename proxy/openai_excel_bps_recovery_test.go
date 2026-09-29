package proxy

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const bpsInvalidEncryptedBody = `{"error":{"code":"invalid_encrypted_content","message":"The encrypted content could not be verified."}}`

func TestResponsesExcelBPSRetriesOnceWithoutUnverifiableReasoning(t *testing.T) {
	setExcelBPSGlobalForTest(t, true, "")
	harness := newExcelBPSRouteHarness(t, func(n int) (int, string) {
		if n == 1 {
			return 400, bpsInvalidEncryptedBody
		}
		return 200, bpsTestTextSSE("resp_bps_retry", "RETRIED")
	})
	raw := `{"model":"gpt-6-astra","input":[{"role":"user","content":"hi"},{"type":"reasoning","id":"rs_native_1","summary":[],"encrypted_content":"gAAAAnative-opaque"},{"role":"assistant","content":"earlier"},{"role":"user","content":"again"}],"stream":true}`
	rec := harness.post(t, raw)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "RETRIED") {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	if len(harness.bpsBodies) != 2 || harness.nativeCalls != 0 {
		t.Fatalf("bps=%d native=%d, want 2/0", len(harness.bpsBodies), harness.nativeCalls)
	}
	if !strings.Contains(string(harness.bpsBodies[0]), "native-opaque") || strings.Contains(string(harness.bpsBodies[1]), "native-opaque") {
		t.Fatal("retry did not drop only the unverifiable reasoning")
	}
}

func TestResponsesExcelBPSFallsBackWhenEncryptedContextStillFails(t *testing.T) {
	setExcelBPSGlobalForTest(t, true, "")
	harness := newExcelBPSRouteHarness(t, func(int) (int, string) { return 400, bpsInvalidEncryptedBody })
	raw := `{"model":"gpt-6-astra","input":[{"role":"user","content":"hi"},{"type":"reasoning","id":"rs_native_1","summary":[],"encrypted_content":"gAAAAnative-opaque"},{"role":"user","content":"again"}],"stream":true}`
	rec := harness.post(t, raw)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "native-ok") {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	if len(harness.bpsBodies) != 2 || harness.nativeCalls != 1 {
		t.Fatalf("bps=%d native=%d, want 2/1", len(harness.bpsBodies), harness.nativeCalls)
	}
}

func excelBPSHostedToolReasonForTest(raw []byte) string {
	if reason := excelBPSImageIntentReason(raw); reason != "" {
		return reason
	}
	return excelBPSLiveWebSearchReason(raw)
}

func TestExcelBPSHostedToolReason(t *testing.T) {
	for raw, want := range map[string]string{
		`{"model":"gpt-6-astra","input":"hi","tools":[{"type":"web_search","external_web_access":false}]}`:                     "",
		`{"model":"gpt-6-astra","input":"hi","tools":[{"type":"web_search","external_web_access":true}]}`:                      "web_search",
		`{"model":"gpt-6-astra","input":"hi","tools":[{"type":"web_search_preview","search_context_size":"high"}]}`:            "web_search",
		`{"model":"gpt-6-astra","input":"hi","tools":[{"type":"web_search","external_web_access":true}],"tool_choice":"none"}`: "",
		`{"model":"gpt-6-astra","input":"hi","tools":[{"type":"image_generation"}],"tool_choice":{"type":"image_generation"}}`: "image_generation",
		`{"model":"gpt-6-astra","input":"hi","tools":[{"type":"image_generation"}]}`:                                           "",
		`{"model":"gpt-6-astra","input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`:         "",
	} {
		if got := excelBPSHostedToolReasonForTest([]byte(raw)); got != want {
			t.Fatalf("excelBPSHostedToolReasonForTest(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestResponsesExcelBPSKeepsLiveWebSearchNativeAndDropsUnusedImageTool(t *testing.T) {
	setExcelBPSGlobalForTest(t, true, "")
	harness := newExcelBPSRouteHarness(t, func(int) (int, string) { return 200, bpsTestTextSSE("resp_bps_tools", "BPS-OK") })
	live := harness.post(t, `{"model":"gpt-6-astra","input":[{"role":"user","content":"news"}],"tools":[{"type":"web_search","external_web_access":true}],"stream":true}`)
	if live.Code != 200 || !strings.Contains(live.Body.String(), "native-ok") || len(harness.bpsBodies) != 0 {
		t.Fatalf("live web search = %d %s (bps=%d)", live.Code, live.Body.String(), len(harness.bpsBodies))
	}
	plain := harness.post(t, `{"model":"gpt-6-astra","input":[{"role":"user","content":"explain goroutines"}],"tools":[{"type":"image_generation"}],"stream":true}`)
	if plain.Code != 200 || !strings.Contains(plain.Body.String(), "BPS-OK") || len(harness.bpsBodies) != 1 {
		t.Fatalf("plain request = %d %s (bps=%d)", plain.Code, plain.Body.String(), len(harness.bpsBodies))
	}
	if body := string(harness.bpsBodies[0]); strings.Contains(body, "image_generation") || strings.Contains(body, "Hosted tools unavailable") {
		t.Fatalf("unused image tool reached the Basispoints prompt: %s", body)
	}
}

func TestChatCompletionsKeepsRequiredWebSearchNative(t *testing.T) {
	setExcelBPSGlobalForTest(t, true, "")
	harness := newExcelBPSRouteHarness(t, func(int) (int, string) { return 200, bpsTestTextSSE("resp_bps_chat_search", "BPS-OK") })
	rec := harness.postPath(t, "/v1/chat/completions", `{"model":"gpt-6-astra","messages":[{"role":"user","content":"news"}],"tools":[{"type":"web_search_preview","search_context_size":"high"}],"stream":true}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "native-ok") {
		t.Fatalf("chat response = %d %s", rec.Code, rec.Body.String())
	}
	if len(harness.bpsBodies) != 0 || harness.nativeCalls != 1 {
		t.Fatalf("bps=%d native=%d, want 0/1", len(harness.bpsBodies), harness.nativeCalls)
	}
}

func TestForwardExcelBPSRecordsTheTierBasispointsRan(t *testing.T) {
	setExcelBPSGlobalForTest(t, true, "")
	harness := newExcelBPSRouteHarness(t, func(int) (int, string) { return 200, bpsTestTextSSE("resp_bps_effort", "OK") })
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(""))
	result, err := forwardExcelBPS(c.Request.Context(), c, harness.account, []byte(`{"model":"gpt-6-astra","input":"hi","reasoning":{"effort":"max"}}`), t.Name(), t.Name(), "", false, true, false)
	if err != nil || result.Effort != "xhigh" {
		t.Fatalf("effort = %q, err = %v", result.Effort, err)
	}
	if got := gjson.GetBytes(harness.bpsBodies[0], "reasoning_effort").String(); got != "xhigh" {
		t.Fatalf("wire effort = %q", got)
	}
}
