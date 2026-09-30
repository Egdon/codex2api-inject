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
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// Persistence and failure publication now use the upstream
// codex_basispoints_settings_test.go coverage, not the retired master switch.
func TestBPSSettingsExposeOnlyUpstreamGlobal(t *testing.T) {
	h, _, _ := newImagesSettingsHandler(t)
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings()) })
	response := invokeResponseCacheSettingsAdmin(t, h, http.MethodGet, nil)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil || response.Code != http.StatusOK {
		t.Fatalf("GET settings=%d err=%v", response.Code, err)
	}
	if _, exists := fields["openai_excel_bps_enabled"]; exists {
		t.Fatal("retired master setting remains in API")
	}
	var global bool
	if err := json.Unmarshal(fields["codex_basispoints_enabled"], &global); err != nil || global {
		t.Fatalf("upstream global default=%t err=%v", global, err)
	}
}

func TestBPSAccountEnableEligibilityAndBulkPartialResults(t *testing.T) {
	oldGlobal := auth.ExcelBPSGlobalEnabled()
	auth.SetExcelBPSGlobalEnabled(false)
	t.Cleanup(func() { auth.SetExcelBPSGlobalEnabled(oldGlobal) })
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
	if w := put(foreign, `{"openai_excel_bps":false,"openai_excel_bps_opt_out":true}`); w.Code != http.StatusOK {
		t.Fatalf("foreign disable=%d %s", w.Code, w.Body.String())
	}
	auth.SetExcelBPSGlobalEnabled(true)
	if w := put(foreign, `{"openai_excel_bps":false,"openai_excel_bps_opt_out":false}`); w.Code != http.StatusBadRequest {
		t.Fatalf("foreign inherit with global on=%d %s", w.Code, w.Body.String())
	}
	if w := put(foreign, `{"openai_excel_bps":false,"openai_excel_bps_opt_out":true}`); w.Code != http.StatusOK {
		t.Fatalf("foreign explicit off with global on=%d %s", w.Code, w.Body.String())
	}
	if w := put(oauth, `{"openai_excel_bps":false,"openai_excel_bps_opt_out":false}`); w.Code != http.StatusOK || !account.IsExcelBPSEnabled() {
		t.Fatalf("ordinary inherit with global on=%d %s", w.Code, w.Body.String())
	}
	auth.SetExcelBPSGlobalEnabled(false)
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
