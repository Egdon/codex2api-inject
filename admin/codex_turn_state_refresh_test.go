package admin

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func refreshTestToken(blocks int) string {
	raw := make([]byte, 57+16*blocks)
	raw[0] = 128
	binary.BigEndian.PutUint64(raw[1:9], uint64(time.Now().Unix()))
	return base64.URLEncoding.EncodeToString(raw)
}

func TestTurnStateRefreshUsesScopeAndSavesOnlyValidatedResponses(t *testing.T) {
	for _, failure := range []string{"success", "no-reissue", "no-reissue-open-stream", "degraded", "incomplete", "failed-verification", "degraded-verification", "invalid-verification", "metadata-verification", "degraded-metadata-verification", "echo-verification"} {
		t.Run(failure, func(t *testing.T) {
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "refresh.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			proxy.SetCodexTurnStateTemplateDatabase(db)
			defer proxy.SetCodexTurnStateTemplateDatabase(nil)
			prior := proxy.CurrentRuntimeSettings()
			defer proxy.ApplyRuntimeSettings(prior)
			cfg := prior
			cfg.CodexTurnStateTemplateCache = true
			cfg.CodexTurnStateAccountMode = "personal"
			cfg.CodexForceWebsocket = false
			proxy.ApplyRuntimeSettings(cfg)
			store := auth.NewStore(db, nil, nil)
			account := &auth.Account{DBID: 41, AccessToken: "test", PlanType: "pro", CodexTurnStateModels: "gpt-5.6-luna", Models: []string{"gpt-5.6-luna", "gpt-5.6-terra"}}
			store.AddAccount(account)
			h := &Handler{store: store, db: db}
			models := h.codexTurnStateRefreshModels(context.Background(), account)
			if len(models) != 1 || models[0] != "gpt-5.6-luna" {
				t.Fatalf("scope: %v", models)
			}
			healthy := refreshTestToken(10)
			degraded := refreshTestToken(11)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests == 1 && r.Header.Get("X-Codex-Turn-State") != "" {
					t.Error("acquisition used previous state")
				}
				if requests == 2 && r.Header.Get("X-Codex-Turn-State") != healthy {
					t.Error("validation did not send candidate")
				}
				// Candidate acquisition must not write the database, even before verification.
				rows, _ := db.ListCodexTurnStateTemplates(context.Background(), account.ID())
				if len(rows) != 0 {
					t.Error("unverified template already saved")
				}
				state := healthy
				if failure == "degraded" || failure == "degraded-verification" && requests == 2 {
					state = degraded
				}
				if failure == "invalid-verification" && requests == 2 {
					state = "invalid"
				}
				if requests == 1 || !strings.HasPrefix(failure, "no-reissue") && !strings.Contains(failure, "metadata") && failure != "echo-verification" {
					w.Header().Set("X-Codex-Turn-State", state)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if failure == "incomplete" {
					fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
					return
				}
				if failure == "failed-verification" && requests == 2 {
					fmt.Fprint(w, "data: {\"type\":\"response.failed\"}\n\n")
					return
				}
				if requests == 2 && (strings.Contains(failure, "metadata") || failure == "echo-verification") {
					eventType := "response.metadata"
					if failure == "degraded-metadata-verification" {
						state = degraded
					}
					if failure == "echo-verification" {
						eventType, state = "response.created", degraded
					}
					fmt.Fprintf(w, "data: {\"type\":%q,\"headers\":{\"X-Codex-Turn-State\":%q}}\n\n", eventType, state)
				}
				fmt.Fprint(w, antigravityTestSSEBody("ok"))
				if failure == "no-reissue-open-stream" {
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}
			}))
			defer server.Close()
			proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "test"})
			defer proxy.SetResinConfig(nil)
			router := gin.New()
			router.POST("/accounts/:id/turn-state/refresh", h.RefreshCodexTurnStateTemplates)
			response := httptest.NewRecorder()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			router.ServeHTTP(response, httptest.NewRequest("POST", "/accounts/41/turn-state/refresh", nil).WithContext(ctx))
			rows, err := db.ListCodexTurnStateTemplates(context.Background(), account.ID())
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if failure == "success" || strings.HasPrefix(failure, "no-reissue") || failure == "metadata-verification" || failure == "echo-verification" {
				want = 1
			}
			if len(rows) != want || !strings.Contains(response.Body.String(), fmt.Sprintf(`"saved":%d`, want)) {
				t.Fatalf("saved=%d response=%s", len(rows), response.Body.String())
			}
			if want == 1 && rows[0].Value != healthy {
				t.Fatal("saved value differs from upstream candidate")
			}
			if strings.Contains(response.Body.String(), healthy) {
				t.Fatal("template value leaked to admin")
			}
		})
	}
}

func TestTurnStateConnectionAndQualityScope(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{DBID: 45, PlanType: "pro", CodexTurnStateModels: "gpt-5.6-luna", Models: []string{"gpt-5.6-luna", "gpt-5.6-terra"}}
	h := &Handler{store: store}
	options := h.qualityTestOptionsForAccount(context.Background(), account)
	if len(options.Models) != 1 || options.Models[0] != "gpt-5.6-luna" {
		t.Fatalf("quality scope: %v", options.Models)
	}
	if _, err := h.connectionTestModelForAccount(context.Background(), account, "gpt-5.6-terra"); err == nil {
		t.Fatal("connection test allowed excluded model")
	}
	model, err := h.connectionTestModelForAccount(context.Background(), account, "")
	if err != nil || model != "gpt-5.6-luna" {
		t.Fatalf("default model=%s err=%v", model, err)
	}
	account.CodexTurnStateModels = ""
	account.Models = []string{"gpt-6-astra", "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.5"}
	models := h.codexTurnStateRefreshModels(context.Background(), account)
	for _, model := range models {
		if model != "gpt-6-astra" && !strings.HasPrefix(model, "gpt-5.6-") {
			t.Fatalf("unexpected default model %s", model)
		}
	}
	for _, want := range account.Models[:4] {
		found := false
		for _, model := range models {
			if model == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing model %s", want)
		}
	}
}

func TestTurnStateAutoReviewExcludedFromRefreshAndQuality(t *testing.T) {
	h := &Handler{store: auth.NewStore(nil, nil, nil)}
	for _, scope := range []string{"", "*", "codex-auto-review"} {
		account := &auth.Account{DBID: 46, PlanType: "pro", Models: []string{"codex-auto-review", "gpt-5.6-luna"}, CodexTurnStateModels: scope}
		for _, model := range h.codexTurnStateRefreshModels(context.Background(), account) {
			if model == "codex-auto-review" {
				t.Errorf("refresh includes auto-review with scope %q", scope)
			}
		}
		for _, model := range h.qualityTestOptionsForAccount(context.Background(), account).Models {
			if model == "codex-auto-review" {
				t.Errorf("quality test includes auto-review with scope %q", scope)
			}
		}
		if h.validateQualityTestForAccount(context.Background(), account, qualityTestRequest{Model: "codex-auto-review"}) == nil {
			t.Error("direct quality request for excluded model was accepted")
		}
	}
	// Relay accounts must not reintroduce the model into the quality workbench.
	account := &auth.Account{DBID: 47, UpstreamType: auth.UpstreamOpenAIResponses, Models: []string{"codex-auto-review", "gpt-5.6-luna"}}
	options := h.qualityTestOptionsForAccount(context.Background(), account)
	if len(options.Models) != 1 || options.Models[0] != "gpt-5.6-luna" {
		t.Fatalf("relay quality options: %v", options.Models)
	}
}
