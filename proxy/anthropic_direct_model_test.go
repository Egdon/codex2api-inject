package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// The built-in Claude→Codex table also lists IDs that Antigravity publishes
// under the same name (claude-sonnet-4-6). Checking that table first rewrote
// them to gpt-5.5: an Antigravity-only key got 503, and a key that can reach
// Codex as well silently got a GPT answer.

const directModelTestRequest = `{"model":"claude-sonnet-4-6","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`

func directModelTestStore(t *testing.T, accounts ...*auth.Account) *auth.Store {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	for _, account := range accounts {
		store.AddAccount(account)
	}
	return store
}

func directModelTestCodexAccount() *auth.Account {
	return &auth.Account{DBID: 7801, AccessToken: "codex-token", Status: auth.StatusReady, HealthTier: auth.HealthTierHealthy}
}

func directModelTestAntigravityAccount(models ...string) *auth.Account {
	return &auth.Account{
		DBID:                 7802,
		UpstreamType:         auth.UpstreamAntigravity,
		AccessToken:          "google-access-token",
		RefreshToken:         "google-refresh-token",
		AntigravityProjectID: "google-project",
		Models:               models,
		HealthTier:           auth.HealthTierHealthy,
		Status:               auth.StatusReady,
	}
}

func directModelTestContext(row *database.APIKeyRow) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(directModelTestRequest))
	if row != nil {
		c.Set(contextAPIKeyRow, row)
		c.Set(contextAPIKeyID, row.ID)
	}
	return c
}

func directModelRoutingModel(h *Handler, c *gin.Context) string {
	supported := h.supportedModelIDs(c.Request.Context())
	body := h.resolveMessagesRoutingBodyForRequest(c, []byte(directModelTestRequest), "claude-sonnet-4-6", supported)
	return gjson.GetBytes(body, "model").String()
}

func directModelTranslatedModel(t *testing.T, h *Handler, c *gin.Context) string {
	t.Helper()
	var state anthropicCodexTranslation
	body, err := h.translateAnthropicMessagesToCodexOnce(c, &state, []byte(directModelTestRequest), h.supportedModelIDs(c.Request.Context()))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	return gjson.GetBytes(body, "model").String()
}

func TestMessagesKeepsClaudeModelPublishedByAntigravity(t *testing.T) {
	cases := []struct {
		name string
		row  *database.APIKeyRow
	}{
		{"antigravity channel key", &database.APIKeyRow{ID: 7810, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelAntigravity}}},
		{"unrestricted key reaching codex too", &database.APIKeyRow{ID: 7811}},
		{"no key row", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := directModelTestStore(t, directModelTestCodexAccount(), directModelTestAntigravityAccount("claude-sonnet-4-6", "claude-opus-4-6-thinking"))
			h := &Handler{store: store}
			c := directModelTestContext(tc.row)
			if got := directModelRoutingModel(h, c); got != "claude-sonnet-4-6" {
				t.Fatalf("routing model = %q, want the Antigravity-published claude-sonnet-4-6", got)
			}
			if got := directModelTranslatedModel(t, h, c); got != "claude-sonnet-4-6" {
				t.Fatalf("translated body model = %q, want it to match the routing model", got)
			}
		})
	}
}

// A model name must not switch providers because the pool is momentarily busy:
// cooling Antigravity accounts yield a retryable error, not a GPT answer.
func TestMessagesDirectModelIgnoresTransientCooldown(t *testing.T) {
	account := directModelTestAntigravityAccount("claude-sonnet-4-6")
	account.Status = auth.StatusCooldown
	account.CooldownUtil = time.Now().Add(time.Hour)
	store := directModelTestStore(t, directModelTestCodexAccount(), account)
	h := &Handler{store: store}
	if got := directModelRoutingModel(h, directModelTestContext(&database.APIKeyRow{ID: 7812})); got != "claude-sonnet-4-6" {
		t.Fatalf("routing model = %q, want claude-sonnet-4-6 while the Antigravity account cools down", got)
	}
}

func TestMessagesKeepsCodexFallbackWithoutDirectAccount(t *testing.T) {
	otherKeyAccount := directModelTestAntigravityAccount("claude-sonnet-4-6")
	otherKeyAccount.AllowedAPIKeyIDs = []int64{9999}
	brokenAccount := directModelTestAntigravityAccount("claude-sonnet-4-6")
	brokenAccount.Status = auth.StatusError

	cases := []struct {
		name     string
		accounts []*auth.Account
		row      *database.APIKeyRow
	}{
		{"codex-only pool", []*auth.Account{directModelTestCodexAccount()}, &database.APIKeyRow{ID: 7820}},
		{"codex-only pool without key row", []*auth.Account{directModelTestCodexAccount()}, nil},
		{"codex channel key on a mixed pool", []*auth.Account{directModelTestCodexAccount(), directModelTestAntigravityAccount("claude-sonnet-4-6")},
			&database.APIKeyRow{ID: 7821, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelCodex}}},
		{"antigravity account without the model", []*auth.Account{directModelTestCodexAccount(), directModelTestAntigravityAccount("gemini-3.7-flash-tiered")}, &database.APIKeyRow{ID: 7822}},
		{"antigravity account bound to another key", []*auth.Account{directModelTestCodexAccount(), otherKeyAccount}, &database.APIKeyRow{ID: 7823}},
		{"antigravity account in error state", []*auth.Account{directModelTestCodexAccount(), brokenAccount}, &database.APIKeyRow{ID: 7824}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{store: directModelTestStore(t, tc.accounts...)}
			c := directModelTestContext(tc.row)
			if got := directModelRoutingModel(h, c); got != "gpt-5.5" {
				t.Fatalf("routing model = %q, want the gpt-5.5 Codex fallback", got)
			}
			if got := directModelTranslatedModel(t, h, c); got != "gpt-5.5" {
				t.Fatalf("translated body model = %q, want gpt-5.5", got)
			}
		})
	}
}

func TestMessagesConfiguredMappingBeatsDirectModel(t *testing.T) {
	for _, mapping := range []string{`{"claude-sonnet-4-6":"gpt-5.6-sol"}`, `{"claude-*":"gpt-5.6-sol"}`} {
		t.Run(mapping, func(t *testing.T) {
			store := directModelTestStore(t, directModelTestCodexAccount(), directModelTestAntigravityAccount("claude-sonnet-4-6"))
			store.SetModelMapping(mapping)
			h := &Handler{store: store}
			c := directModelTestContext(&database.APIKeyRow{ID: 7830})
			if got := directModelRoutingModel(h, c); got != "gpt-5.6-sol" {
				t.Fatalf("routing model = %q, want the configured gpt-5.6-sol", got)
			}
			if got := directModelTranslatedModel(t, h, c); got != "gpt-5.6-sol" {
				t.Fatalf("translated body model = %q, want gpt-5.6-sol", got)
			}
		})
	}
}

// An OpenAI Responses relay that declares the Claude ID serves it under that
// name too, so the Codex default must not override it either.
func TestMessagesKeepsClaudeModelDeclaredByRelay(t *testing.T) {
	relay := &auth.Account{
		DBID:         7840,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://relay.example",
		APIKey:       "relay-key",
		Models:       []string{"claude-sonnet-4-6"},
		Status:       auth.StatusReady,
	}
	h := &Handler{store: directModelTestStore(t, directModelTestCodexAccount(), relay)}
	if got := directModelRoutingModel(h, directModelTestContext(&database.APIKeyRow{ID: 7841})); got != "claude-sonnet-4-6" {
		t.Fatalf("routing model = %q, want claude-sonnet-4-6 declared by the relay", got)
	}
}

func TestMessagesDirectModelDecisionIsMemoizedPerRequest(t *testing.T) {
	store := directModelTestStore(t, directModelTestCodexAccount())
	h := &Handler{store: store}
	c := directModelTestContext(&database.APIKeyRow{ID: 7850})
	if h.messagesModelServedDirectlyForRequest(c, "claude-sonnet-4-6") {
		t.Fatal("codex-only pool must not serve claude-sonnet-4-6 directly")
	}
	store.AddAccount(directModelTestAntigravityAccount("claude-sonnet-4-6"))
	if h.messagesModelServedDirectlyForRequest(c, "claude-sonnet-4-6") {
		t.Fatal("the decision must stay stable within one request so routing and translation agree")
	}
	if !h.messagesModelServedDirectlyForRequest(directModelTestContext(&database.APIKeyRow{ID: 7850}), "claude-sonnet-4-6") {
		t.Fatal("a new request should see the Antigravity account")
	}
}

// End to end: the repro from the issue. An Antigravity-only key asks
// /v1/messages for claude-sonnet-4-6 and must reach the Antigravity upstream
// with that model instead of failing with "No available accounts".
func TestMessagesRoutesDefaultMappedClaudeModelToAntigravity(t *testing.T) {
	handler, upstream := newAntigravityTransportTestHandler(t,
		`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`)
	handler.store.AddAccount(directModelTestAntigravityAccount("claude-sonnet-4-6"))

	recorder := invokeAntigravityTransport(t, handler, "/v1/messages", directModelTestRequest, func(h *Handler, c *gin.Context) {
		c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 7860, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelAntigravity}})
		c.Set(contextAPIKeyID, int64(7860))
		h.Messages(c)
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	upstream.mu.Lock()
	model, _ := upstream.envelope["model"].(string)
	upstream.mu.Unlock()
	if model != "claude-sonnet-4-6" {
		t.Fatalf("upstream model = %q, want claude-sonnet-4-6", model)
	}
	if !strings.Contains(recorder.Body.String(), `"ok"`) {
		t.Fatalf("response did not carry the upstream text: %s", recorder.Body.String())
	}
}
