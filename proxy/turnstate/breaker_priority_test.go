package turnstate

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Pure state fixtures: no database, browser, or live proxy calls.
func TestBreakerThresholdWindowAndHalfOpenLease(t *testing.T) {
	now := time.Now()
	var b harvestBreaker
	for i := 0; i < 9; i++ {
		b.observe(now, int64(i%3+1), i < 8, 0)
	}
	if b.snapshot(now).State != "closed" {
		t.Fatal("opened before minimum sample count")
	}
	b.observe(now, 3, false, 0)
	if s := b.snapshot(now); s.State != "open" || s.Samples != 10 || s.Failures != 8 || s.Accounts != 3 {
		t.Fatalf("threshold snapshot: %+v", s)
	}
	if b.available(now) {
		t.Fatal("open circuit admitted work")
	}
	trialAt := now.Add(30 * time.Second)
	if !b.available(trialAt) {
		t.Fatal("expiry did not admit a trial")
	}
	lease := b.reserve()
	if lease == 0 || b.available(trialAt) {
		t.Fatal("half-open lease not exclusive")
	}
	b.observe(trialAt, 1, false, 0) // old in-flight success must not resolve trial
	if b.state != "half_open" || b.observed {
		t.Fatal("old unit resolved half-open trial")
	}
	b.observe(trialAt, 2, true, lease)
	b.release(lease)
	if b.state != "open" || b.pause != 60*time.Second {
		t.Fatal("failed trial did not escalate")
	}
	trialAt = trialAt.Add(60 * time.Second)
	if !b.available(trialAt) {
		t.Fatal("second trial blocked")
	}
	lease = b.reserve()
	b.observe(trialAt, 3, false, lease)
	b.release(lease)
	if s := b.snapshot(trialAt); s.State != "closed" || s.Samples != 0 {
		t.Fatalf("successful trial: %+v", s)
	}
	for i := 0; i < 400; i++ {
		b.observe(trialAt, 1, true, 0)
	}
	if s := b.snapshot(trialAt); s.State != "closed" || s.Samples != 256 {
		t.Fatalf("single-account cap: %+v", s)
	}
	if s := b.snapshot(trialAt.Add(breakerWindow)); s.Samples != 0 {
		t.Fatalf("expired samples retained: %+v", s)
	}
}

func TestBreakerExclusionsAndInconclusiveRelease(t *testing.T) {
	for _, tc := range []struct {
		name          string
		hdr           http.Header
		err           error
		observe, fail bool
	}{
		{"312 ordinary response", nil, nil, true, false},
		{"HTTP 503", http.Header{}, &probeFailure{kind: probeHTTP, status: 503}, true, false},
		{"transport", nil, &probeFailure{kind: probeNetwork}, true, true},
		{"401", http.Header{}, fmt.Errorf("status 401"), false, false},
		{"cancelled", nil, context.Canceled, false, false},
		{"local protocol error", nil, fmt.Errorf("bad local proxy URL"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observe, failed := networkOutcome(context.Background(), tc.hdr, tc.err)
			if observe != tc.observe || failed != tc.fail {
				t.Fatalf("outcome %v/%v", observe, failed)
			}
		})
	}
	b := harvestBreaker{state: "half_open", pause: 30 * time.Second}
	lease := b.reserve()
	b.release(lease)
	if b.reserved || b.state != "open" || !b.retryAt.After(time.Now()) {
		t.Fatal("inconclusive lease hung or hot-looped")
	}
}

func TestSchedulerPriorityAcrossAccountsAgingAndRR(t *testing.T) {
	now := time.Now()
	a := &scheduledCell{accountID: 1, model: "recovery", waitingSince: now}
	b := &scheduledCell{accountID: 2, model: "renew", waitingSince: now}
	c := &scheduledCell{accountID: 3, model: "urgent", waitingSince: now}
	s := &harvestScheduler{accounts: []int64{1, 2, 3}, byAccount: map[int64][]*scheduledCell{1: {a}, 2: {b}, 3: {c}}, activeByAccount: map[int64]int{}, priority: func(c *scheduledCell, _ time.Time) int {
		switch c.model {
		case "recovery":
			return 2
		case "renew":
			return 1
		default:
			return 0
		}
	}}
	if got := s.pick(now, 1); got != c {
		t.Fatal("RR bypassed globally urgent tier")
	}
	c.active = true
	if got := s.pick(now, 1); got != b {
		t.Fatal("recovery bypassed ordinary renewal")
	}
	c.active = false
	a.waitingSince = now.Add(-2 * time.Minute)
	s.cursor = 0
	if got := s.pick(now, 1); got != a {
		t.Fatal("aging did not promote recovery")
	}
	if got := s.pick(now, 1); got != c {
		t.Fatal("equal-tier RR fairness lost")
	}
	older := &scheduledCell{accountID: 3, model: "urgent", waitingSince: now.Add(-time.Minute)}
	s.byAccount[3] = append(s.byAccount[3], older)
	s.cursor = 2
	if got := s.pick(now, 1); got != older {
		t.Fatal("equal-tier oldest model did not win")
	}
}
