package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestBPSSettingsPersistenceAndFailurePublication(t *testing.T) {
	oldMaster, oldRuntime := auth.OpenAIExcelBPSEnabled(), proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { auth.SetOpenAIExcelBPSEnabled(oldMaster); proxy.ApplyRuntimeSettings(oldRuntime) })
	auth.SetOpenAIExcelBPSEnabled(true)
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	h := NewHandler(store, db, tc, proxy.NewRateLimiter(0), "synthetic-admin")
	request := func(method, body string, ctx context.Context) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(method, "/api/admin/settings", strings.NewReader(body)).WithContext(ctx)
		c.Request.Header.Set("Content-Type", "application/json")
		if method == http.MethodGet {
			h.GetSettings(c)
		} else {
			h.UpdateSettings(c)
		}
		return w
	}
	check := func(w *httptest.ResponseRecorder, want bool) {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var response map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		var got bool
		if err := json.Unmarshal(response["openai_excel_bps_enabled"], &got); err != nil || got != want {
			t.Fatalf("master=%v err=%v", got, err)
		}
	}
	check(request(http.MethodGet, "", context.Background()), true)
	check(request(http.MethodPut, `{"openai_excel_bps_enabled":false}`, context.Background()), false)
	if auth.OpenAIExcelBPSEnabled() || proxy.CurrentRuntimeSettings().OpenAIExcelBPSEnabled {
		t.Fatal("saved false not published")
	}
	check(request(http.MethodPut, `{"site_name":"unrelated"}`, context.Background()), false)
	if saved, err := db.GetOpenAIExcelBPSEnabled(context.Background()); err != nil || saved {
		t.Fatalf("persisted=%t err=%v", saved, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if w := request(http.MethodPut, `{"openai_excel_bps_enabled":true}`, cancelled); w.Code != http.StatusInternalServerError {
		t.Fatalf("cancel status=%d", w.Code)
	}
	if auth.OpenAIExcelBPSEnabled() {
		t.Fatal("failed persistence published true")
	}
	if w := request(http.MethodPut, `{"openai_excel_bps_enabled":"yes"}`, context.Background()); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid bool status=%d", w.Code)
	}
}

func TestBPSAccountEnableEligibilityAndBulkPartialResults(t *testing.T) {
	db := newTestAdminDB(t)
	ctx := context.Background()
	oauth, err := db.InsertAccount(ctx, "ordinary", "synthetic-refresh", "")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := db.InsertAccount(ctx, "foreign", "foreign-refresh", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateCredentials(ctx, foreign, map[string]interface{}{"upstream_type": auth.UpstreamAntigravity, "access_token": "synthetic", auth.ExcelBPSCredentialKey: true}); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: oauth, AccessToken: "synthetic"}
	store.AddAccount(account)
	h := &Handler{db: db, store: store}
	put := func(id int64, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(id)}}
		c.Request = httptest.NewRequest(http.MethodPut, "/scheduler", strings.NewReader(body))
		h.UpdateAccountScheduler(c)
		return w
	}
	if w := put(foreign, `{"openai_excel_bps":true}`); w.Code != http.StatusBadRequest {
		t.Fatalf("foreign enable=%d %s", w.Code, w.Body.String())
	}
	if w := put(foreign, `{"openai_excel_bps":false}`); w.Code != http.StatusOK {
		t.Fatalf("foreign disable=%d %s", w.Code, w.Body.String())
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/batch", strings.NewReader(fmt.Sprintf(`{"ids":[%d,%d,%d],"openai_excel_bps":true}`, oauth, foreign, foreign+1000)))
	h.BatchUpdateAccounts(c)
	var result struct {
		Success int `json:"success"`
		Failed  int `json:"failed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != http.StatusOK || result.Success != 1 || result.Failed != 2 {
		t.Fatalf("partial response=%s err=%v", w.Body.String(), err)
	}
	if !account.IsExcelBPSEnabled() {
		t.Fatal("successful opt-in not published")
	}
	row, err := db.GetAccountByID(ctx, foreign)
	if err != nil || row.GetCredentialBool(auth.ExcelBPSCredentialKey) {
		t.Fatal("ineligible account changed")
	}
}

func TestBPSSourceFiltersAndProbeEventJSON(t *testing.T) {
	for _, value := range []string{"", "bps", "codex", "other", "unknown", " BPS "} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/usage", nil)
		q := c.Request.URL.Query()
		q.Set("upstream_source", value)
		c.Request.URL.RawQuery = q.Encode()
		filter, ok := parseUsageLogsFilter(c, time.Time{}, time.Time{})
		if !ok || filter.UpstreamSource != strings.ToLower(strings.TrimSpace(value)) {
			t.Fatalf("filter=%+v ok=%t", filter, ok)
		}
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage?upstream_source=invalid", nil)
	if _, ok := parseUsageLogsFilter(c, time.Time{}, time.Time{}); ok || w.Code != http.StatusBadRequest {
		t.Fatal("invalid source accepted")
	}
	for _, event := range []any{testEvent{Type: "test_complete", UpstreamSource: "bps"}, batchOperationEvent{Type: "progress", UpstreamSource: "codex"}, database.QualityTestJob{UpstreamSource: "bps"}} {
		raw, err := json.Marshal(event)
		if err != nil || !strings.Contains(string(raw), `"upstream_source"`) {
			t.Fatal("source JSON contract absent")
		}
	}
}
