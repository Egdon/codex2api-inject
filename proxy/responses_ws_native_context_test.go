package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

type nativeWSFixture struct {
	url     string
	account *auth.Account
}

func newNativeWSFixture(t *testing.T, execute func(context.Context, []byte) (*http.Response, error)) nativeWSFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	resetResponseCacheForTest()
	oldExecutor := WebsocketExecuteFunc
	t.Cleanup(func() {
		WebsocketExecuteFunc = oldExecutor
		resetResponseCacheForTest()
	})
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, _, _, _ string, _ *DeviceProfileConfig, _ http.Header, _ string) (*http.Response, error) {
		if int64(1) != account.GetActiveRequests() {
			t.Error("upstream request did not occupy account capacity")
		}
		return execute(ctx, body)
	}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 1, AccessToken: "fixture", PlanType: "plus"}
	store.AddAccount(account)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return nativeWSFixture{url: "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses", account: account}
}

func (fixture nativeWSFixture) connect(t *testing.T) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(fixture.url, http.Header{"Session-Id": {"native-context"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func nativeWSTurn(t *testing.T, conn *websocket.Conn, input string) []byte {
	t.Helper()
	var request map[string]any
	if err := json.Unmarshal([]byte(input), &request); err != nil {
		t.Fatal(err)
	}
	request["type"], request["model"], request["store"] = "response.create", "gpt-5.5", false
	if err := conn.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	return readResponsesWSTerminalEvent(t, conn)
}

func nativeWSResponse(id string, outputs ...string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(wsContextTestSSE(id, outputs...)))}
}

func TestNativeWSStoreFalseRecoversToolChainAfterReconnect(t *testing.T) {
	for _, failure := range []string{"binding_lost", "invalid_id"} {
		t.Run(failure, func(t *testing.T) {
			var calls atomic.Int32
			fixture := newNativeWSFixture(t, func(_ context.Context, body []byte) (*http.Response, error) {
				switch calls.Add(1) {
				case 1:
					return nativeWSResponse("one", `{"type":"reasoning","encrypted_content":"fixture-opaque"}`, `{"type":"function_call","call_id":"a","name":"exec","arguments":"{}"}`), nil
				case 2:
					if int64(1) != gjson.GetBytes(body, "input.#").Int() || "one" != gjson.GetBytes(body, "previous_response_id").String() {
						t.Error("healthy continuation repeated history")
					}
					return nativeWSResponse("two", `{"type":"function_call","call_id":"b","name":"exec","arguments":"{}"}`), nil
				case 3:
					return nativeWSContinuationFailure(failure), nativeWSContinuationError(failure)
				default:
					if gjson.GetBytes(body, "previous_response_id").Exists() || int64(7) != gjson.GetBytes(body, "input.#").Int() || "fixture-opaque" != gjson.GetBytes(body, "input.2.encrypted_content").String() {
						t.Errorf("incomplete or duplicate recovery: %s", body)
					}
					return nativeWSResponse("recovered"), nil
				}
			})
			conn := fixture.connect(t)
			assertNativeWSSuccess(t, nativeWSTurn(t, conn, `{"input":[{"type":"additional_tools","tools":[]},{"role":"user","content":"root"}]}`))
			assertNativeWSSuccess(t, nativeWSTurn(t, conn, `{"previous_response_id":"one","input":[{"type":"function_call_output","call_id":"a","output":"1"}]}`))
			conn.Close()
			conn = fixture.connect(t)
			assertNativeWSSuccess(t, nativeWSTurn(t, conn, `{"previous_response_id":"two","input":[{"type":"function_call_output","call_id":"b","output":"2"}]}`))
			if int32(4) != calls.Load() {
				t.Fatal("recovery repeated")
			}
		})
	}
}

func TestNativeWSEncryptedRecoveryRequiresCompatibleAccount(t *testing.T) {
	resetResponseCacheForTest()
	t.Cleanup(resetResponseCacheForTest)
	owner := nativeWSCachePrefix + "fixture"
	account := &auth.Account{DBID: 1, CredentialGeneration: 1}
	cacheNativeWSContext(nativeWSCompletedContext{owner: owner, input: `[{"role":"user","content":"root"}]`,
		completed:  []byte(`{"response":{"id":"encrypted","output":[{"type":"reasoning","encrypted_content":"fixture"}]}}`),
		provenance: nativeWSAccountProvenance(account)})
	body := []byte(`{"previous_response_id":"encrypted","input":[{"role":"user","content":"next"}]}`)
	source := newResponsesWSReplaySource(body, owner)
	source.setAccount(account)
	input := source.Input()
	if "fixture" != gjson.Get(input, "1.encrypted_content").String() {
		t.Fatal("compatible recovery discarded encrypted reasoning")
	}
	recovered := newResponsesWSReplaySourceFromRecovery(input, source)
	for _, changed := range []*auth.Account{{DBID: 2, CredentialGeneration: 1}, {DBID: 1, CredentialGeneration: 2}} {
		for _, snapshot := range []*responsesWSReplaySource{source, recovered} {
			snapshot.setAccount(changed)
			if "" != snapshot.Input() || snapshot.err == nil {
				t.Fatal("encrypted context replayed on incompatible account")
			}
		}
	}
}

func TestNativeWSCacheSeparatesStreamAndExecutionLane(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	identity := requestSessionIdentity{affinityID: "thread", explicitUpstreamID: "thread"}
	root := nativeWSTurnCacheOwner(c, "key:1", nativeWSTurnScope{body: []byte(`{"stream_id":"main"}`), identity: identity})
	for _, body := range []string{`{"stream_id":"other"}`, `{"stream_id":"main","client_metadata":{"x-codex-turn-metadata":"{\"request_kind\":\"memory\"}"}}`} {
		if root == nativeWSTurnCacheOwner(c, "key:1", nativeWSTurnScope{body: []byte(body), identity: identity}) {
			t.Fatalf("execution lane shared native context: %s", body)
		}
	}
	for _, key := range []string{"fixture-key-one", "fixture-key-two"} {
		c.Request.Header.Set("Authorization", "Bearer "+key)
		other := nativeWSTurnCacheOwner(c, "key:1", nativeWSTurnScope{body: []byte(`{"stream_id":"main"}`), identity: identity})
		if root == other {
			t.Fatal("static API keys shared native context")
		}
		root = other
	}
}

func nativeWSContinuationFailure(failure string) *http.Response {
	if failure == "binding_lost" {
		return nil
	}
	return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{\"error\":{\"type\":\"invalid_request_error\",\"message\":\"Invalid `previous_response_id`.\"}}"))}
}

func nativeWSContinuationError(failure string) error {
	if failure == "binding_lost" {
		return &ResponsesContinuationLostError{Reason: "original_connection_unavailable"}
	}
	return nil
}

func assertNativeWSSuccess(t *testing.T, terminal []byte) {
	t.Helper()
	if "response.completed" != gjson.GetBytes(terminal, "type").String() {
		t.Fatalf("unexpected terminal: %s", terminal)
	}
}

func TestNativeWSMissingContextKeepsDownstreamUsable(t *testing.T) {
	fixture := newNativeWSFixture(t, func(_ context.Context, body []byte) (*http.Response, error) {
		if gjson.GetBytes(body, "previous_response_id").Exists() {
			return nil, &ResponsesContinuationLostError{Reason: "original_connection_unavailable"}
		}
		return nativeWSResponse("fresh"), nil
	})
	conn := fixture.connect(t)
	terminal := nativeWSTurn(t, conn, `{"previous_response_id":"missing","input":[{"role":"user","content":"next"}]}`)
	if "previous_response_not_found" != gjson.GetBytes(terminal, "error.code").String() || int64(409) != gjson.GetBytes(terminal, "status").Int() {
		t.Fatalf("missing continuation classification: %s", terminal)
	}
	assertNativeWSSuccess(t, nativeWSTurn(t, conn, `{"input":[{"role":"user","content":"full history"}]}`))
}

func TestNativeWSCacheRejectsIncompleteSnapshotsAndIsolatesLegacyNamespace(t *testing.T) {
	resetResponseCacheForTest()
	t.Cleanup(resetResponseCacheForTest)
	backend := newRecordingResponseContextBackend(true)
	SetResponseContextCache(backend)
	t.Cleanup(func() { backend.TokenCache.Close() })
	owner := nativeWSCachePrefix + "key:1"
	input := `[{"role":"user","content":"root"}]`
	completed := []byte(`{"response":{"id":"one","output":[]}}`)
	cacheResponsesWSCompletedResponse(owner, input, completed, []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque"}`)})
	next := []byte(`{"previous_response_id":"one","input":[]}`)
	if _, _, err := degradeResponsesWSContinuationWithSource(next, owner, newResponsesWSReplaySource(next, owner)); err == nil || err.Code != "previous_response_not_found" {
		t.Fatal("opaque reasoning was silently removed")
	}
	missing := []byte(`{"previous_response_id":"missing","input":[]}`)
	_, _, _ = degradeResponsesWSContinuationWithSource(missing, owner, newResponsesWSReplaySource(missing, owner))
	drainResponseCacheBackendWrites()
	if sets, gets := backend.counts(); sets != 0 || gets != 0 {
		t.Fatalf("native context accessed legacy namespace: writes=%d reads=%d", sets, gets)
	}
	config := defaultResponseCacheConfig()
	config.maxItems = 1
	configureResponseCacheForTest(config)
	cacheResponsesWSCompletedResponse(owner, input, completed, []json.RawMessage{json.RawMessage(`{"role":"assistant","content":"reply"}`)})
	if responseCacheLookupHit == nativeWSLocalLookup(owner, "one").Kind {
		t.Fatal("tail-trimmed context admitted")
	}
}

func TestNativeWSCommitBarrierAndScope(t *testing.T) {
	owner := nativeWSCacheOwner("key:1", requestSessionIdentity{affinityID: "thread", explicitUpstreamID: "thread"})
	other := nativeWSCacheOwner("key:1", requestSessionIdentity{affinityID: "other", explicitUpstreamID: "other"})
	if owner == other || owner == nativeWSCacheOwner("key:2", requestSessionIdentity{affinityID: "thread", explicitUpstreamID: "thread"}) {
		t.Fatal("cache scope is not isolated")
	}
	finish := beginNativeWSCommit(owner, "pending")
	defer finish()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitNativeWSCommit(ctx, owner, "pending"); err != context.Canceled {
		t.Fatalf("commit barrier ignored cancellation: %v", err)
	}
	finish()
	if err := waitNativeWSCommit(context.Background(), owner, "pending"); err != nil {
		t.Fatal(err)
	}
}

func TestPreviousIDErrorRecognitionIsNarrow(t *testing.T) {
	for _, message := range []string{"Invalid `previous_response_id`.", "Invalid previous_response_id", "Invalid model", "Invalid request", "previous_response_id format unsupported"} {
		payload, _ := json.Marshal(map[string]any{"error": map[string]string{"type": "invalid_request_error", "message": message}})
		want := strings.HasPrefix(message, "Invalid ") && strings.Contains(message, "previous_response_id")
		if want != isPreviousResponseNotFoundBody(payload) {
			t.Fatal(fmt.Sprintf("wrong classification: %s", payload))
		}
	}
}
