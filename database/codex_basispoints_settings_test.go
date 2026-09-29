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
