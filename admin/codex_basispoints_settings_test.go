package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

func TestBasispointsSettingsPersistenceAndReload(t *testing.T) {
	h, db, _ := newImagesSettingsHandler(t)
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings()) })
	for _, tc := range []struct {
		patch      map[string]any
		want       bool
		wantModels string
	}{
		{map[string]any{"codex_basispoints_enabled": true, "codex_basispoints_models": "GPT-6-Astra, gpt-5.6-sol"}, true, "gpt-6-astra,gpt-5.6-sol"},
		{map[string]any{"site_name": "unrelated patch"}, true, "gpt-6-astra,gpt-5.6-sol"},
		{map[string]any{"codex_basispoints_enabled": false, "codex_basispoints_models": ""}, false, ""},
	} {
		// A stale instance must preserve the persisted switch on unrelated updates.
		proxy.UpdateRuntimeSettings(func(s proxy.RuntimeSettings) proxy.RuntimeSettings {
			s.CodexBasispointsEnabled = false
			s.CodexBasispointsModels = ""
			return s
		})
		response := invokeResponseCacheSettingsAdmin(t, h, http.MethodPut, tc.patch)
		if response.Code != 200 {
			t.Fatalf("PUT: %d %s", response.Code, response.Body.String())
		}
		got := decodeResponseCacheSettingsResponse(t, response)
		runtime := proxy.CurrentRuntimeSettings()
		if got.CodexBasispointsEnabled != tc.want || runtime.CodexBasispointsEnabled != tc.want || auth.ExcelBPSGlobalEnabled() != tc.want {
			t.Fatalf("PUT/runtime/auth enabled mismatch for %v", tc.patch)
		}
		if got.CodexBasispointsModels != tc.wantModels || runtime.CodexBasispointsModels != tc.wantModels {
			t.Fatalf("PUT/runtime models = %q/%q, want %q", got.CodexBasispointsModels, runtime.CodexBasispointsModels, tc.wantModels)
		}
		saved, err := db.GetSystemSettings(context.Background())
		if err != nil || saved.CodexBasispointsEnabled != tc.want || saved.CodexBasispointsModels != tc.wantModels {
			t.Fatalf("persisted mismatch: %+v %v", saved, err)
		}
		proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
		proxy.ApplyRuntimeSettingsFromSystem(saved)
		get := invokeResponseCacheSettingsAdmin(t, h, http.MethodGet, nil)
		reloaded := decodeResponseCacheSettingsResponse(t, get)
		if get.Code != 200 || reloaded.CodexBasispointsEnabled != tc.want || reloaded.CodexBasispointsModels != tc.wantModels || auth.ExcelBPSGlobalEnabled() != tc.want {
			t.Fatal("reload/GET mismatch")
		}
	}
}

func TestBasispointsSettingsFailedSavePreservesRuntime(t *testing.T) {
	h, db, path := newImagesSettingsHandler(t)
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings()) })
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER reject_bps_settings BEFORE INSERT ON system_settings BEGIN SELECT RAISE(ABORT, 'forced write failure'); END`); err != nil {
		t.Fatal(err)
	}
	response := invokeResponseCacheSettingsAdmin(t, h, http.MethodPut, map[string]any{"codex_basispoints_enabled": true})
	if response.Code != 500 {
		t.Fatalf("PUT status=%d", response.Code)
	}
	saved, err := db.GetSystemSettings(context.Background())
	if err != nil || saved.CodexBasispointsEnabled || proxy.CurrentRuntimeSettings().CodexBasispointsEnabled || auth.ExcelBPSGlobalEnabled() {
		t.Fatal("failed save enabled routing")
	}
}

func TestParseAccountSchedulerUpdateExcelBPSOptOut(t *testing.T) {
	update, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{
		ExcelBPSEnabled: json.RawMessage(`false`),
		ExcelBPSOptOut:  json.RawMessage(`true`),
	})
	if err != nil {
		t.Fatalf("parseAccountSchedulerUpdate: %v", err)
	}
	if !update.hasChanges() || !update.ExcelBPSOptOut.Set || !update.ExcelBPSOptOut.Value {
		t.Fatalf("opt-out update = %#v", update.ExcelBPSOptOut)
	}
	if value, ok := update.CredentialUpdates[auth.ExcelBPSOptOutCredentialKey].(bool); !ok || !value {
		t.Fatalf("credential update = %#v", update.CredentialUpdates)
	}
	if _, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{ExcelBPSOptOut: json.RawMessage(`1`)}); err == nil {
		t.Fatal("non-boolean opt-out was accepted")
	}
}

func TestBasispoints403SettingsPersistValidateAndReload(t *testing.T) {
	h, db, _ := newImagesSettingsHandler(t)
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings()) })
	get := decodeResponseCacheSettingsResponse(t, invokeResponseCacheSettingsAdmin(t, h, http.MethodGet, nil))
	if !get.CodexBasispoints403AutoPause || get.CodexBasispoints403ProbeIntervalMin != 1 {
		t.Fatalf("defaults = auto_pause %t interval %d, want true/1", get.CodexBasispoints403AutoPause, get.CodexBasispoints403ProbeIntervalMin)
	}
	for _, invalid := range []int{0, -1, 10081} {
		response := invokeResponseCacheSettingsAdmin(t, h, http.MethodPut, map[string]any{"codex_basispoints_403_probe_interval_minutes": invalid})
		if response.Code != http.StatusBadRequest {
			t.Fatalf("interval %d accepted with %d", invalid, response.Code)
		}
	}
	response := invokeResponseCacheSettingsAdmin(t, h, http.MethodPut, map[string]any{
		"codex_basispoints_403_auto_pause":             false,
		"codex_basispoints_403_probe_interval_minutes": 10080,
	})
	if response.Code != 200 {
		t.Fatalf("PUT: %d %s", response.Code, response.Body.String())
	}
	got := decodeResponseCacheSettingsResponse(t, response)
	runtime := proxy.CurrentRuntimeSettings()
	if got.CodexBasispoints403AutoPause || got.CodexBasispoints403ProbeIntervalMin != 10080 || !runtime.CodexBasispoints403PauseDisabled || runtime.CodexBasispoints403ProbeIntervalMin != 10080 {
		t.Fatalf("PUT/runtime mismatch: %+v", got)
	}
	saved, err := db.GetSystemSettings(context.Background())
	if err != nil || !saved.CodexBasispoints403PauseDisabled || saved.CodexBasispointsProbeMinutes != 10080 {
		t.Fatalf("persisted mismatch: %+v %v", saved, err)
	}
	proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
	proxy.ApplyRuntimeSettingsFromSystem(saved)
	reloaded := decodeResponseCacheSettingsResponse(t, invokeResponseCacheSettingsAdmin(t, h, http.MethodGet, nil))
	if reloaded.CodexBasispoints403AutoPause || reloaded.CodexBasispoints403ProbeIntervalMin != 10080 {
		t.Fatal("reload/GET mismatch")
	}
}
