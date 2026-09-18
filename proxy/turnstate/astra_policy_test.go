package turnstate

import (
	"encoding/json"
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
