package proxy

import (
	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesExcelBPSNativeFallbackRoutesOriginalHistory(t *testing.T) {
	for _, agent := range []bool{false, true} {
		t.Run(map[bool]string{false: "upstream_500", true: "agent_context"}[agent], func(t *testing.T) {
			resetExcelBPSHealthForTest(t)
			oldDo, oldResin := excelBPSDo, resinCfg.Load()
			t.Cleanup(func() { excelBPSDo = oldDo; resinCfg.Store(oldResin) })
			bpsCalls, nativeCalls := 0, 0
			excelBPSDo = func(*http.Request, *auth.Account, string) (*http.Response, error) {
				bpsCalls++
				return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"server_error"}}`)), Header: make(http.Header)}, nil
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nativeCalls++
				body := readUpstreamRequestBody(r)
				if agent && (gjson.GetBytes(body, "input.0.type").String() != "agent_message" || gjson.GetBytes(body, "input.0.content.1.encrypted_content").String() != "opaque-synthetic") {
					t.Errorf("opaque agent context was lost")
				}
				if !strings.HasSuffix(r.URL.Path, "/backend-api/codex/responses") {
					t.Errorf("wrong fallback endpoint: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"native-ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-native\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
			}))
			defer upstream.Close()
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.5"})
			account := testExcelBPSAccount()
			account.PlanType = "pro"
			store.AddAccount(account)
			h := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
			router := gin.New()
			h.RegisterRoutes(router)
			raw := `{"model":"gpt-5.5","input":[{"role":"user","content":"synthetic probe"}],"stream":true}`
			if agent {
				raw = `{"model":"gpt-5.5","input":[{"type":"agent_message","author":"parent","recipient":"child","content":[{"type":"input_text","text":"synthetic probe"},{"type":"encrypted_content","encrypted_content":"opaque-synthetic"}]}],"stream":true}`
			}
			for i := 0; i < 2; i++ {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(raw))
				req.Header.Set("Content-Type", "application/json")
				router.ServeHTTP(rec, req)
				if rec.Code != 200 || !strings.Contains(rec.Body.String(), "native-ok") || strings.Contains(rec.Body.String(), "response.failed") {
					t.Fatalf("fallback response=%d %s", rec.Code, rec.Body.String())
				}
			}
			if nativeCalls != 2 {
				t.Fatalf("native calls=%d want2", nativeCalls)
			}
			if agent && bpsCalls != 0 {
				t.Fatalf("agent context reached BPS %d times", bpsCalls)
			}
			if !account.IsExcelBPSEnabled() {
				t.Fatal("fallback changed account BPS setting")
			}
		})
	}
}
