package proxy

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/turnstate"
	"github.com/tidwall/gjson"
)

func fakeHarvestToken(issued int64) string {
	raw := make([]byte, 1+8+16+160+32)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(issued))
	return base64.URLEncoding.EncodeToString(raw)
}

func turnStateTraceContext() (context.Context, *upstreamTraceAudit) {
	audit := &upstreamTraceAudit{requestID: "req-1"}
	return context.WithValue(context.Background(), upstreamTraceContextKey{}, audit), audit
}

func TestExecuteRequestInjectsHarvestTurnState(t *testing.T) {
	turnstate.SetConfig(turnstate.Config{InjectEnabled: true})
	t.Cleanup(func() {
		turnstate.SetConfig(turnstate.DefaultConfig())
		turnstate.Global().ReplaceAll(nil)
	})
	now := time.Now().Unix()
	injected := fakeHarvestToken(now)
	for _, tc := range []struct {
		name         string
		id           int64
		putTicket    bool
		customHeader string
		wantHeader   string
		wantInjected string
	}{
		{name: "harvest ticket beats client header and custom header", id: 4242, putTicket: true, customHeader: "custom-header-state", wantHeader: injected, wantInjected: injected},
		{name: "no harvest ticket keeps client echo", id: 4243, wantHeader: "client-state"},
		{name: "no harvest ticket lets custom header win", id: 4244, customHeader: "custom-header-state", wantHeader: "custom-header-state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.putTicket {
				turnstate.Global().Put(turnstate.CachedTicket{
					AccountID: tc.id, Model: "gpt-5.5", Token: injected,
					IssuedUnix: now, Length: 292, Blocks: 10,
				})
			}
			account := &auth.Account{DBID: tc.id, AccessToken: "token-1"}
			if tc.customHeader != "" {
				account.CustomHeaders = map[string]string{"X-Codex-Turn-State": tc.customHeader}
			}

			var capturedHeader http.Header
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedHeader = r.Header.Clone()
				w.Header().Set("X-Codex-Turn-State", "minted-by-upstream")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			previousResin := resinCfg.Load()
			t.Cleanup(func() { resinCfg.Store(previousResin) })
			SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "test"})
			clientPool.Delete(fmt.Sprintf("resin|%d", account.ID()))

			ctx, _ := turnStateTraceContext()
			ctx = WithCodexClientModel(ctx, "gpt-5.5")
			downstream := http.Header{}
			downstream.Set("X-Codex-Turn-State", "client-state")
			resp, err := ExecuteRequest(ctx, account, []byte(`{"model":"gpt-5.5","input":"hi"}`), "", "", "api-key-1", nil, downstream, false)
			if err != nil {
				t.Fatalf("ExecuteRequest: %v", err)
			}
			_ = resp.Body.Close()
			if got := capturedHeader.Get("X-Codex-Turn-State"); got != tc.wantHeader {
				t.Fatalf("outbound X-Codex-Turn-State = %q, want %q", got, tc.wantHeader)
			}
			if got := downstream.Get("X-Codex-Turn-State"); got != "client-state" {
				t.Fatalf("downstream headers mutated: %q", got)
			}
			snapshot := snapshotUpstreamTrace(ctx)
			if snapshot.InjectedTurnState != tc.wantInjected {
				t.Fatalf("trace injected = %q, want %q", snapshot.InjectedTurnState, tc.wantInjected)
			}
			if snapshot.UpstreamTurnState != "minted-by-upstream" {
				t.Fatalf("trace upstream = %q, want minted-by-upstream", snapshot.UpstreamTurnState)
			}
		})
	}
}

func TestPrepareCodexTurnStateInjectionWebsocketBody(t *testing.T) {
	turnstate.SetConfig(turnstate.Config{InjectEnabled: true})
	t.Cleanup(func() {
		turnstate.SetConfig(turnstate.DefaultConfig())
		turnstate.Global().ReplaceAll(nil)
	})
	now := time.Now().Unix()
	wsState := fakeHarvestToken(now)
	turnstate.Global().Put(turnstate.CachedTicket{
		AccountID: 7, Model: "gpt-5.5", Token: wsState,
		IssuedUnix: now, Length: 292, Blocks: 10,
	})
	account := &auth.Account{DBID: 7}
	ctx, body, headers := prepareCodexTurnStateInjection(context.Background(), account, []byte(`{"model":"gpt-5.5"}`), nil, true)
	if got := gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String(); got != wsState {
		t.Fatalf("frame client_metadata = %q, want harvest token", got)
	}
	if got := headers.Get("X-Codex-Turn-State"); got != wsState {
		t.Fatalf("handshake header = %q, want harvest token", got)
	}
	if got := CodexTurnStateInjectionFromContext(ctx); got != wsState {
		t.Fatalf("ctx injection = %q, want harvest token", got)
	}
	_, httpBody, _ := prepareCodexTurnStateInjection(context.Background(), account, []byte(`{"model":"gpt-5.5"}`), nil, false)
	if gjson.GetBytes(httpBody, "client_metadata").Exists() {
		t.Fatal("HTTP path must not fabricate client_metadata")
	}
	plain := &auth.Account{DBID: 8}
	ctx, body, headers = prepareCodexTurnStateInjection(context.Background(), plain, []byte(`{"model":"gpt-5.5"}`), nil, true)
	if headers != nil || gjson.GetBytes(body, "client_metadata").Exists() || CodexTurnStateInjectionFromContext(ctx) != "" {
		t.Fatal("unconfigured account must be a no-op")
	}
}

func TestObserveCodexTurnStateFrame(t *testing.T) {
	ctx, audit := turnStateTraceContext()
	audit.current = &upstreamTraceAttempt{accountID: 1}
	ObserveCodexTurnStateFrame(ctx, []byte(`{"type":"response.output_text.delta","delta":"turn-state is a phrase"}`))
	if audit.current.upstreamTurnState != "" {
		t.Fatalf("content frame must not be treated as turn state: %q", audit.current.upstreamTurnState)
	}
	ObserveCodexTurnStateFrame(ctx, []byte(`{"type":"response.metadata","headers":{"X-Codex-Turn-State":"frame-state"}}`))
	if audit.current.upstreamTurnState != "frame-state" {
		t.Fatalf("metadata frame turn state = %q, want frame-state", audit.current.upstreamTurnState)
	}
	ObserveCodexTurnStateFrame(ctx, []byte(`{"type":"response.metadata","client_metadata":{"x-codex-turn-state":"later-state"}}`))
	if audit.current.upstreamTurnState != "later-state" {
		t.Fatalf("later frame must win: %q", audit.current.upstreamTurnState)
	}
	if got := codexTurnStateFromFrame([]byte(`{"headers":{"x-codex-turn-state":"bad\nvalue"}}`)); got != "" {
		t.Fatalf("control characters must be rejected: %q", got)
	}
}

func TestPrepareCodexTurnStateInjectionOffIsNoop(t *testing.T) {
	turnstate.SetConfig(turnstate.DefaultConfig())
	account := &auth.Account{DBID: 9, CodexTurnState: "should-not-inject"}
	ctx, body, headers := prepareCodexTurnStateInjection(context.Background(), account, []byte(`{"model":"gpt-6-astra"}`), http.Header{"X-Codex-Turn-State": {"client"}}, true)
	if CodexTurnStateInjectionFromContext(ctx) != "" {
		t.Fatal("inject switch off must not inject")
	}
	if headers.Get("X-Codex-Turn-State") != "client" {
		t.Fatalf("client header mutated: %q", headers.Get("X-Codex-Turn-State"))
	}
	if gjson.GetBytes(body, "client_metadata.x-codex-turn-state").Exists() {
		t.Fatal("WS metadata must stay untouched when inject is off")
	}
}

func TestPrepareCodexTurnStateInjectionHarvestCacheByModel(t *testing.T) {
	turnstate.SetConfig(turnstate.Config{InjectEnabled: true})
	t.Cleanup(func() {
		turnstate.SetConfig(turnstate.DefaultConfig())
		turnstate.Global().ReplaceAll(nil)
	})
	now := time.Now().Unix()
	astra := fakeHarvestToken(now)
	turnstate.Global().Put(turnstate.CachedTicket{
		AccountID: 11, Model: "gpt-6-astra", Token: astra,
		IssuedUnix: now, Length: 292, Blocks: 10,
	})
	turnstate.Global().Put(turnstate.CachedTicket{
		AccountID: 11, Model: "gpt-5.6-sol", Token: "sol-expired",
		IssuedUnix: now - 4000, Length: 292, Blocks: 10,
	})
	account := &auth.Account{DBID: 11, CodexTurnState: "credential-fallback"}
	_, body, headers := prepareCodexTurnStateInjection(context.Background(), account, []byte(`{"model":"gpt-6-astra"}`), nil, true)
	if headers.Get("X-Codex-Turn-State") != astra {
		t.Fatalf("astra harvest ticket not used: %q", headers.Get("X-Codex-Turn-State"))
	}
	if gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != astra {
		t.Fatal("WS frame must carry harvest ticket")
	}
	_, _, solHeaders := prepareCodexTurnStateInjection(context.Background(), account, []byte(`{"model":"gpt-5.6-sol"}`), nil, true)
	if solHeaders != nil && solHeaders.Get("X-Codex-Turn-State") != "" {
		t.Fatalf("expired sol ticket must not inject, got %q", solHeaders.Get("X-Codex-Turn-State"))
	}
}
