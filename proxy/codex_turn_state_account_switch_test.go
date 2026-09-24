package proxy

import (
	"context"
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/turnstate"
	"github.com/tidwall/gjson"
)

func TestTurnStateAccountSwitchDoesNotInjectManualCredential(t *testing.T) {
	// v3.0.1 deleted the credential field and the passive template cache.
	// Fork prepareCodexTurnStateInjection still injects a harvest ticket only
	// when the harvest inject switch is on, so an off switch leaves the client
	// header and frame alone.
	turnstate.SetConfig(turnstate.DefaultConfig())
	t.Cleanup(func() { turnstate.SetConfig(turnstate.DefaultConfig()) })
	account := &auth.Account{DBID: 82}
	for _, websocket := range []bool{false, true} {
		body := []byte(`{"model":"gpt-5.6-luna","client_metadata":{"x-codex-turn-state":"client-state"}}`)
		original := http.Header{codexTurnStateHeader: []string{"client-state"}}
		ctx, rewritten, headers := prepareCodexTurnStateInjection(withCodexTurnStateInjection(context.Background(), "previous-attempt"), account, body, original, websocket)
		if headers.Get(codexTurnStateHeader) != "client-state" {
			t.Fatal("inject switch off replaced the client header")
		}
		if gjson.GetBytes(rewritten, "client_metadata.x-codex-turn-state").String() != "client-state" {
			t.Fatal("inject switch off rewrote the frame")
		}
		if CodexTurnStateInjectionFromContext(ctx) != "previous-attempt" {
			t.Fatal("inject switch off changed the caller context")
		}
		if original.Get(codexTurnStateHeader) != "client-state" {
			t.Fatal("prepare mutated caller headers")
		}
	}
}
