package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func setExcelBPSGlobalForTest(t *testing.T, enabled bool, models string) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings {
		s.CodexBasispointsEnabled = enabled
		s.CodexBasispointsModels = models
		return s
	})
}

func TestExcelBPSRouteAvailable(t *testing.T) {
	for _, tc := range []struct {
		name            string
		global          bool
		models          string
		enabled, optOut bool
		accountModels   []string
		model           string
		want            bool
	}{
		{name: "global off, not opted in", model: "gpt-6-astra"},
		{name: "global on follows default", global: true, model: "gpt-6-astra", want: true},
		{name: "global on excluded account", global: true, optOut: true, model: "gpt-6-astra"},
		{name: "explicit account opt-in", enabled: true, model: "gpt-5.5", want: true},
		{name: "global list admits model", global: true, models: "gpt-6-astra,gpt-5.6-sol", model: "GPT-6-Astra", want: true},
		{name: "global list rejects model", global: true, models: "gpt-6-astra", model: "gpt-5.5"},
		{name: "global list also scopes opted-in accounts", enabled: true, models: "gpt-6-astra", model: "gpt-5.5"},
		{name: "account allowlist still applies", global: true, accountModels: []string{"gpt-5.5"}, model: "gpt-6-astra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setExcelBPSGlobalForTest(t, tc.global, tc.models)
			account := &auth.Account{DBID: 7, AccessToken: "synthetic", ExcelBPSEnabled: tc.enabled, ExcelBPSOptOut: tc.optOut, Models: tc.accountModels}
			if got := excelBPSRouteAvailable(account, tc.model); got != tc.want {
				t.Fatalf("excelBPSRouteAvailable = %t, want %t", got, tc.want)
			}
		})
	}
}

// bpsTestSSE renders synthetic native Basispoints events.
func bpsTestSSE(events ...string) string {
	var out strings.Builder
	for _, event := range events {
		out.WriteString("event: " + gjson.Get(event, "type").String() + "\n")
		out.WriteString("data: " + event + "\n\n")
	}
	return out.String()
}

func bpsTestTransportCall(callID, name, arguments string) string {
	code, _ := json.Marshal(`{"name":"` + name + `","arguments":` + arguments + `}`)
	args, _ := json.Marshal(`{"code":` + string(code) + `,"summary":"s","extended_summary":"e","destructive":false,"references":[]}`)
	return `{"type":"function_call","id":"fc_` + callID + `","call_id":"` + callID + `","name":"run_officejs","arguments":` + string(args) + `,"status":"completed"}`
}

type excelBPSRouteHarness struct {
	router      *gin.Engine
	account     *auth.Account
	mu          sync.Mutex
	bpsBodies   [][]byte
	nativeCalls int
}

func newExcelBPSRouteHarness(t *testing.T, bpsResponses func(n int) (int, string)) *excelBPSRouteHarness {
	t.Helper()
	harness := &excelBPSRouteHarness{}
	oldDo, oldResin := excelBPSDo, resinCfg.Load()
	t.Cleanup(func() { excelBPSDo = oldDo; resinCfg.Store(oldResin) })
	excelBPSDo = func(req *http.Request, _ *auth.Account, _ string) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		harness.mu.Lock()
		harness.bpsBodies = append(harness.bpsBodies, body)
		n := len(harness.bpsBodies)
		harness.mu.Unlock()
		status, payload := bpsResponses(n)
		header := make(http.Header)
		header.Set("Content-Type", "text/event-stream")
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(payload)), Header: header}, nil
	}
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		harness.mu.Lock()
		harness.nativeCalls++
		harness.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"native-ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-native\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	t.Cleanup(native.Close)
	SetResinConfig(&ResinConfig{BaseURL: native.URL, PlatformName: "test"})
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-6-astra"})
	// Deliberately not opted in: the global default must select it.
	harness.account = &auth.Account{DBID: 92, AccessToken: "synthetic-access-token", AccountID: "chatgpt-account", PlanType: "pro"}
	store.AddAccount(harness.account)
	h := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	harness.router = gin.New()
	h.RegisterRoutes(harness.router)
	return harness
}

func (h *excelBPSRouteHarness) post(t *testing.T, raw string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	h.router.ServeHTTP(rec, req)
	return rec
}

func TestResponsesExcelBPSGlobalDefaultKeepsPreviousResponseContinuation(t *testing.T) {
	setExcelBPSGlobalForTest(t, true, "")
	call := bpsTestTransportCall("call_weather_1", "get_weather", `{"city":"Paris"}`)
	harness := newExcelBPSRouteHarness(t, func(n int) (int, string) {
		if n == 1 {
			return 200, bpsTestSSE(
				`{"type":"response.created","response":{"id":"resp_bps_turn1","status":"in_progress","output":[]}}`,
				`{"type":"response.output_item.done","output_index":0,"item":`+call+`}`,
				`{"type":"response.completed","response":{"id":"resp_bps_turn1","status":"completed","output":[`+call+`],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}`,
			)
		}
		return 200, bpsTestSSE(
			`{"type":"response.created","response":{"id":"resp_bps_turn2","status":"in_progress","output":[]}}`,
			`{"type":"response.output_text.delta","output_index":0,"delta":"SUNNY"}`,
			`{"type":"response.completed","response":{"id":"resp_bps_turn2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"SUNNY"}]}],"usage":{"input_tokens":12,"output_tokens":1,"total_tokens":13}}}`,
		)
	})
	tools := `[{"type":"function","name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]`
	first := harness.post(t, `{"model":"gpt-6-astra","input":[{"role":"user","content":"weather in Paris?"}],"tools":`+tools+`,"stream":true}`)
	if first.Code != 200 || !strings.Contains(first.Body.String(), `"name":"get_weather"`) {
		t.Fatalf("first turn = %d %s", first.Code, first.Body.String())
	}
	second := harness.post(t, `{"model":"gpt-6-astra","previous_response_id":"resp_bps_turn1","input":[{"type":"function_call_output","call_id":"call_weather_1","output":"sunny"}],"tools":`+tools+`,"stream":true}`)
	if second.Code != 200 || !strings.Contains(second.Body.String(), "SUNNY") {
		t.Fatalf("second turn = %d %s", second.Code, second.Body.String())
	}
	if harness.nativeCalls != 0 || len(harness.bpsBodies) != 2 {
		t.Fatalf("native=%d bps=%d, want 0/2", harness.nativeCalls, len(harness.bpsBodies))
	}
	input := gjson.GetBytes(harness.bpsBodies[1], "input")
	if !input.IsArray() {
		t.Fatalf("second BPS body has no input: %s", harness.bpsBodies[1])
	}
	var sawUser, sawCall, sawOutput bool
	for _, item := range input.Array() {
		switch {
		case item.Get("role").String() == "user" && strings.Contains(item.Raw, "weather in Paris?"):
			sawUser = true
		case item.Get("type").String() == "function_call" && item.Get("call_id").String() == "call_weather_1":
			sawCall = item.Get("name").String() == "run_officejs"
		case item.Get("type").String() == "function_call_output" && item.Get("call_id").String() == "call_weather_1":
			sawOutput = true
		}
	}
	if !sawUser || !sawCall || !sawOutput {
		t.Fatalf("expanded history user=%t call=%t output=%t: %s", sawUser, sawCall, sawOutput, input.Raw)
	}
}

func TestResponsesExcelBPSGlobalModelListKeepsOtherModelsNative(t *testing.T) {
	setExcelBPSGlobalForTest(t, true, "gpt-5.6-sol")
	harness := newExcelBPSRouteHarness(t, func(int) (int, string) {
		return 500, `{"error":{"type":"server_error"}}`
	})
	rec := harness.post(t, `{"model":"gpt-6-astra","input":[{"role":"user","content":"hello"}],"stream":true}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "native-ok") {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	if len(harness.bpsBodies) != 0 || harness.nativeCalls != 1 {
		t.Fatalf("bps=%d native=%d, want 0/1", len(harness.bpsBodies), harness.nativeCalls)
	}
	if got := rec.Header().Get("X-Codex2api-Upstream-Fallback"); got != "" {
		t.Fatalf("unlisted model should not be a BPS fallback, header=%q", got)
	}
}
