package turnstate

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func schedulerConfig(t *testing.T) Config {
	t.Helper()
	old := GetConfig()
	t.Cleanup(func() { SetConfig(old) })
	cfg := DefaultConfig()
	cfg.Models = []string{"m1", "m2", "m3"}
	cfg.ZooUserPrefix, cfg.ZooPassword = "fixture", "fixture"
	cfg.Concurrency, cfg.AccountConcurrency, cfg.MaxAttempts = 2, 1, 1
	SetConfig(cfg)
	return cfg
}

func awaitSchedulerSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler barrier timed out")
	}
}

func TestSchedulerLimitsDedupCancelAndDetachedSnapshots(t *testing.T) {
	schedulerConfig(t)
	h := NewHarvester(nil, stubStore{accounts: []*auth.Account{testAccount(1), testAccount(2)}}, NewCache())
	started := make(chan struct{}, 16)
	var mu sync.Mutex
	active, peak := 0, 0
	perAccount := map[int64]int{}
	violated := false
	h.probeFn = func(ctx context.Context, cfg Config, acc *auth.Account, model, inject string) (string, http.Header, error) {
		mu.Lock()
		active++
		perAccount[acc.ID()]++
		peak = max(peak, active)
		violated = violated || active > cfg.Concurrency || perAccount[acc.ID()] > cfg.AccountConcurrency
		mu.Unlock()
		started <- struct{}{}
		<-ctx.Done()
		mu.Lock()
		active--
		perAccount[acc.ID()]--
		mu.Unlock()
		return "", nil, ctx.Err()
	}
	job, s, err := h.submit(context.Background(), 0, []int64{1, 2}, "", true, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.CancelJob(); awaitSchedulerSignal(t, s.done) })
	awaitSchedulerSignal(t, started)
	awaitSchedulerSignal(t, started)
	duplicate, err := h.EnqueueSelected([]int64{1, 2}, "", true)
	if err != nil || duplicate.ID != job.ID || duplicate.Total != 6 {
		t.Fatalf("dedup snapshot=%+v err=%v", duplicate, err)
	}
	duplicate.Cells[0].Status = "mutated"
	duplicate.AccountIDs[0] = 99
	snapshot := h.CurrentJob()
	if snapshot.Cells[0].Status == "mutated" || snapshot.AccountIDs[0] == 99 {
		t.Fatal("snapshot aliases internal slices")
	}
	h.CancelJob()
	awaitSchedulerSignal(t, s.done)
	snapshot = h.CurrentJob()
	if snapshot.Status != "cancelled" || snapshot.Done != 6 || snapshot.CancelledCount != 6 || snapshot.Exhausted != 0 || snapshot.FinishedUnix == 0 {
		t.Fatalf("cancel snapshot=%+v", snapshot)
	}
	h.CancelJob()
	if h.CurrentJob().Status != "cancelled" {
		t.Fatal("terminal cancel mutated job")
	}
	mu.Lock()
	defer mu.Unlock()
	if violated || peak != 2 || active != 0 {
		t.Fatalf("limits violated=%v peak=%d active=%d", violated, peak, active)
	}
	h.publishMu.Lock()
	defer h.publishMu.Unlock()
	if len(h.generations) != 0 {
		t.Fatal("terminal generations retained")
	}
}

func TestSchedulerBusyAutoScanMergesWithoutIntervalGate(t *testing.T) {
	cfg := schedulerConfig(t)
	cfg.Models, cfg.AutoHarvest, cfg.IntervalMinutes = []string{"m1"}, true, 1440
	SetConfig(cfg)
	h := NewHarvester(nil, stubStore{accounts: []*auth.Account{testAccount(1), testAccount(2)}}, NewCache())
	started := make(chan struct{}, 8)
	h.probeFn = func(ctx context.Context, cfg Config, acc *auth.Account, model, inject string) (string, http.Header, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", nil, ctx.Err()
	}
	job, s, err := h.submit(context.Background(), 1, nil, "", true, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.CancelJob(); awaitSchedulerSignal(t, s.done) })
	awaitSchedulerSignal(t, started)
	cfg.Models = []string{"m1", "m2"}
	SetConfig(cfg)
	h.scanAuto(context.Background())
	h.scanAuto(context.Background())
	merged := h.CurrentJob()
	if merged.ID != job.ID || merged.Total != 4 || merged.Kind != "mixed" {
		t.Fatalf("busy auto merge=%+v", merged)
	}
	// The newly admitted second account can run while the first remains blocked.
	awaitSchedulerSignal(t, started)
	h.CancelJob()
	awaitSchedulerSignal(t, s.done)
}

func TestSchedulerPickRoundRobinModelsBurstAndRetryDelay(t *testing.T) {
	now := time.Now()
	s := &harvestScheduler{accounts: []int64{1, 2}, byAccount: map[int64][]*scheduledCell{}, activeByAccount: map[int64]int{}}
	a := &scheduledCell{accountID: 1, model: "a", manual: true}
	b := &scheduledCell{accountID: 1, model: "b", manual: true}
	automatic := &scheduledCell{accountID: 2, model: "auto"}
	s.byAccount[1], s.byAccount[2] = []*scheduledCell{a, b}, []*scheduledCell{automatic}
	for i, want := range []*scheduledCell{a, b, a, automatic} {
		if got := s.pick(now, 1); got != want {
			t.Fatalf("dispatch %d got=%p want=%p", i, got, want)
		}
	}
	a.ready, b.ready = now.Add(time.Hour), now.Add(time.Hour)
	if got := s.pick(now, 1); got != automatic {
		t.Fatal("sleeping retries blocked automatic work")
	}
	s.activeByAccount[2] = 1
	if got := s.pick(now, 1); got != nil {
		t.Fatal("per-account slot ignored")
	}
	if retryDelay(1) != time.Second || retryDelay(3) != 4*time.Second || retryDelay(50) != 32*time.Second {
		t.Fatal("retry delay not bounded exponential")
	}
}

func TestSchedulerGenerationProtectsPasteAndClearInMemoryAndDB(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(fmt.Sprintf("clear=%v", clear), func(t *testing.T) {
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "generation.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			h := NewHarvester(db, stubStore{}, NewCache())
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			token := fakeFernet(time.Now().Unix(), 160)
			h.probeFn = func(ctx context.Context, cfg Config, acc *auth.Account, model, inject string) (string, http.Header, error) {
				if inject != "" {
					close(entered)
					<-release
				}
				return token, nil, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { defer close(done); h.harvestCell(ctx, DefaultConfig(), testAccount(1), "m1") }()
			awaitSchedulerSignal(t, entered)
			if clear {
				if err := h.Clear(context.Background(), 1, "m1"); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := h.ManualPaste(context.Background(), 1, "m1", "manual-value"); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			awaitSchedulerSignal(t, done)
			ticket, exists := h.cache.Get(1, "m1")
			rows, err := db.ListTurnStateTickets(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if clear {
				if exists || len(rows) != 0 {
					t.Fatal("stale confirm resurrected cleared ticket")
				}
			} else if !exists || ticket.Token != "manual-value" || len(rows) != 1 || rows[0].Token != "manual-value" {
				t.Fatal("stale confirm overwrote pasted ticket")
			}
		})
	}
}

func TestSchedulerRejectsOversizedAdmissionAndReadmitsTerminalKeys(t *testing.T) {
	cfg := schedulerConfig(t)
	cfg.Models = make([]string, maxHarvestCells+1)
	for i := range cfg.Models {
		cfg.Models[i] = fmt.Sprintf("m%d", i)
	}
	SetConfig(cfg)
	h := NewHarvester(nil, stubStore{accounts: []*auth.Account{testAccount(1)}}, NewCache())
	if _, err := h.Enqueue(1, "", true); err == nil {
		t.Fatal("oversized manual queue accepted")
	}
	if h.CurrentJob() != nil {
		t.Fatal("rejected admission started a job")
	}
	if _, err := h.Enqueue(-1, "", true); err == nil {
		t.Fatal("negative account accepted")
	}
	// Install a bounded synthetic busy round to exercise admission without probes.
	h.job = &Job{ID: "fixture", Status: "running", Total: 2, Done: 1, Cells: []JobCell{{AccountID: 1, Model: "m0", Phase: "exhausted"}}}
	h.scheduler = &harvestScheduler{ctx: context.Background(), cells: map[string]*scheduledCell{}, byAccount: map[int64][]*scheduledCell{}, activeByAccount: map[int64]int{}, wake: make(chan struct{}, 1)}
	job, err := h.Enqueue(1, "m0", true)
	if err != nil || job.Total != 3 || job.Done != 1 || len(job.Cells) != 1 || job.Cells[0].Phase != "queued" {
		t.Fatalf("terminal key readmission: %+v %v", job, err)
	}
}

func TestSchedulerConfirmCancellationDoesNotPublishAnd401CoolsDown(t *testing.T) {
	h := NewHarvester(nil, stubStore{}, NewCache())
	original := CachedTicket{AccountID: 1, Model: "m1", Token: "original", ConfirmWarning: true}
	h.cache.Put(original)
	ctx, cancel := context.WithCancel(context.Background())
	h.probeFn = func(ctx context.Context, cfg Config, acc *auth.Account, model, inject string) (string, http.Header, error) {
		if inject != "" {
			cancel()
			return "", nil, ctx.Err()
		}
		return fakeFernet(time.Now().Unix(), 160), nil, nil
	}
	h.harvestCell(ctx, DefaultConfig(), testAccount(1), "m1")
	if got, _ := h.cache.Get(1, "m1"); got != original {
		t.Fatal("cancelled confirm changed original ticket")
	}
	h.probeFn = func(context.Context, Config, *auth.Account, string, string) (string, http.Header, error) {
		return "", nil, fmt.Errorf("status 401")
	}
	h.harvestCell(context.Background(), DefaultConfig(), testAccount(1), "m1")
	got, _ := h.cache.Get(1, "m1")
	if got.Token != original.Token || !got.ConfirmWarning || got.CooldownUntil <= time.Now().Unix() || got.Exhausted {
		t.Fatal("401 lost original ticket or lacks cooldown")
	}
}
