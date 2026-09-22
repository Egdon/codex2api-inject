package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestTurnStateAccountSwitchPersistenceAndRuntime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	id := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	if err := store.LoadAccountByID(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, store: store}
	if store.FindByID(id).IsCodexTurnStateDisabled() {
		t.Fatal("default must follow global")
	}
	for _, tc := range []struct {
		body     string
		disabled bool
	}{
		{`{"codex_turn_state_disabled":true,"codex_turn_state":"saved-state","codex_turn_state_models":"gpt-5.6-*"}`, true},
		{`{"codex_turn_state_disabled":false}`, false},
		{`{"codex_turn_state_disabled":true}`, true},
		{`{"codex_turn_state_disabled":null}`, false},
	} {
		response := patchAccountScheduler(t, h, id, tc.body)
		if response.Code != http.StatusOK {
			t.Fatalf("save: %s", response.Body.String())
		}
		row, err := db.GetAccountByID(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if row.GetCredentialBool(auth.CodexTurnStateDisabledCredentialKey) != tc.disabled || store.FindByID(id).IsCodexTurnStateDisabled() != tc.disabled {
			t.Fatal("DB/runtime mismatch")
		}
		if h.buildAccountResponse(row, nil, nil, nil, nil, true).CodexTurnStateDisabled != tc.disabled {
			t.Fatal("response mismatch")
		}
		value, scope, _ := store.FindByID(id).CodexTurnStateConfig()
		if value != "saved-state" || scope != "gpt-5.6-*" {
			t.Fatal("switch destroyed saved state/scope")
		}
		reloaded := auth.NewStore(db, nil, nil)
		if err := reloaded.LoadAccountByID(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		if reloaded.FindByID(id).IsCodexTurnStateDisabled() != tc.disabled {
			t.Fatal("restart lost switch")
		}
	}
	if response := patchAccountScheduler(t, h, id, `{"codex_turn_state_disabled":"true"}`); response.Code != http.StatusBadRequest {
		t.Fatal("invalid switch accepted")
	}
}

func TestTurnStateDisabledConnectionTestsStillObserveEachModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := proxy.CurrentRuntimeSettings()
	defer proxy.ApplyRuntimeSettings(previous)
	cfg := previous
	cfg.CodexTurnStateTemplateCache = true
	cfg.CodexForceWebsocket = false
	proxy.ApplyRuntimeSettings(cfg)
	account := &auth.Account{DBID: 83, AccessToken: "test", PlanType: "pro", CodexTurnStateDisabled: true, CodexTurnState: "manual-state"}
	proxy.ClearCodexTurnStateTemplatesForAccount(account.ID())
	defer proxy.ClearCodexTurnStateTemplatesForAccount(account.ID())
	store := auth.NewStore(nil, nil, nil)
	store.AddAccount(account)
	healthy := refreshTestToken(10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Codex-Turn-State") != "" {
			t.Error("disabled connection test injected state")
		}
		w.Header().Set("X-Codex-Turn-State", healthy)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, antigravityTestSSEBody("ok"))
	}))
	defer server.Close()
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "test"})
	defer proxy.SetResinConfig(nil)
	h := &Handler{store: store}
	router := gin.New()
	router.GET("/accounts/:id/test", h.TestConnection)
	for _, model := range []string{"gpt-5.6-luna", "gpt-6-astra"} {
		for i := 0; i < 2; i++ {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("GET", "/accounts/83/test?model="+model, nil))
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"success":true`) {
				t.Fatalf("connection test failed: %s", response.Body.String())
			}
		}
	}
	status := proxy.GetCodexTurnStateStatus(account)
	if status.State != "healthy" || status.InjectionEnabled || len(status.Models) != 2 {
		t.Fatalf("missing observations: %+v", status)
	}
	for _, model := range status.Models {
		if model.Consecutive != 2 || model.Length != len(healthy) {
			t.Fatalf("model state missing: %+v", model)
		}
	}
}
