package wsrelay

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func wsTurnStateToken(blocks int) string {
	raw := make([]byte, 57+16*blocks)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(time.Now().Unix()))
	return base64.URLEncoding.EncodeToString(raw)
}

func turnStateTemplateExecutor(t *testing.T, state string) (*Executor, *auth.Account, <-chan []byte) {
	t.Helper()
	previous := proxy.CurrentRuntimeSettings()
	next := previous
	next.CodexTurnStateTemplateCache = true
	next.CodexTurnStateAccountMode = proxy.CodexTurnStateAccountModePersonal
	proxy.ApplyRuntimeSettings(next)
	t.Setenv("CODEX_TURN_STATE_INJECT_MODE", "replace-only")
	t.Setenv("CODEX_TURN_STATE_DRY_RUN", "false")
	proxy.ClearCodexTurnStateTemplatesForAccount(1)
	t.Cleanup(func() {
		proxy.ApplyRuntimeSettings(previous)
		proxy.ClearCodexTurnStateTemplatesForAccount(1)
	})
	frames := make(chan []byte, 4)
	manager, wc := newReadPumpTestConnection(t, func(conn *websocket.Conn) {
		for {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				return
			}
			frames <- frame
			// Duplicate metadata frames are still one response observation.
			for i := 0; i < 2; i++ {
				_ = conn.WriteJSON(map[string]any{"type": "codex.response.metadata", "metadata": map[string]string{"x-codex-turn-state": state}})
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_turn_state","status":"completed","output":[]}}`))
		}
	})
	wsURL, err := buildWebsocketURL(proxy.CodexBaseURL + CodexWsEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	key := manager.poolKey(1, wsURL, "turn-state-test", "")
	wc.URL, wc.PoolKey = wsURL, key
	wc.session.ID = "turn-state-test"
	wc.SetState(StateConnected)
	wc.Touch()
	manager.connections.Store(key, wc)
	manager.sessions.Store(key, wc.session)
	manager.probeFunc = func(*WsConnection) bool { return true }
	return NewExecutorWithManager(manager), &auth.Account{DBID: 1, AccessToken: "test-token", PlanType: "pro"}, frames
}

func TestTurnStateTemplateHarvestsWebsocketMetadata(t *testing.T) {
	healthy, degraded := wsTurnStateToken(10), wsTurnStateToken(11)
	exec, account, _ := turnStateTemplateExecutor(t, healthy)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ws, err := exec.ExecuteRequestViaWebsocket(ctx, account, []byte(`{"model":"gpt-5.6-luna","input":"hi"}`), "turn-state-test", "", "", nil, http.Header{}, "")
	if err != nil {
		t.Fatal(err)
	}
	response := websocketResponseToHTTP(ctx, ws, http.StatusOK, http.Header{})
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"X-Codex-Turn-State": []string{degraded}}
	proxy.ApplyCodexTurnStateTemplate(ctx, headers, account, "gpt-5.6-luna")
	if headers.Get("X-Codex-Turn-State") != healthy {
		t.Fatal("upstream WebSocket metadata was not cached for the next degraded request")
	}
}

func TestTurnStateTemplateRewritesReusedWebsocketFrame(t *testing.T) {
	healthy, degraded := wsTurnStateToken(10), wsTurnStateToken(11)
	exec, account, frames := turnStateTemplateExecutor(t, healthy)
	proxy.CaptureCodexTurnStateTemplate(context.Background(), account, "gpt-5.6-luna", http.Header{"X-Codex-Turn-State": []string{healthy}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	body := []byte(`{"model":"gpt-5.6-luna","input":"hi","client_metadata":{"x-codex-turn-state":"` + degraded + `"}}`)
	ws, err := exec.ExecuteRequestViaWebsocket(ctx, account, body, "turn-state-test", "", "", nil, http.Header{"X-Codex-Turn-State": []string{degraded}}, "")
	if err != nil {
		t.Fatal(err)
	}
	response := websocketResponseToHTTP(ctx, ws, http.StatusOK, http.Header{})
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	frame := <-frames
	if gjson.GetBytes(frame, "client_metadata.x-codex-turn-state").String() != healthy {
		t.Fatal("template rewrite did not reach the reused connection's response.create frame")
	}
}

func TestTurnStateTemplateWebsocketKeepsLiveTemplateAfterFailedObservations(t *testing.T) {
	healthy, degraded := wsTurnStateToken(10), wsTurnStateToken(11)
	exec, account, frames := turnStateTemplateExecutor(t, degraded)
	proxy.CaptureCodexTurnStateTemplate(nil, account, "gpt-5.6-luna", http.Header{"X-Codex-Turn-State": []string{healthy}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i := 1; i <= 3; i++ {
		// Body-only input must be replaced as well; it must never seed the cache.
		body := []byte(`{"model":"gpt-5.6-luna","input":"hi","client_metadata":{"x-codex-turn-state":"` + degraded + `"}}`)
		ws, err := exec.ExecuteRequestViaWebsocket(ctx, account, body, "turn-state-test", "", "", nil, http.Header{}, "")
		if err != nil {
			t.Fatal(err)
		}
		response := websocketResponseToHTTP(ctx, ws, http.StatusOK, http.Header{"X-Codex-Turn-State": []string{healthy}})
		if response.Header.Get("X-Codex-Turn-State") != "" {
			t.Fatal("stale handshake state was relayed")
		}
		if _, err := io.ReadAll(response.Body); err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		got := gjson.GetBytes(<-frames, "client_metadata.x-codex-turn-state").String()
		want := healthy
		if got != want {
			t.Fatalf("turn %d: live template must survive failed observations", i)
		}
		state := proxy.GetCodexTurnStateStatus(account).State
		wantState := "recovering"
		if state != wantState {
			t.Fatalf("turn %d: state=%s, want %s", i, state, wantState)
		}
	}
}

func TestTurnStateAccountSwitchKeepsWebsocketObservations(t *testing.T) {
	healthy, degraded := wsTurnStateToken(10), wsTurnStateToken(11)
	exec, account, frames := turnStateTemplateExecutor(t, degraded)
	proxy.CaptureCodexTurnStateTemplate(nil, account, "gpt-5.6-luna", http.Header{"X-Codex-Turn-State": []string{healthy}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, disabled := range []bool{true, true, false} {
		account.CodexTurnStateDisabled = disabled
		body := []byte(`{"model":"gpt-5.6-luna","input":"hi","client_metadata":{"x-codex-turn-state":"` + degraded + `"}}`)
		ws, err := exec.ExecuteRequestViaWebsocket(ctx, account, body, "turn-state-test", "", "", nil, http.Header{}, "")
		if err != nil {
			t.Fatal(err)
		}
		response := websocketResponseToHTTP(ctx, ws, http.StatusOK, http.Header{})
		if _, err := io.ReadAll(response.Body); err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		got := gjson.GetBytes(<-frames, "client_metadata.x-codex-turn-state").String()
		want := healthy
		if disabled {
			want = degraded
		}
		if got != want {
			t.Fatal("frame ignored account switch")
		}
		status := proxy.GetCodexTurnStateStatus(account)
		if len(status.Models) != 1 || status.Models[0].Length != len(degraded) {
			t.Fatal("disabled account lost observations")
		}
		if disabled && (status.State == "recovering" || status.State == "ready") {
			t.Fatal("disabled injection falsely reported recovery")
		}
		if !disabled && status.State != "recovering" {
			t.Fatal("re-enabled account lost saved template")
		}
	}
}
