package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func antigravityQuotaJSON(t *testing.T, remaining float64) string {
	t.Helper()
	snapshot := map[string]interface{}{
		"models": []map[string]interface{}{{"model_id": "gemini-3-flash", "remaining_fraction": 1}},
		"quota_groups": []map[string]interface{}{{
			"display_name": "Gemini Models",
			"buckets": []map[string]interface{}{
				{"bucket_id": "gemini-5h", "window": "5h", "remaining_fraction": 1},
				{"bucket_id": "gemini-weekly", "window": "weekly", "remaining_fraction": remaining},
			},
		}},
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestListAccountsPageAntigravityAuthKindFilterSummaryAndUsageSort(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _, _ := newPagedAccountsHandler(t)
	ctx := context.Background()
	insert := func(name string, credentials map[string]interface{}) int64 {
		t.Helper()
		credentials["upstream_type"] = auth.UpstreamAntigravity
		id, err := handler.db.InsertAccountWithUpstream(ctx, name, "google", auth.UpstreamAntigravity, credentials, "")
		if err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
		return id
	}
	tight := insert("ag-tight", map[string]interface{}{
		"refresh_token": "rt-tight", "email": "tight@example.org", "antigravity_quota": antigravityQuotaJSON(t, 0.2),
	})
	loose := insert("ag-loose", map[string]interface{}{
		"refresh_token": "rt-loose", "email": "loose@example.org", "antigravity_quota": antigravityQuotaJSON(t, 0.9),
	})
	apiKey := insert("ag-key", map[string]interface{}{"api_key": "AIza-test-key"})

	list := func(query string) accountsPageResponse {
		t.Helper()
		recorder := invokeListAccounts(t, handler, "/api/admin/accounts?view=page&channel=antigravity&page=1&page_size=10&"+query)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", query, recorder.Code, recorder.Body.String())
		}
		var page accountsPageResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode page: %v", err)
		}
		return page
	}
	ids := func(page accountsPageResponse) []int64 {
		got := make([]int64, 0, len(page.Accounts))
		for _, account := range page.Accounts {
			got = append(got, account.ID)
		}
		return got
	}

	all := list("sort=usage&order=desc")
	if all.Summary.OAuth != 2 || all.Summary.APIKey != 1 {
		t.Fatalf("summary oauth/api_key = %d/%d, want 2/1", all.Summary.OAuth, all.Summary.APIKey)
	}
	if got := ids(all); len(got) != 3 || got[0] != tight || got[1] != loose || got[2] != apiKey {
		t.Fatalf("usage desc order = %v, want [%d %d %d]", got, tight, loose, apiKey)
	}
	if got := ids(list("auth_kind=api_key")); len(got) != 1 || got[0] != apiKey {
		t.Fatalf("api_key filter = %v, want [%d]", got, apiKey)
	}
	if got := ids(list("auth_kind=oauth&sort=id&order=asc")); len(got) != 2 || got[0] != tight || got[1] != loose {
		t.Fatalf("oauth filter = %v, want [%d %d]", got, tight, loose)
	}
}

func TestAntigravityQuotaUsedPercentUsesTightestBucket(t *testing.T) {
	used, ok := auth.AntigravityQuotaUsedPercent(antigravityQuotaJSON(t, 0.25))
	if !ok || used < 74.99 || used > 75.01 {
		t.Fatalf("used = %v ok=%t, want 75", used, ok)
	}
	if _, ok := auth.AntigravityQuotaUsedPercent(""); ok {
		t.Fatal("empty snapshot must be unknown")
	}
	if _, ok := auth.AntigravityQuotaUsedPercent(`{"forbidden":true,"models":[]}`); ok {
		t.Fatal("forbidden snapshot must be unknown")
	}
}

func readBatchOperationEvents(t *testing.T, body string) []batchOperationEvent {
	t.Helper()
	events := make([]batchOperationEvent, 0)
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event batchOperationEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("decode event %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

// 通用批量刷新(含流式进度)对 Antigravity 账号走控制面同步，禁用账号也能刷新；
// 不存在的账号计入失败而不是中断整批。
func TestBatchRefreshAccountsStreamsAntigravitySync(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"google-subject-batch","email":"batch@example.com","verified_email":true,"name":"Batch"}`)
		case "/load":
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"project-batch","paidTier":{"id":"pro","name":"Pro"}}`)
		case "/quota":
			_, _ = io.WriteString(w, `{"models":{"gemini-3-flash":{"quotaInfo":{"remainingFraction":0.5}}}}`)
		case "/summary":
			_, _ = io.WriteString(w, `{"groups":[{"displayName":"Gemini Models","buckets":[]}]}`)
		case "/credits":
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	previousEndpoints := auth.DefaultAntigravityEndpoints
	auth.DefaultAntigravityEndpoints = auth.AntigravityEndpoints{
		UserInfoURL: server.URL + "/userinfo", LoadProject: []string{server.URL + "/load"},
		Quota: []string{server.URL + "/quota"}, QuotaSummary: []string{server.URL + "/summary"}, AICredits: []string{server.URL + "/credits"},
	}
	defer func() { auth.DefaultAntigravityEndpoints = previousEndpoints }()

	handler, db, _, _ := newImportGroupsTestHandler(t)
	// 运行时池为空:禁用账号本就不在池里,识别必须回落到数据库行。
	handler.store = nil
	ctx := context.Background()
	accountID, err := db.InsertAccountWithUpstream(ctx, "before", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "access",
		"refresh_token": "refresh", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"credential_family_id": "ag_family_batch",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountEnabled(ctx, accountID, false); err != nil {
		t.Fatal(err)
	}
	if !handler.isAntigravityAccountID(ctx, accountID) {
		t.Fatal("disabled Antigravity account must be recognised from the database row")
	}

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	body, _ := json.Marshal(map[string]interface{}{"ids": []int64{accountID, 987654321}})
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/batch-refresh?stream=true", bytes.NewReader(body))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	handler.BatchRefreshAccounts(ginContext)

	events := readBatchOperationEvents(t, recorder.Body.String())
	if len(events) == 0 || events[len(events)-1].Type != "complete" {
		t.Fatalf("stream events = %#v", events)
	}
	complete := events[len(events)-1]
	if complete.Action != "batch_refresh" || complete.Success != 1 || complete.Failed != 1 || complete.Total != 2 {
		t.Fatalf("complete event = %#v", complete)
	}
	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredential("project_id") != "project-batch" || !strings.Contains(row.GetCredential("antigravity_quota"), "gemini-3-flash") {
		t.Fatalf("refreshed credentials = %#v", row.Credentials)
	}
	if row.Enabled {
		t.Fatal("sync must not re-enable a disabled account")
	}
}

func TestAntigravityCleanPathsPruneInsteadOfInvalidate(t *testing.T) {
	for _, path := range []string{
		"/api/admin/accounts/antigravity/clean-banned",
		"/api/admin/accounts/antigravity/clean-error",
	} {
		if shouldInvalidateAccountSnapshotCaches(http.MethodPost, path, http.StatusOK) {
			t.Fatalf("%s must use incremental pruning", path)
		}
	}
}
