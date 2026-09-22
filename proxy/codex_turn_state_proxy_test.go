package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestTurnStateProxyScopesEgressToRefresh(t *testing.T) {
	account := &auth.Account{DBID: 9931, CodexTurnStateProxyURL: "socks5://issuance:1080"}
	acquire, candidate := NewCodexTurnStateRefresh(context.Background(), account.ID(), "gpt-5.6-luna", nil, account.CodexTurnStateProxy())
	verify, _ := NewCodexTurnStateRefresh(context.Background(), account.ID(), "gpt-5.6-luna", candidate)
	inherited, _ := NewCodexTurnStateRefresh(context.Background(), account.ID(), "gpt-5.6-luna", nil)
	for _, resin := range []bool{false, true} {
		t.Run(fmt.Sprint(resin), func(t *testing.T) {
			if resin {
				withResin(t, &ResinConfig{BaseURL: "http://resin.test", PlatformName: "test"})
			} else {
				withResin(t, nil)
			}
			for _, ws := range []bool{false, true} {
				for _, ctx := range []context.Context{acquire, verify} {
					e := ResolveCodexRequestEgress(ctx, account, "https://upstream.test/responses", "http://normal:8080", ws)
					if e.Kind != CodexEgressProxy || e.DialProxyURL != account.CodexTurnStateProxyURL || e.URL != "https://upstream.test/responses" {
						t.Fatalf("refresh route: %+v", e)
					}
				}
				for _, ctx := range []context.Context{context.Background(), WithCodexTurnStateAdminProbe(nil), inherited} {
					e := ResolveCodexRequestEgress(ctx, account, "https://upstream.test/responses", "http://normal:8080", ws)
					if resin && !e.ViaResin() || !resin && e.DialProxyURL != "http://normal:8080" {
						t.Fatalf("ordinary route changed: %+v", e)
					}
				}
			}
		})
	}
	if CodexTurnStateRefreshProxy(acquire, &auth.Account{DBID: 9932}) != "" {
		t.Fatal("cross-account override")
	}
}

func TestTurnStateProxyHTTPAcquisitionAndValidation(t *testing.T) {
	enableTurnStateTemplateCache(t)
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	var ordinary, dedicated, turns atomic.Int32
	healthy := syntheticTurnState(10, time.Now(), 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn := turns.Add(1)
		if r.Header.Get("X-Resin-Account") != "" {
			t.Error("dedicated request used Resin headers")
		}
		if turn == 1 {
			if r.Header.Get(codexTurnStateHeader) != "" {
				t.Error("acquisition carried previous state")
			}
			w.Header().Set(codexTurnStateHeader, healthy)
		} else if r.Header.Get(codexTurnStateHeader) != healthy {
			t.Error("validation did not carry acquired template")
		}
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer upstream.Close()
	tunnel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			t.Error("expected CONNECT")
			w.WriteHeader(400)
			return
		}
		dedicated.Add(1)
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
	normal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ordinary.Add(1); fmt.Fprint(w, "ok") }))
	defer normal.Close()
	withResin(t, &ResinConfig{BaseURL: normal.URL, PlatformName: "test"})
	account := &auth.Account{DBID: 9933, AccessToken: "fixture", PlanType: "pro", ProxyURL: "http://ordinary.invalid:8080", CodexTurnStateProxyURL: tunnel.URL}
	client := getPooledClient(account, tunnel.URL)
	tr := client.Transport.(*http.Transport)
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Local TLS fixture only.
	defer func() {
		tr.CloseIdleConnections()
		clientPool.Delete(clientPoolKey(account, tunnel.URL, codexTransportModeStandard))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	acquire, candidate := NewCodexTurnStateRefresh(ctx, account.ID(), "gpt-5.6-luna", nil, tunnel.URL)
	payload := []byte(`{"model":"gpt-5.6-luna","input":"hi","stream":true}`)
	call := func(ctx context.Context) {
		resp, err := ExecuteRequest(ctx, account, payload, "", account.ProxyURL, "", nil, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatal(err)
		}
	}
	call(acquire)
	if !candidate.HasCandidate() {
		t.Fatal("acquisition failed")
	}
	verify, verified := NewCodexTurnStateRefresh(ctx, account.ID(), "gpt-5.6-luna", candidate)
	call(verify)
	if !verified.SaveVerified(account) || turns.Load() != 2 || dedicated.Load() == 0 || ordinary.Load() != 0 {
		t.Fatal("refresh did not exclusively use dedicated proxy")
	}
	call(ctx)
	if ordinary.Load() != 1 || turns.Load() != 2 {
		t.Fatal("ordinary request used issuance proxy")
	}
	// A broken dedicated exit must not silently fall back to Resin/default.
	broken, _ := NewCodexTurnStateRefresh(ctx, account.ID(), "gpt-5.6-luna", nil, "http://127.0.0.1:1")
	if resp, err := ExecuteRequest(broken, account, payload, "", account.ProxyURL, "", nil, nil, false); err == nil {
		resp.Body.Close()
		t.Fatal("broken proxy fell back")
	}
	if ordinary.Load() != 1 || !strings.Contains(account.ProxyURL, "ordinary.invalid") {
		t.Fatal("default route changed")
	}
}
