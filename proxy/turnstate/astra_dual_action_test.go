package turnstate

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestAstraDualActionConfigDefaultsAndExplicitZero(t *testing.T) {
	for _, raw := range []string{`{}`, `{"astra_policy_enabled":true}`, `{"astra_failure_priority":0}`} {
		cfg, err := ParseConfigJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		want := int64(-1)
		if raw == `{"astra_failure_priority":0}` {
			want = 0
		}
		if cfg.AstraFailurePriority == nil || *cfg.AstraFailurePriority != want || cfg.AstraRecoveryPriority != 0 || cfg.AstraGroupFailureBatches != 2 || cfg.AstraPriorityFailureBatches != 1 || cfg.AstraPriorityPolicyEnabled {
			t.Fatalf("unexpected defaults for %s: %+v", raw, cfg)
		}
		encoded, err := EncodeConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		again, err := ParseConfigJSON(encoded)
		if err != nil || *again.AstraFailurePriority != want {
			t.Fatalf("priority lost in roundtrip: %s %v", encoded, err)
		}
		public, _ := json.Marshal(Publicize(cfg))
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(public, &fields); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"astra_group_failure_batches", "astra_priority_policy_enabled", "astra_priority_failure_batches", "astra_failure_priority", "astra_recovery_priority"} {
			if _, ok := fields[key]; !ok {
				t.Fatalf("missing public field %s", key)
			}
		}
	}
	priority := int64(-101)
	cfg := NormalizeConfig(Config{AstraGroupFailureBatches: 99, AstraPriorityFailureBatches: -1, AstraFailurePriority: &priority, AstraRecoveryPriority: 101})
	if cfg.AstraGroupFailureBatches != 50 || cfg.AstraPriorityFailureBatches != 1 || *cfg.AstraFailurePriority != -100 || cfg.AstraRecoveryPriority != 100 {
		t.Fatal("normalization did not bound fields")
	}
}

func TestAstraFailurePrioritySnapshotIsolation(t *testing.T) {
	old := GetConfig()
	defer SetConfig(old)
	cfg := DefaultConfig()
	SetConfig(cfg)
	*cfg.AstraFailurePriority = 77
	first := GetConfig()
	if *first.AstraFailurePriority != -1 {
		t.Fatal("SetConfig retained caller pointer")
	}
	*first.AstraFailurePriority = 88
	public := Publicize(GetConfig())
	*public.AstraFailurePriority = 99
	if *GetConfig().AstraFailurePriority != -1 {
		t.Fatal("GetConfig or Publicize exposed atomic snapshot pointer")
	}
}

func TestAstraDualActionSettingsFence(t *testing.T) {
	before := DefaultConfig()
	for name, change := range map[string]func(*Config){
		"group enabled":         func(c *Config) { c.AstraPolicyEnabled = true },
		"priority enabled":      func(c *Config) { c.AstraPriorityPolicyEnabled = true },
		"group batches":         func(c *Config) { c.AstraGroupFailureBatches = 3 },
		"priority batches":      func(c *Config) { c.AstraPriorityFailureBatches = 2 },
		"failure group":         func(c *Config) { c.AstraFailureGroupID = 1 },
		"recovery group":        func(c *Config) { c.AstraRecoveryGroupID = 2 },
		"failure priority zero": func(c *Config) { *c.AstraFailurePriority = 0 },
		"recovery priority":     func(c *Config) { c.AstraRecoveryPriority = 1 },
		"miss percentage":       func(c *Config) { c.AstraMissThresholdPercent = 90 },
	} {
		t.Run(name, func(t *testing.T) {
			after := NormalizeConfig(before)
			change(&after)
			if !astraPolicySettingsChanged(before, after) || !astraPolicySettingsChanged(after, before) {
				t.Fatal("action setting change did not fence both directions")
			}
		})
	}
	after := NormalizeConfig(before)
	after.AstraRecheckMinutes++
	if astraPolicySettingsChanged(before, after) {
		t.Fatal("recheck interval should not reset shared evidence")
	}
}

func TestAstraPriorityOnlyKeepsAllModelsAndOwnershipSnapshot(t *testing.T) {
	old := GetConfig()
	defer SetConfig(old)
	cfg := DefaultConfig()
	cfg.AstraPriorityPolicyEnabled = true
	cfg.astraPolicyEpoch = 2
	SetConfig(cfg)
	h := &Harvester{db: &database.DB{}, policyLoaded: time.Now(), policyStates: map[int64]database.AstraPolicyState{
		1: {AccountID: 1, Epoch: 1, ConsecutiveFailures: 9, Demoted: true, GroupTriggered: true, PriorityDemoted: true, PriorityTriggered: true, NextRecoveryAt: time.Now().Add(time.Hour).Unix()},
	}}
	for _, model := range DefaultModels {
		if !h.automaticPolicyAllowed(1, model, cfg) {
			t.Fatalf("priority-only policy restricted %s", model)
		}
	}
	s := h.PolicySnapshot(1)
	if !s.Enabled || s.ConsecutiveFailures != 0 || !s.Demoted || !s.GroupTriggered || !s.PriorityDemoted || !s.PriorityTriggered {
		t.Fatalf("epoch reset erased ownership/latches: %+v", s)
	}
	cfg.AstraPolicyEnabled = true
	SetConfig(cfg)
	if h.automaticPolicyAllowed(1, astraModel, cfg) || h.automaticPolicyAllowed(1, "gpt-5.6-sol", cfg) {
		t.Fatal("group ownership must retain low-frequency Astra-only admission")
	}
}

func TestAstraSchedulerPrioritySafeParsing(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  int64
	}{
		{nil, 0}, {"", 0}, {" 0 ", 0}, {"-12", -12}, {float64(7), 7}, {float64(1000000), 100}, {int64(-9), -9},
		{"1.5", 0}, {float64(1.5), 0}, {"9223372036854775808", 0},
		{"-101", -100}, {"101", 100}, {map[string]any{"secret": "not-a-priority"}, 0},
	} {
		if got := astraSchedulerPriority(&database.AccountRow{Credentials: map[string]interface{}{"scheduler_priority": tc.value}}); got != tc.want {
			t.Fatalf("priority %v: got %d want %d", tc.value, got, tc.want)
		}
	}
}

type astraPriorityStore struct {
	stubStore
	priorities []int64
	afterApply func()
}

func (s *astraPriorityStore) ApplyAccountSchedulerPriority(id int64, priority *int64) bool {
	s.priorities = append(s.priorities, *priority)
	s.FindByID(id).SetSchedulerPriority(*priority)
	if s.afterApply != nil {
		after := s.afterApply
		s.afterApply = nil
		after()
	}
	return true
}

// Offline source fixture: it never starts the harvester or makes a probe.
func TestAstraPriorityOnlyOutcomeRefreshAndManualRace(t *testing.T) {
	old := GetConfig()
	defer SetConfig(old)
	SetConfig(DefaultConfig())
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "astra-runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := db.InsertAccount(ctx, "astra-runtime", "fixture-refresh", "")
	if err != nil {
		t.Fatal(err)
	}
	store := &astraPriorityStore{stubStore: stubStore{accounts: []*auth.Account{testAccount(id)}}}
	h := NewHarvester(db, store, NewCache())
	cfg := DefaultConfig()
	cfg.AstraPriorityPolicyEnabled = true
	cfg, err = h.SaveConfig(ctx, cfg, true, true)
	if err != nil {
		t.Fatalf("priority-only requires no groups: %v", err)
	}
	if cfg.astraPolicyEpoch == 0 {
		t.Fatal("priority admission missing epoch")
	}
	key := ticketKey(id, astraModel)
	generation, version := h.retainGeneration(id, astraModel)
	cell := &scheduledCell{key: key, accountID: id, model: astraModel, generation: generation, version: version, max: 1}
	h.preparePolicyBatch(ctx, cell, cfg)
	if cell.policyEpoch == 0 {
		t.Fatal("priority-only batch did not capture expectation")
	}
	cell.attempt, cell.ordinaryMisses = 1, 1
	store.afterApply = func() {
		if err := db.UpdateCredentials(ctx, id, map[string]interface{}{"scheduler_priority": int64(9)}); err != nil {
			t.Fatal(err)
		}
	}
	h.mu.Lock()
	h.finishPolicyBatch(ctx, cell, attemptResult{phase: "exhausted"})
	h.mu.Unlock()
	if len(store.priorities) != 2 || store.priorities[0] != -1 || store.priorities[1] != 9 || store.FindByID(id).GetSchedulerPriority() != 9 {
		t.Fatalf("fresh DB reconciliation lost manual change: %v", store.priorities)
	}
	groups, err := db.GetAccountGroupIDs(ctx, id)
	if err != nil || len(groups) != 0 {
		t.Fatalf("priority-only action changed groups: %v %v", groups, err)
	}
	before := cfg.astraPolicyEpoch
	cfg.AstraPriorityFailureBatches = 3
	cfg, err = h.SaveConfig(ctx, cfg, true, true)
	if err != nil || cfg.astraPolicyEpoch <= before {
		t.Fatalf("settings did not advance epoch: %v", err)
	}
	cfg.AstraPriorityPolicyEnabled = false
	before = cfg.astraPolicyEpoch
	cfg, err = h.SaveConfig(ctx, cfg, true, true)
	if err != nil || cfg.astraPolicyEpoch <= before {
		t.Fatalf("disable did not fence old outcomes: %v", err)
	}
}
