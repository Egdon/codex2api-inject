package wsrelay

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestTurnStateProxyWebsocketIsolationAndReuse(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	cfg := previous
	cfg.CodexTurnStateTemplateCache = true
	cfg.CodexTurnStateAccountMode = "personal"
	proxy.ApplyRuntimeSettings(cfg)
	defer proxy.ApplyRuntimeSettings(previous)
	oldResin := proxy.GetResinConfig()
	defer proxy.SetResinConfig(oldResin)
	var ordinary, dedicated, handshakes atomic.Int32
	healthy := wsTurnStateToken(10)
	handler := func(counter *atomic.Int32, isDedicated bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if isDedicated && r.Header.Get("X-Resin-Account") != "" {
				t.Error("dedicated WS went through Resin")
			}
			conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			for {
				_, frame, err := conn.ReadMessage()
				if err != nil {
					return
				}
				n := counter.Add(1)
				if isDedicated {
					state := gjson.GetBytes(frame, "client_metadata.x-codex-turn-state").String()
					if n == 1 && state != "" || n == 2 && state != healthy {
						t.Error("wrong acquisition/validation frame state")
					}
					if n == 1 {
						conn.WriteJSON(map[string]any{"type": "response.metadata", "headers": map[string]string{"X-Codex-Turn-State": healthy}})
					}
				}
				conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_fixture","status":"completed","output":[]}}`))
			}
		}
	}
	upstream := httptest.NewTLSServer(handler(&dedicated, true))
	defer upstream.Close()
	tunnel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			t.Error("expected CONNECT")
			w.WriteHeader(400)
			return
		}
		handshakes.Add(1)
		target, err := net.Dial("tcp", upstream.Listener.Addr().String())
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			target.Close()
			t.Error(err)
			return
		}
		fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { defer conn.Close(); defer target.Close(); io.Copy(target, conn) }()
		io.Copy(conn, target)
	}))
	defer tunnel.Close()
	normal := httptest.NewServer(handler(&ordinary, false))
	defer normal.Close()
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: normal.URL, PlatformName: "test"})
	manager := NewManager()
	defer manager.Stop()
	manager.dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Local TLS fixture only.
	executor := NewExecutorWithManager(manager)
	account := &auth.Account{DBID: 9934, AccessToken: "fixture", PlanType: "pro", ProxyURL: "http://normal.invalid:8080", CodexTurnStateProxyURL: tunnel.URL}
	proxy.ClearCodexTurnStateTemplatesForAccount(account.ID())
	defer proxy.ClearCodexTurnStateTemplatesForAccount(account.ID())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	call := func(ctx context.Context) {
		resp, err := executor.ExecuteRequestViaWebsocket(ctx, account, []byte(`{"model":"gpt-5.6-luna","input":"hi"}`), "same-session", account.ProxyURL, "", nil, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		httpResp := websocketResponseToHTTP(ctx, resp, http.StatusOK, nil)
		defer httpResp.Body.Close()
		if _, err := io.ReadAll(httpResp.Body); err != nil {
			t.Fatal(err)
		}
	}
	call(ctx) // Populate the ordinary pool before acquiring through the dedicated exit.
	acquire, candidate := proxy.NewCodexTurnStateRefresh(ctx, account.ID(), "gpt-5.6-luna", nil, tunnel.URL)
	call(acquire)
	if !candidate.HasCandidate() {
		t.Fatal("WS acquisition lost candidate")
	}
	verify, validated := proxy.NewCodexTurnStateRefresh(ctx, account.ID(), "gpt-5.6-luna", candidate)
	call(verify)
	if !validated.SaveVerified(account) {
		t.Fatal("WS validation failed")
	}
	call(ctx)
	if ordinary.Load() != 2 || dedicated.Load() != 2 || handshakes.Load() != 1 {
		t.Fatalf("ordinary=%d dedicated=%d proxy dials=%d", ordinary.Load(), dedicated.Load(), handshakes.Load())
	}
}
