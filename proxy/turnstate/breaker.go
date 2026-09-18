package turnstate

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/codex2api/auth"
)

const (
	breakerWindow     = 60 * time.Second
	breakerMinSamples = 10
	breakerMaxSamples = 256
)

type BreakerSnapshot struct {
	State    string `json:"state"`
	RetryAt  int64  `json:"retry_at"`
	Samples  int    `json:"samples"`
	Failures int    `json:"failures"`
	Accounts int    `json:"accounts"`
}

type networkObservation struct {
	at        time.Time
	accountID int64
	failed    bool
}

// Protected by Harvester.mu. A lease covers one acquisition+confirmation unit;
// already dispatched units remain free to finish when the circuit opens.
type harvestBreaker struct {
	observations []networkObservation
	state        string
	retryAt      time.Time
	pause        time.Duration
	epoch        uint64
	reserved     bool
	observed     bool
}

func (b *harvestBreaker) refresh(now time.Time) {
	if b.state == "" {
		b.state = "closed"
	}
	i := 0
	for i < len(b.observations) && now.Sub(b.observations[i].at) >= breakerWindow {
		i++
	}
	if i > 0 {
		b.observations = append(b.observations[:0], b.observations[i:]...)
	}
	if b.state == "open" && !now.Before(b.retryAt) {
		b.state = "half_open"
	}
}

func (b *harvestBreaker) snapshot(now time.Time) BreakerSnapshot {
	b.refresh(now)
	s := BreakerSnapshot{State: b.state, Samples: len(b.observations)}
	if b.state != "closed" {
		s.RetryAt = b.retryAt.Unix()
	}
	accounts := make(map[int64]struct{})
	for _, o := range b.observations {
		accounts[o.accountID] = struct{}{}
		if o.failed {
			s.Failures++
		}
	}
	s.Accounts = len(accounts)
	return s
}

func (h *Harvester) BreakerSnapshot() BreakerSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.breaker.snapshot(time.Now())
}

func (b *harvestBreaker) available(now time.Time) bool {
	b.refresh(now)
	return b.state == "closed" || b.state == "half_open" && !b.reserved
}

func (b *harvestBreaker) reserve() uint64 {
	if b.state != "half_open" {
		return 0
	}
	b.epoch++
	b.reserved, b.observed = true, false
	return b.epoch
}

func (b *harvestBreaker) open(now time.Time) {
	if b.pause == 0 {
		b.pause = 30 * time.Second
	} else {
		b.pause = min(2*b.pause, 300*time.Second)
	}
	b.state, b.retryAt = "open", now.Add(b.pause)
	b.reserved, b.observed = false, false
}

func (b *harvestBreaker) observe(now time.Time, accountID int64, failed bool, lease uint64) {
	b.refresh(now)
	if len(b.observations) == breakerMaxSamples {
		b.observations = append(b.observations[:0], b.observations[1:]...)
	}
	b.observations = append(b.observations, networkObservation{now, accountID, failed})
	if b.state == "half_open" {
		if lease != 0 && b.reserved && lease == b.epoch {
			b.observed = true
			if failed {
				b.open(now)
			}
		}
		return
	}
	if b.state != "closed" {
		return
	}
	s := b.snapshot(now)
	if s.Samples >= breakerMinSamples && s.Accounts >= 3 && s.Failures*100 >= s.Samples*80 {
		b.open(now)
	}
}

func (b *harvestBreaker) release(lease uint64) {
	if lease == 0 || b.state != "half_open" || !b.reserved || lease != b.epoch {
		return
	}
	b.reserved = false
	if b.observed {
		b.state, b.retryAt, b.pause = "closed", time.Time{}, 0
		b.observations = b.observations[:0]
	} else {
		// An excluded (401/cancelled/stale) trial is inconclusive, not a
		// success. Release its lease, but avoid immediately hot-looping.
		b.state, b.retryAt = "open", time.Now().Add(30*time.Second)
	}
	b.observed = false
}

// A real HTTP response (even a 4xx/5xx or protocol error) proves network
// reachability. Only transport failures count against the network circuit.
func networkOutcome(ctx context.Context, hdr http.Header, err error) (observe, failed bool) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || err != nil && isUnauthorized(err) {
		return false, false
	}
	if hdr != nil || err == nil {
		return true, false
	}
	kind, _ := classifyRetry(err, hdr)
	if kind == probeNetwork {
		return true, true
	}
	if kind == probeHTTP || kind == probeOverload {
		return true, false
	}
	return false, false
}

func (h *Harvester) observedProbe(ctx context.Context, cfg Config, acc *auth.Account, task scheduledCell, inject string) (string, http.Header, error) {
	if inject == "" {
		h.mu.Lock()
		if h.scheduler != nil {
			if c := h.scheduler.cells[task.key]; c != nil && c.generation == task.generation {
				c.attempt++
				h.job.Cells[c.index].Attempt = c.attempt
				h.job.Cells[c.index].Region = cfg.ZooRegion
			}
		}
		h.mu.Unlock()
	}
	token, hdr, err := h.probe(ctx, cfg, acc, task.model, inject)
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.generationCurrent(task.key, task.generation, task.version) {
		return token, hdr, err
	}
	if observe, failed := networkOutcome(ctx, hdr, err); observe {
		h.breaker.observe(time.Now(), task.accountID, failed, task.breakerLease)
		if h.scheduler != nil {
			h.signalLocked(h.scheduler)
		}
	}
	return token, hdr, err
}
