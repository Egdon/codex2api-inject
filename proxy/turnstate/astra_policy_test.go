package turnstate

import (
	"context"
	"encoding/json"
	"github.com/codex2api/database"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAstraConfigEpochPrivateAndPersisted(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.AstraPolicyEnabled || cfg.AstraRecheckMinutes != 30 {
		t.Fatal("unsafe policy defaults")
	}
	cfg.astraPolicyEpoch = 1234
	cfg.MaxAttempts = 50
	raw, err := EncodeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseConfigJSON(raw)
	if err != nil || parsed.astraPolicyEpoch != 1234 || parsed.MaxAttempts != 50 {
		t.Fatalf("roundtrip %+v %v", parsed, err)
	}
	public, _ := json.Marshal(Publicize(cfg))
	if strings.Contains(string(public), "epoch") {
		t.Fatal("epoch exposed")
	}
}
func TestAstraBatchThresholdEvidence(t *testing.T) {
	for _, tc := range []struct {
		name             string
		attempts, misses int
		result           attemptResult
		outcome, reason  string
	}{
		{"all ordinary", 30, 30, attemptResult{phase: "exhausted"}, "ordinary_miss", "qualified_miss"},
		{"23/30", 30, 23, attemptResult{phase: "exhausted"}, "inconclusive", "insufficient_misses"},
		{"24/30", 30, 24, attemptResult{phase: "exhausted"}, "ordinary_miss", "qualified_miss"},
		{"incomplete", 29, 24, attemptResult{phase: "exhausted"}, "inconclusive", "not_exhausted"},
		{"retry", 29, 24, attemptResult{phase: "retrying"}, "inconclusive", "not_exhausted"},
		{"401", 30, 24, attemptResult{phase: "skipped", status: "401"}, "inconclusive", "interrupted"},
		{"cancel", 30, 24, cancelledResult(), "inconclusive", "interrupted"},
		{"success precedence", 30, 24, attemptResult{phase: "succeeded", new292: true, confirmed: true}, "new_292", "success"},
		{"warning precedence", 30, 24, attemptResult{phase: "succeeded", new292: true}, "new_292", "confirmation_warning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &scheduledCell{attempt: tc.attempts, max: 30, ordinaryMisses: tc.misses, missThresholdPercent: 80}
			outcome, e := classifyPolicyBatch(c, tc.result)
			if outcome != tc.outcome || e.Reason != tc.reason || e.Attempts != tc.attempts || e.MaxAttempts != 30 || e.OrdinaryMisses != tc.misses || e.ThresholdPercent != 80 || e.Exhausted != (tc.result.phase == "exhausted") {
				t.Fatalf("%s %+v", outcome, e)
			}
		})
	}
}

func TestAstraBatchInvalidEvidenceFailsClosed(t *testing.T) {
	for _, c := range []scheduledCell{
		{attempt: 30, max: 30, ordinaryMisses: 30},
		{attempt: 30, max: 30, ordinaryMisses: 30, missThresholdPercent: 101},
		{attempt: 30, max: 30, ordinaryMisses: -1, missThresholdPercent: 80},
		{attempt: 30, max: 30, ordinaryMisses: 31, missThresholdPercent: 80},
	} {
		outcome, _ := classifyPolicyBatch(&c, attemptResult{phase: "exhausted"})
		if outcome != "inconclusive" {
			t.Fatalf("invalid evidence qualified: %+v", c)
		}
	}
}

func TestAstraThresholdConfigAndAdmissionFreeze(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{`{}`, 80}, {`{"max_attempts":30}`, 80},
		{`{"astra_miss_threshold_percent":0}`, 80},
		{`{"astra_miss_threshold_percent":-1}`, 1},
		{`{"astra_miss_threshold_percent":101}`, 100},
		{`{"astra_miss_threshold_percent":75}`, 75},
	} {
		cfg, err := ParseConfigJSON(tc.raw)
		if err != nil || cfg.AstraMissThresholdPercent != tc.want {
			t.Fatalf("%s: %+v %v", tc.raw, cfg, err)
		}
		raw, err := EncodeConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		again, err := ParseConfigJSON(raw)
		if err != nil || again.AstraMissThresholdPercent != tc.want {
			t.Fatalf("roundtrip: %+v %v", again, err)
		}
	}
	old := GetConfig()
	defer SetConfig(old)
	cfg := DefaultConfig()
	cfg.MaxAttempts = 30
	c := &scheduledCell{max: cfg.MaxAttempts, model: astraModel}
	(&Harvester{}).preparePolicyBatch(context.Background(), c, cfg) // disabled and no DB
	cfg.MaxAttempts, cfg.AstraMissThresholdPercent = 50, 100
	SetConfig(cfg)
	c.attempt, c.ordinaryMisses = 30, 24
	outcome, e := classifyPolicyBatch(c, attemptResult{phase: "exhausted"})
	if outcome != "ordinary_miss" || e.ThresholdPercent != 80 || e.MaxAttempts != 30 {
		t.Fatalf("not frozen: %s %+v", outcome, e)
	}
}

func TestAstraPolicySnapshotClonesEvidence(t *testing.T) {
	h := &Harvester{db: &database.DB{}, policyLoaded: time.Now(), policyStates: map[int64]database.AstraPolicyState{
		1: {AccountID: 1, LatestBatch: &database.AstraBatchEvidence{OrdinaryMisses: 24}},
	}}
	s := h.PolicySnapshot(1)
	s.LatestBatch.OrdinaryMisses = 0
	if h.PolicySnapshot(1).LatestBatch.OrdinaryMisses != 24 {
		t.Fatal("snapshot aliases cached evidence")
	}
}

func TestRetryMissesDoNotBackOffButFailuresDo(t *testing.T) {
	if d := (attemptResult{ordinaryMiss: true}).delay(20); d != 0 {
		t.Fatalf("ordinary miss delay %v", d)
	}
	if d := (attemptResult{retryKind: probeNetwork}).delay(20); d != 32*time.Second {
		t.Fatalf("network delay %v", d)
	}
	for _, kind := range []probeFailureKind{probeHTTP, probeOverload} {
		if d := (attemptResult{retryKind: kind}).delay(20); d != 32*time.Second {
			t.Fatal(d)
		}
	}
	if d := boundedRetryAfter(http.Header{"Retry-After": []string{"999999"}}); d != 5*time.Minute {
		t.Fatal(d)
	}
	if d := (attemptResult{retryKind: probeNetwork, retryAfter: time.Minute}).delay(2); d != time.Minute {
		t.Fatal(d)
	}
}
