package admin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
)

func TestTurnStateProxyPersistenceAndIsolation(t *testing.T) {
	db := newTestAdminDB(t)
	id := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	if err := store.LoadAccountByID(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, store: store}
	for _, tc := range []struct{ body, want string }{
		{`{"proxy_url":"http://ordinary.example:8080","codex_turn_state_proxy_url":" socks5://issuance.example:1080 "}`, "socks5://issuance.example:1080"},
		{`{"codex_turn_state_models":"gpt-5.6-luna"}`, "socks5://issuance.example:1080"},
		{`{"codex_turn_state_proxy_url":"https://issuance.example:8443"}`, "https://issuance.example:8443"},
		{`{"codex_turn_state_proxy_url":null}`, ""},
		{`{"codex_turn_state_proxy_url":""}`, ""},
	} {
		response := patchAccountScheduler(t, h, id, tc.body)
		if response.Code != http.StatusOK {
			t.Fatalf("save failed: %s", response.Body.String())
		}
		row, err := db.GetAccountByID(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if row.GetCredential(auth.CodexTurnStateProxyURLCredentialKey) != tc.want || store.FindByID(id).CodexTurnStateProxy() != tc.want {
			t.Fatal("DB/runtime mismatch")
		}
		if row.ProxyURL != "http://ordinary.example:8080" || store.FindByID(id).ProxyURL != row.ProxyURL {
			t.Fatal("issuance proxy changed ordinary proxy")
		}
		if h.buildAccountResponse(row, nil, nil, nil, nil, true).CodexTurnStateProxyURL != tc.want {
			t.Fatal("response lost issuance proxy")
		}
		reloaded := auth.NewStore(db, nil, nil)
		if err := reloaded.LoadAccountByID(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		if reloaded.FindByID(id).CodexTurnStateProxy() != tc.want {
			t.Fatal("reload lost issuance proxy")
		}
	}
	for _, body := range []string{`{"codex_turn_state_proxy_url":true}`, `{"codex_turn_state_proxy_url":"file:///tmp/proxy"}`, `{"codex_turn_state_proxy_url":"http://host:99999"}`} {
		if response := patchAccountScheduler(t, h, id, body); response.Code != http.StatusBadRequest {
			t.Fatalf("accepted invalid proxy: %s", body)
		}
	}
}

func TestTurnStateProxyRefreshHandlerUsesBothStages(t *testing.T) {
	db := newTestAdminDB(t)
	proxy.SetCodexTurnStateTemplateDatabase(db)
	defer proxy.SetCodexTurnStateTemplateDatabase(nil)
	prior := proxy.CurrentRuntimeSettings()
	cfg := prior
	cfg.CodexTurnStateTemplateCache = true
	cfg.CodexForceWebsocket = true
	cfg.CodexTurnStateAccountMode = "personal"
	proxy.ApplyRuntimeSettings(cfg)
	defer proxy.ApplyRuntimeSettings(prior)
	store := auth.NewStore(db, nil, nil)
	account := &auth.Account{DBID: 9935, AccessToken: "fixture", PlanType: "pro", Models: []string{"gpt-5.6-luna"}, CodexTurnStateModels: "gpt-5.6-luna", ProxyURL: "http://ordinary.test:8080", CodexTurnStateProxyURL: "socks5://issuance.test:1080"}
	store.AddAccount(account)
	handler := &Handler{db: db, store: store}
	old := proxy.WebsocketExecuteFunc
	defer func() { proxy.WebsocketExecuteFunc = old }()
	calls := 0
	healthy := refreshTestToken(10)
	proxy.WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, body []byte, sessionID, proxyURL, apiKey string, device *proxy.DeviceProfileConfig, headers http.Header, poolKey string) (*http.Response, error) {
		calls++
		if proxyURL != "socks5://issuance.test:1080" || proxy.CodexTurnStateRefreshProxy(ctx, a) != proxyURL {
			t.Error("handler did not select dedicated proxy")
		}
		if calls == 1 {
			proxy.CaptureCodexTurnStateTemplate(ctx, a, "gpt-5.6-luna", http.Header{"X-Codex-Turn-State": []string{healthy}})
			// A concurrent settings change must not split acquisition/verification exits.
			store.ApplyAccountCodexTurnStateProxyURL(a.ID(), "http://changed.test:8080")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(antigravityTestSSEBody("ok")))}, nil
	}
	router := gin.New()
	router.POST("/accounts/:id/turn-state/refresh", handler.RefreshCodexTurnStateTemplates)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", fmt.Sprintf("/accounts/%d/turn-state/refresh", account.ID()), nil))
	if calls != 2 || !strings.Contains(response.Body.String(), `"saved":1`) {
		t.Fatalf("calls=%d response=%s", calls, response.Body.String())
	}
	if account.ProxyURL != "http://ordinary.test:8080" {
		t.Fatal("refresh modified default proxy")
	}
}
