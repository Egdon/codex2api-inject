package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/turnstate"
	"github.com/tidwall/gjson"
)

func TestTurnStateAccountSwitchKeepsHTTPProbeObservations(t *testing.T) {
	for _, globalEnabled := range []bool{false, true} {
		t.Run(fmt.Sprint(globalEnabled), func(t *testing.T) {
			enableTurnStateTemplateCache(t)
			account := &auth.Account{DBID: 81, AccessToken: "test", PlanType: "pro", CodexTurnStateDisabled: true}
			healthy := syntheticTurnState(10, time.Now(), 1)
			degraded := syntheticTurnState(11, time.Now(), 2)
			CaptureCodexTurnStateTemplate(nil, account, "gpt-5.6-luna", http.Header{codexTurnStateHeader: []string{healthy}})
			cfg := CurrentRuntimeSettings()
			cfg.CodexTurnStateTemplateCache = globalEnabled
			ApplyRuntimeSettings(cfg)
			var sent string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sent = r.Header.Get(codexTurnStateHeader)
				w.Header().Set(codexTurnStateHeader, degraded)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			previous := resinCfg.Load()
			t.Cleanup(func() { resinCfg.Store(previous); clientPool.Delete(fmt.Sprintf("resin|%d", account.ID())) })
			SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "test"})
			clientPool.Delete(fmt.Sprintf("resin|%d", account.ID()))
			for _, model := range []string{"gpt-5.6-luna", "gpt-6-astra"} {
				for i := 0; i < 2; i++ {
					response, err := ExecuteRequest(WithCodexTurnStateAdminProbe(context.Background()), account, []byte(fmt.Sprintf(`{"model":%q,"input":"hi"}`, model)), "", "", "", nil, http.Header{}, false)
					if err != nil {
						t.Fatal(err)
					}
					_ = response.Body.Close()
					if sent != "" {
						t.Fatal("disabled probe injected state")
					}
				}
			}
			status := GetCodexTurnStateStatus(account)
			if status.InjectionEnabled || status.State != "degraded" || len(status.Models) != 2 {
				t.Fatalf("observations lost: %+v", status)
			}
			for _, model := range status.Models {
				if model.State != "degraded" || model.Consecutive != 2 {
					t.Fatalf("model observation: %+v", model)
				}
			}
			account.CodexTurnStateDisabled = false
			ctx := WithCodexTurnStateAdminProbe(context.Background())
			headers := http.Header{}
			ApplyCodexTurnStateTemplate(ctx, headers, account, "gpt-5.6-luna")
			if (headers.Get(codexTurnStateHeader) == healthy) != globalEnabled {
				t.Fatal("follow-global failed or template lost")
			}
		})
	}
}

func TestTurnStateAccountSwitchDoesNotInjectManualCredential(t *testing.T) {
	// Fork prepareCodexTurnStateInjection injects a harvest ticket only when
	// the harvest inject switch is on. Upstream v3.0.0 used this function to
	// inject account.CodexTurnState. That credential path is not the fork hot
	// path; the passive template cache applies later in ApplyCodexTurnStateTemplate.
	turnstate.SetConfig(turnstate.DefaultConfig())
	t.Cleanup(func() { turnstate.SetConfig(turnstate.DefaultConfig()) })
	enableTurnStateTemplateCache(t)
	for _, disabled := range []bool{false, true} {
		account := &auth.Account{DBID: 82, CodexTurnState: "manual-state", CodexTurnStateDisabled: disabled}
		for _, websocket := range []bool{false, true} {
			body := []byte(`{"model":"gpt-5.6-luna","client_metadata":{"x-codex-turn-state":"client-state"}}`)
			original := http.Header{codexTurnStateHeader: []string{"client-state"}}
			ctx, rewritten, headers := prepareCodexTurnStateInjection(withCodexTurnStateInjection(context.Background(), "previous-attempt"), account, body, original, websocket)
			if headers.Get(codexTurnStateHeader) != "client-state" {
				t.Fatal("manual credential replaced the client header")
			}
			if gjson.GetBytes(rewritten, "client_metadata.x-codex-turn-state").String() != "client-state" {
				t.Fatal("manual credential rewrote the frame")
			}
			if CodexTurnStateInjectionFromContext(ctx) != "previous-attempt" {
				t.Fatal("inject switch off changed the caller context")
			}
			if original.Get(codexTurnStateHeader) != "client-state" || account.CodexTurnState != "manual-state" {
				t.Fatal("prepare mutated caller headers or account config")
			}
		}
	}
}
