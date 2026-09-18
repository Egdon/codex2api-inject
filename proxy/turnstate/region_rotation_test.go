package turnstate

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestRegionStageBoundaries(t *testing.T) {
	for _, tc := range []struct {
		budget int
		counts [4]int
	}{
		{20, [4]int{5, 5, 5, 5}}, {22, [4]int{5, 5, 5, 7}}, {6, [4]int{1, 1, 1, 3}},
		{3, [4]int{1, 1, 1, 0}}, {1, [4]int{1, 0, 0, 0}}, {50, [4]int{12, 12, 12, 14}},
	} {
		var got [4]int
		for n := 1; n <= tc.budget; n++ {
			got[batchRegionStage(n, tc.budget)]++
		}
		if got != tc.counts {
			t.Fatalf("budget %d: %v want %v", tc.budget, got, tc.counts)
		}
	}
}

func TestRegionPlanDefaultsAndDistinctEurope(t *testing.T) {
	cfg := NormalizeConfig(Config{ZooRegion: "jp"})
	if cfg.ZooRegionMode != regionModeFixed {
		t.Fatal("legacy mode changed")
	}
	if plan := newBatchRegionPlan(cfg); plan != [4]string{"JP", "JP", "JP", "JP"} {
		t.Fatal(plan)
	}
	cfg.ZooRegionMode = regionModeRotation
	for _, first := range []string{"DE", "FR", "JP", ""} {
		cfg.ZooRegion = first
		for n := 0; n < 20; n++ {
			plan := newBatchRegionPlan(cfg)
			for i := 0; i < 3; i++ {
				if !slices.Contains([]string{"DE", "FR", "GB", "NL", "SE", "FI", "CH", "IE"}, plan[i]) {
					t.Fatal(plan)
				}
				for j := 0; j < i; j++ {
					if plan[i] == plan[j] {
						t.Fatal("duplicate Europe", plan)
					}
				}
			}
			if first == "DE" || first == "FR" {
				if plan[0] != first {
					t.Fatal(plan)
				}
			}
			if !slices.Contains([]string{"JP", "KR", "TW"}, plan[3]) {
				t.Fatal(plan)
			}
		}
	}
}

func TestAcquisitionAndConfirmationUseFrozenRegion(t *testing.T) {
	h := NewHarvester(nil, stubStore{}, NewCache())
	acc := testAccount(9)
	g, version := h.retainGeneration(acc.ID(), "gpt-6-astra")
	task := scheduledCell{key: ticketKey(acc.ID(), "gpt-6-astra"), accountID: acc.ID(), model: "gpt-6-astra", max: 20, attempt: 6, generation: g, version: version, regionPlan: [4]string{"DE", "FR", "NL", "TW"}}
	defer h.releaseGeneration(task.key, g)
	var regions []string
	token := fakeFernet(time.Now().Unix(), 160)
	h.probeFn = func(ctx context.Context, cfg Config, acc *auth.Account, model, inject string) (string, http.Header, error) {
		regions = append(regions, cfg.ZooRegion)
		return token, http.Header{}, nil
	}
	cfg := DefaultConfig()
	cfg.ZooRegion = "GB"
	result := h.attempt(context.Background(), cfg, acc, task, func() {})
	if result.phase != "succeeded" || !slices.Equal(regions, []string{"FR", "FR"}) {
		t.Fatalf("%+v %v", result, regions)
	}
	if cfg.ZooRegion != "GB" {
		t.Fatal("caller configuration modified")
	}
}
