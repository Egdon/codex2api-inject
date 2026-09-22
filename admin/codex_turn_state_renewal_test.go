package admin

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

func TestTurnStateRenewalRetriesRetainOldTemplateAndUseIssuanceProxy(t *testing.T) {
	for _, mode := range []string{"failure", "same-issuance", "new-issuance", "rotated-success", "verification-failure"} {
		t.Run(mode, func(t *testing.T) {
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
			account := &auth.Account{DBID: 9950, AccessToken: "fixture", PlanType: "pro", CodexTurnStateModels: "gpt-5.6-luna", ProxyURL: "http://ordinary.test:8080", CodexTurnStateProxyURL: "socks5://issuance.test:1080"}
			store.AddAccount(account)
			h := &Handler{store: store, db: db}
			proxies := []string{account.CodexTurnStateProxy()}
			// The normal pool switch stays off; explicit renewal can still use enabled nodes.
			if store.GetProxyPoolEnabled() {
				t.Fatal("fixture pool should default off")
			}
			disabledID, err := db.InsertProxy(context.Background(), "http://disabled.test:8080", "disabled")
			if err != nil {
				t.Fatal(err)
			}
			disabled := false
			if err := db.UpdateProxy(context.Background(), disabledID, nil, nil, &disabled); err != nil {
				t.Fatal(err)
			}
			failedID, err := db.InsertProxy(context.Background(), "http://failed.test:8080", "failed")
			if err != nil {
				t.Fatal(err)
			}
			if err := db.UpdateProxyTestResult(context.Background(), failedID, "http://failed.test:8080", "error", "", "", 0); err != nil {
				t.Fatal(err)
			}
			if _, err := db.InsertProxy(context.Background(), proxies[0], "configured"); err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 9; i++ {
				url := fmt.Sprintf("socks5://pool-%d.test:1080", i)
				if _, err := db.InsertProxy(context.Background(), url, "pool"); err != nil {
					t.Fatal(err)
				}
				proxies = append(proxies, url)
			}
			activeAttempt := 1

			now := time.Now().Truncate(time.Second)
			issued := now.Add(-50 * time.Minute)
			raw, _ := base64.URLEncoding.DecodeString(refreshTestToken(10))
			binary.BigEndian.PutUint64(raw[1:9], uint64(issued.Unix()))
			oldToken := base64.URLEncoding.EncodeToString(raw)
			row := database.CodexTurnStateTemplate{AccountID: account.ID(), Model: "gpt-5.6-luna", Value: oldToken, IssuedAt: issued.Unix()}
			if err := db.SaveCodexTurnStateTemplate(context.Background(), row); err != nil {
				t.Fatal(err)
			}
			old := proxy.WebsocketExecuteFunc
			defer func() { proxy.WebsocketExecuteFunc = old }()
			calls := 0
			token := refreshTestToken(11)
			if mode == "same-issuance" {
				token = oldToken
			}
			if mode == "new-issuance" {
				token = refreshTestToken(10)
			}
			proxy.WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, body []byte, sessionID, proxyURL, apiKey string, device *proxy.DeviceProfileConfig, headers http.Header, poolKey string) (*http.Response, error) {
				calls++
				if proxyURL != proxies[activeAttempt-1] {
					t.Errorf("attempt %d used proxy %s, want %s", activeAttempt, proxyURL, proxies[activeAttempt-1])
				}
				responseToken := token
				if mode == "verification-failure" && calls%2 == 1 {
					responseToken = refreshTestToken(10)
				}
				proxy.CaptureCodexTurnStateTemplate(ctx, a, row.Model, http.Header{"X-Codex-Turn-State": []string{responseToken}})
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(antigravityTestSSEBody("ok")))}, nil
			}
			clock := func() time.Time { return now }
			account.DispatchPaused = 1
			h.renewCodexTurnStateModel(context.Background(), row, clock)
			account.DispatchPaused = 0
			if calls != 0 {
				t.Fatal("paused account issued a request")
			}

			codexTurnStateRefreshRunning.Store(account.ID(), true)
			h.renewCodexTurnStateModel(context.Background(), row, clock)
			codexTurnStateRefreshRunning.Delete(account.ID())
			if calls != 0 {
				t.Fatal("manual refresh lock ignored")
			}
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			h.renewCodexTurnStateModel(canceled, row, clock)
			if calls != 0 {
				t.Fatal("canceled context issued a request")
			}
			for attempt := 1; attempt <= 11; attempt++ {
				activeAttempt = min(attempt, 10)
				if attempt == 2 {
					h = &Handler{store: store, db: db}
				}
				if mode == "rotated-success" && attempt == 3 {
					token = refreshTestToken(10)
				}
				h.renewCodexTurnStateModel(context.Background(), row, clock)
				if mode == "new-issuance" || mode == "rotated-success" && attempt == 3 {
					break
				}
				want := min(attempt, 10)
				if mode == "same-issuance" || mode == "verification-failure" {
					want *= 2
				}
				if calls != want {
					t.Fatalf("attempt %d: calls=%d want %d", attempt, calls, want)
				}
				now = now.Add(9 * time.Second)
				h.renewCodexTurnStateModel(context.Background(), row, clock)
				if calls != want {
					t.Fatal("retried before ten-second delay")
				}
				now = now.Add(time.Second)
			}
			saved, ok, err := db.GetCodexTurnStateTemplate(context.Background(), account.ID(), row.Model)
			if err != nil || !ok {
				t.Fatalf("template lost: %v", err)
			}
			if mode == "new-issuance" || mode == "rotated-success" {
				wantCalls := 2
				if mode == "rotated-success" {
					wantCalls = 4
				}
				if calls != wantCalls || saved.IssuedAt <= row.IssuedAt {
					t.Fatal("successful renewal did not extend expiry")
				}
				before := calls
				h.renewCodexTurnStateModel(context.Background(), saved, clock)
				if calls != before {
					t.Fatal("fresh template renewed immediately")
				}
			} else if saved.Value != oldToken || saved.IssuedAt != row.IssuedAt {
				t.Fatal("failed renewal changed old template/expiry")
			}
			history, err := db.ListCodexTurnStateHistory(context.Background(), 1, 20, database.CodexTurnStateHistoryFilter{AccountID: account.ID()})
			wantRecords := 10
			if mode == "new-issuance" {
				wantRecords = 1
			}
			if mode == "rotated-success" {
				wantRecords = 3
			}
			if err != nil || history.Total != wantRecords {
				t.Fatalf("history count=%d want=%d error=%v", history.Total, wantRecords, err)
			}
			for _, record := range history.Records {
				if record.ProxyURL != proxies[record.Attempt-1] {
					t.Fatal("history did not snapshot the actual attempt proxy")
				}
				wantStatus := "failed"
				if mode == "new-issuance" || mode == "rotated-success" && record.Attempt == 3 {
					wantStatus = "success"
				}
				if record.Status != wantStatus || record.MaxAttempts != 10 || record.ExpiresBefore == 0 {
					t.Fatalf("history result=%#v want status %s", record, wantStatus)
				}
				if (record.ExpiresAfter > 0) != (wantStatus == "success") {
					t.Fatal("failed attempt advertised renewed expiry")
				}
			}
			if account.GetProxyURL() != "http://ordinary.test:8080" || account.CodexTurnStateProxy() != "socks5://issuance.test:1080" {
				t.Fatal("retry rebound account proxies")
			}
			if store.GetProxyPoolEnabled() {
				t.Fatal("retry enabled the ordinary proxy pool")
			}
			if mode == "failure" || mode == "same-issuance" || mode == "verification-failure" {
				used, err := db.CodexTurnStateRenewalProxyHashes(context.Background(), row)
				if err != nil || len(used) != 10 {
					t.Fatalf("used proxy count=%d, err=%v", len(used), err)
				}
			}

		})
	}
}

func TestTurnStateRenewalWorkerBoundsConcurrencyAndStops(t *testing.T) {
	db := newTestAdminDB(t)
	prior := proxy.CurrentRuntimeSettings()
	cfg := prior
	cfg.CodexTurnStateTemplateCache = true
	cfg.CodexForceWebsocket = true
	cfg.CodexTurnStateAccountMode = "personal"
	proxy.ApplyRuntimeSettings(cfg)
	defer proxy.ApplyRuntimeSettings(prior)
	store := auth.NewStore(db, nil, nil)
	issued := time.Now().Add(-50 * time.Minute).Truncate(time.Second)
	raw, _ := base64.URLEncoding.DecodeString(refreshTestToken(10))
	binary.BigEndian.PutUint64(raw[1:9], uint64(issued.Unix()))
	for id := int64(9960); id < 9968; id++ {
		store.AddAccount(&auth.Account{DBID: id, AccessToken: "fixture", PlanType: "pro"})
		if err := db.SaveCodexTurnStateTemplate(context.Background(), database.CodexTurnStateTemplate{AccountID: id, Model: "gpt-5.6-luna", Value: base64.URLEncoding.EncodeToString(raw), IssuedAt: issued.Unix()}); err != nil {
			t.Fatal(err)
		}
	}
	old := proxy.WebsocketExecuteFunc
	defer func() { proxy.WebsocketExecuteFunc = old }()
	started := make(chan struct{}, 8)
	proxy.WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, body []byte, sessionID, proxyURL, apiKey string, device *proxy.DeviceProfileConfig, headers http.Header, poolKey string) (*http.Response, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &Handler{db: db, store: store}
	defer func() { cancel(); h.WaitCodexTurnStateRenewal() }()
	h.StartCodexTurnStateRenewal(ctx)
	h.StartCodexTurnStateRenewal(ctx)
	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("worker did not start four probes")
		}
	}
	cancel()
	done := make(chan struct{})
	go func() { h.WaitCodexTurnStateRenewal(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
	if len(started) != 0 {
		t.Fatal("worker exceeded four concurrent probes")
	}
	history, err := db.ListCodexTurnStateHistory(context.Background(), 1, 20, database.CodexTurnStateHistoryFilter{})
	if err != nil || history.Total != 4 {
		t.Fatalf("canceled worker history=%d err=%v", history.Total, err)
	}
	for _, record := range history.Records {
		if record.Status != "interrupted" {
			t.Fatalf("canceled attempt status=%s", record.Status)
		}
	}

}

func TestTurnStateRenewalDoesNotReuseExhaustedOrDeletedProxies(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	account := &auth.Account{DBID: 9970, AccessToken: "fixture", PlanType: "pro", CodexTurnStateProxyURL: "socks5://initial.test:1080", ProxyURL: "http://ordinary.test:8080"}
	store.AddAccount(account)
	h := &Handler{store: store, db: db}
	row := database.CodexTurnStateTemplate{AccountID: account.ID(), Model: "gpt-5.6-luna", IssuedAt: 100}
	ctx := context.Background()
	if _, err := db.InsertProxy(ctx, account.CodexTurnStateProxy(), "initial"); err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertProxy(ctx, "socks5://alternate.test:1080", "alternate")
	if err != nil {
		t.Fatal(err)
	}
	route, err := h.codexTurnStateRenewalProxy(ctx, account, row, 2)
	if err != nil || route.id != id {
		t.Fatalf("retry route=%#v error=%v", route, err)
	}
	if err := db.RecordCodexTurnStateRenewalProxy(ctx, row, 2, route.id, route.hash); err != nil {
		t.Fatal(err)
	}
	if _, err := h.codexTurnStateRenewalProxy(ctx, account, row, 3); err == nil {
		t.Fatal("exhausted URL reused")
	}
	// Changing a node's URL makes it a different exit even when the ID is stable.
	changed := "socks5://new-alternate.test:1080"
	if err := db.UpdateProxy(ctx, id, &changed, nil, nil); err != nil {
		t.Fatal(err)
	}
	route, err = h.codexTurnStateRenewalProxy(ctx, account, row, 3)
	if err != nil || route.url != changed {
		t.Fatal("new URL was not picked up")
	}
	if err := db.DeleteProxy(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.codexTurnStateRenewalProxy(ctx, account, row, 3); err == nil {
		t.Fatal("deleted proxy reused or fell back to initial")
	}
}
