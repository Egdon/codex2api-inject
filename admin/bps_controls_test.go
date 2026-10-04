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
	"github.com/gin-gonic/gin"
)

// Runtime controls are retired; stored flags remain inert historical data.
// Source filters and response fields still interpret pre-retirement records.
func TestRetiredBPSSettingsAbsentFromAPI(t *testing.T) {
	h, _, _ := newImagesSettingsHandler(t)
	assertNoControls := func(response *httptest.ResponseRecorder) {
		t.Helper()
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil || response.Code != http.StatusOK {
			t.Fatalf("settings status=%d err=%v", response.Code, err)
		}
		for _, field := range []string{"openai_excel_bps_enabled", "codex_basispoints_enabled", "codex_basispoints_models", "codex_basispoints_model_whitelist"} {
			if _, exists := fields[field]; exists {
				t.Fatalf("retired setting %q remains exposed", field)
			}
		}
	}
	assertNoControls(invokeResponseCacheSettingsAdmin(t, h, http.MethodGet, nil))
	update := invokeResponseCacheSettingsAdmin(t, h, http.MethodPut, map[string]any{
		"site_name": "BPS retirement regression", "codex_basispoints_enabled": true,
		"openai_excel_bps_enabled": true,
	})
	if update.Code != http.StatusOK {
		t.Fatalf("ordinary settings update status=%d", update.Code)
	}
	assertNoControls(invokeResponseCacheSettingsAdmin(t, h, http.MethodGet, nil))
}

func TestRetiredBPSAccountFlagsCannotBePatched(t *testing.T) {
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "ordinary", "synthetic-refresh", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateCredentials(ctx, id, map[string]interface{}{
		"access_token": "synthetic", "openai_excel_bps": true,
		"openai_excel_bps_opt_out": false,
	}); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: id, AccessToken: "synthetic"})
	h := &Handler{db: db, store: store}
	put := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(id)}}
		c.Request = httptest.NewRequest(http.MethodPut, "/scheduler", strings.NewReader(body))
		h.UpdateAccountScheduler(c)
		return w
	}
	if w := put(`{"openai_excel_bps":false,"openai_excel_bps_opt_out":true}`); w.Code != http.StatusBadRequest {
		t.Fatalf("retired-only patch status=%d", w.Code)
	}
	if w := put(`{"score_bias_override":7,"openai_excel_bps":false,"openai_excel_bps_opt_out":true}`); w.Code != http.StatusBadRequest {
		t.Fatalf("mixed retired scheduler patch status=%d", w.Code)
	}
	if w := put(`{"score_bias_override":7}`); w.Code != http.StatusOK {
		t.Fatalf("ordinary scheduler update status=%d", w.Code)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/batch", strings.NewReader(fmt.Sprintf(`{"ids":[%d],"openai_excel_bps":false}`, id)))
	h.BatchUpdateAccounts(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("retired-only batch status=%d", w.Code)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !row.GetCredentialBool("openai_excel_bps") || row.GetCredentialBool("openai_excel_bps_opt_out") {
		t.Fatal("unknown retired fields altered persisted historical flags")
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
