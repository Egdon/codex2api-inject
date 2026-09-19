package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/proxy/turnstate"
	"github.com/gin-gonic/gin"
)

// These tests use only in-process HTTP handlers and the local test database.
// The harvester is never started and no probe is enqueued.
func TestTurnStateSettingsLegacyProviderAndLitportFields(t *testing.T) {
	current := turnstate.DefaultConfig()
	current.HarvestProxyProvider = "litport"
	current.LitportHost = "fixture.example:1337"
	current.LitportUsername = "fixture-user"
	current.LitportRegion = "FR"
	current.LitportRegionMode = "rotation"
	current.LitportSessionSeconds = 900
	current.LitportPassword = "fixture-secret"
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"zoo_host":"zoo.example:5000","max_attempts":8}`), &raw); err != nil {
		t.Fatal(err)
	}
	got, err := parseTurnStateSettings(raw, current)
	if err != nil {
		t.Fatal(err)
	}
	if got.HarvestProxyProvider != "litport" || got.LitportHost != current.LitportHost || got.LitportUsername != current.LitportUsername || got.LitportRegion != "FR" || got.LitportRegionMode != "rotation" || got.LitportSessionSeconds != 900 {
		t.Fatal("legacy PUT lost the selected provider or omitted Litport settings")
	}
	if got.LitportPassword != "" {
		t.Fatal("omitted password must be preserved later under the SaveConfig lock")
	}
	legacy, err := parseTurnStateSettings(raw, turnstate.Config{})
	if err != nil || legacy.HarvestProxyProvider != "zooproxy" {
		t.Fatalf("missing legacy provider must default to ZooProxy: %v", err)
	}
}

func TestTurnStateSettingsRejectInvalidProxyInput(t *testing.T) {
	h := &Handler{}
	for _, body := range []string{
		`{"harvest_proxy_provider":"unknown"}`,
		`{"harvest_proxy_provider":""}`,
		`{"harvest_proxy_provider":null}`,
		`{"harvest_proxy_provider":12}`,
		`{"harvest_proxy_provider":"litport","litport_host":"http://fixture:secret@example.test:1337"}`,
		`{"litport_session_seconds":0}`,
		`{"litport_session_seconds":null}`,
		`{"litport_session_seconds":-1}`,
		`{"litport_session_seconds":86401}`,
		`{"litport_session_seconds":1.5}`,
	} {
		t.Run(body, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPut, "/settings/turn-state", strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			h.UpdateTurnStateSettings(c)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("invalid proxy settings returned %d, want 400", w.Code)
			}
			if strings.Contains(w.Body.String(), "fixture:secret") {
				t.Fatal("validation response exposed endpoint credentials")
			}
		})
	}
	for _, seconds := range []string{"1", "86400"} {
		raw := map[string]json.RawMessage{"litport_session_seconds": json.RawMessage(seconds)}
		if _, err := parseTurnStateSettings(raw, turnstate.DefaultConfig()); err != nil {
			t.Fatalf("valid TTL boundary %s rejected: %v", seconds, err)
		}
	}
}

func TestTurnStateSettingsPasswordsAndProviderSwitch(t *testing.T) {
	previous := turnstate.GetConfig()
	t.Cleanup(func() { turnstate.SetConfig(previous) })
	cfg := turnstate.DefaultConfig()
	cfg.ZooPassword = "fixture-zoo-secret"
	cfg.LitportPassword = "fixture-litport-secret"
	cfg.LitportUsername = "fixture-user"
	cfg.DisabledAccountIDs = []int64{42}
	turnstate.SetConfig(cfg)
	h := &Handler{}
	h.SetTurnStateHarvest(turnstate.NewHarvester(newTestAdminDB(t), nil, turnstate.NewCache()))

	request := func(method, body string) map[string]json.RawMessage {
		t.Helper()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(method, "/settings/turn-state", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		if method == http.MethodGet {
			h.GetTurnStateSettings(c)
		} else {
			h.UpdateTurnStateSettings(c)
		}
		if w.Code != http.StatusOK {
			t.Fatalf("settings request failed: HTTP %d", w.Code)
		}
		var public map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &public); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"zoo_password", "litport_password"} {
			if value, exists := public[field]; exists && string(value) != `""` {
				t.Fatalf("public config exposes %s", field)
			}
			if string(public[field+"_set"]) != "true" {
				t.Fatalf("public config missing %s_set", field)
			}
		}
		return public
	}

	request(http.MethodGet, "")
	request(http.MethodPut, `{"harvest_proxy_provider":"litport","zoo_password":" ","litport_password":""}`)
	got := turnstate.GetConfig()
	if got.HarvestProxyProvider != "litport" || got.ZooPassword != cfg.ZooPassword || got.LitportPassword != cfg.LitportPassword {
		t.Fatal("switch with blank passwords must independently preserve both")
	}
	request(http.MethodPut, `{"litport_password":"fixture-litport-replacement"}`)
	got = turnstate.GetConfig()
	if got.HarvestProxyProvider != "litport" || got.LitportUsername != "fixture-user" || got.ZooPassword != cfg.ZooPassword || got.LitportPassword != "fixture-litport-replacement" {
		t.Fatal("legacy PUT must preserve active provider and Zoo password while updating Litport password")
	}
	request(http.MethodPut, `{"harvest_proxy_provider":"zooproxy","zoo_password":"fixture-zoo-replacement","litport_password":" "}`)
	got = turnstate.GetConfig()
	if got.HarvestProxyProvider != "zooproxy" || got.ZooPassword != "fixture-zoo-replacement" || got.LitportPassword != "fixture-litport-replacement" {
		t.Fatal("Zoo password update must not erase the inactive Litport password")
	}
	if len(got.DisabledAccountIDs) != 1 || got.DisabledAccountIDs[0] != 42 {
		t.Fatal("settings saves changed omitted account participation")
	}
	request(http.MethodGet, "")
}
