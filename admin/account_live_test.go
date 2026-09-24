package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func TestGetAccountLiveStateReturnsVisibleInflightCounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{DBID: 42, AccessToken: "token"}
	atomic.StoreInt64(&account.ActiveRequests, 3)
	atomic.StoreInt64(&account.OccupiedRequests, 5)
	store.AddAccount(account)
	store.SetSessionSlotBufferEnabled(true)
	handler := &Handler{store: store}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/live?ids=42,99", nil)
	handler.GetAccountLiveState(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Accounts                 map[string]accountLiveItem `json:"accounts"`
		SessionSlotBufferEnabled bool                       `json:"session_slot_buffer_enabled"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if got := response.Accounts["42"].ActiveRequests; got != 3 {
		t.Fatalf("active_requests = %d, want 3", got)
	}
	if got := response.Accounts["42"].OccupiedRequests; got != 5 {
		t.Fatalf("occupied_requests = %d, want 5", got)
	}
	if !response.SessionSlotBufferEnabled {
		t.Fatal("session slot buffer enabled state was not returned")
	}
	if _, exists := response.Accounts["99"]; exists {
		t.Fatal("missing account unexpectedly returned")
	}
}
