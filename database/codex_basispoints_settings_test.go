package database

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCodexBasispointsSettingRoundTrip(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	settings := &SystemSettings{CodexBasispointsEnabled: true, CodexBasispointsModels: " GPT-6-Astra, gpt-5.6-sol ,gpt-6-astra"}
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings(true): %v", err)
	}
	got, err := db.GetSystemSettings(ctx)
	if err != nil || got == nil || !got.CodexBasispointsEnabled || got.CodexBasispointsModels != "gpt-6-astra,gpt-5.6-sol" {
		t.Fatalf("GetSystemSettings after true = %+v, %v", got, err)
	}
	settings.CodexBasispointsEnabled = false
	settings.CodexBasispointsModels = ""
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings(false): %v", err)
	}
	got, err = db.GetSystemSettings(ctx)
	if err != nil || got == nil || got.CodexBasispointsEnabled || got.CodexBasispointsModels != "" {
		t.Fatalf("GetSystemSettings after false = %+v, %v", got, err)
	}
}

func TestParseCodexBasispointsModels(t *testing.T) {
	for raw, want := range map[string][]string{
		"":                                      nil,
		" , ;":                                  nil,
		"gpt-6-astra":                           {"gpt-6-astra"},
		"GPT-6-Astra\ngpt-5.6-sol; gpt-5.6-sol": {"gpt-6-astra", "gpt-5.6-sol"},
	} {
		if got := ParseCodexBasispointsModels(raw); !reflect.DeepEqual(got, want) {
			t.Fatalf("ParseCodexBasispointsModels(%q) = %#v, want %#v", raw, got, want)
		}
	}
}

func TestCodexBasispointsRouteHealthSettingsRoundTrip(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	// Zero values written by bootstrap map to the defaults.
	if err := db.UpdateSystemSettings(ctx, &SystemSettings{}); err != nil {
		t.Fatalf("UpdateSystemSettings(zero): %v", err)
	}
	got, err := db.GetSystemSettings(ctx)
	if err != nil || got == nil || got.CodexBasispointsProbeMinutes != DefaultCodexBasispoints403ProbeIntervalMinutes || got.CodexBasispoints429CooldownSeconds != DefaultCodexBasispoints429CooldownSeconds {
		t.Fatalf("zero-value settings = %+v, %v", got, err)
	}
	got.CodexBasispoints403PauseDisabled = true
	got.CodexBasispointsProbeMinutes = 30
	got.CodexBasispoints429CooldownSeconds = 600
	if err := db.UpdateSystemSettings(ctx, got); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	again, err := db.GetSystemSettings(ctx)
	if err != nil || again == nil || !again.CodexBasispoints403PauseDisabled || again.CodexBasispointsProbeMinutes != 30 || again.CodexBasispoints429CooldownSeconds != 600 {
		t.Fatalf("round trip = %+v, %v", again, err)
	}
}

func TestNormalizeCodexBasispoints429CooldownSeconds(t *testing.T) {
	for seconds, want := range map[int]int{0: 5, -3: 5, 1: 1, 5: 5, 600: 600, 601: 5} {
		if got := NormalizeCodexBasispoints429CooldownSeconds(seconds); got != want {
			t.Fatalf("NormalizeCodexBasispoints429CooldownSeconds(%d) = %d, want %d", seconds, got, want)
		}
	}
}

func TestCodexBasispointsCacheWriteAsInputRoundTrip(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	// Zero-value settings keep the upstream counters.
	if err := db.UpdateSystemSettings(ctx, &SystemSettings{}); err != nil {
		t.Fatalf("UpdateSystemSettings(zero): %v", err)
	}
	got, err := db.GetSystemSettings(ctx)
	if err != nil || got == nil || got.CodexBasispointsCacheWriteAsInput {
		t.Fatalf("zero-value settings = %+v, %v", got, err)
	}
	for _, asInput := range []bool{true, false} {
		got.CodexBasispointsCacheWriteAsInput = asInput
		if err := db.UpdateSystemSettings(ctx, got); err != nil {
			t.Fatalf("UpdateSystemSettings(%t): %v", asInput, err)
		}
		again, err := db.GetSystemSettings(ctx)
		if err != nil || again == nil || again.CodexBasispointsCacheWriteAsInput != asInput {
			t.Fatalf("round trip %t = %+v, %v", asInput, again, err)
		}
		got = again
	}
}
