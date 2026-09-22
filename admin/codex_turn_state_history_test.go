package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestTurnStateHistoryEndpointFiltersAndValidates(t *testing.T) {
	db := newTestAdminDB(t)
	id, err := db.StartCodexTurnStateHistory(context.Background(), database.CodexTurnStateRenewalRecord{AccountID: 37, AccountName: "fixture", PlanType: "pro", Model: "gpt-5.6-luna", Attempt: 2, MaxAttempts: 10, ProxyURL: "socks5://proxy.example:1080", StartedAt: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishCodexTurnStateHistory(context.Background(), id, "success", "renewed", 2000, 1000, 3600000); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db}
	router := gin.New()
	router.GET("/history", h.ListCodexTurnStateHistory)
	for _, query := range []string{"?status=success&account_id=37&model=gpt-5.6-luna", "?proxy_url=socks5%3A%2F%2Fproxy.example%3A1080"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("GET", "/history"+query, nil))
		var page database.CodexTurnStateHistoryPage
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil || page.Total != 1 {
			t.Fatalf("response: %s", response.Body.String())
		}
	}
	for _, query := range []string{"?account_id=bad", "?account_id=-1", "?status=invalid", "?page=1000001"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("GET", "/history"+query, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid filter accepted: %s", query)
		}
	}
}
