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

func weightedSchedulerFixture(now time.Time, weights ...int) *harvestScheduler {
	s := &harvestScheduler{byAccount: map[int64][]*scheduledCell{}, activeByAccount: map[int64]int{}}
	for i := range weights {
		id := int64(i + 1)
		s.accounts = append(s.accounts, id)
		s.byAccount[id] = []*scheduledCell{{accountID: id, model: "m1", waitingSince: now}}
	}
	s.weight = func(id int64, _ Config) (string, int) { return "fixture", weights[id-1] }
	return s
}

func TestSchedulerSmoothWeightsAndEqualWeightRR(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, weights := range [][]int{{3, 2, 1}, {1, 1, 1}, {10, 10, 10}} {
		s := weightedSchedulerFixture(now, weights...)
		counts := [3]int{}
		for i := 0; i < 600; i++ {
			got := s.pick(now, 1)
			counts[got.accountID-1]++
			if weights[0] == weights[1] && got.accountID != int64(i%3+1) {
				t.Fatalf("equal weights lost RR at %d: %d", i, got.accountID)
			}
		}
		total := weights[0] + weights[1] + weights[2]
		for i, count := range counts {
			if want := 600 * weights[i] / total; count != want {
				t.Fatalf("weights %v counts %v: account %d want %d", weights, counts, i+1, want)
			}
		}
	}
}

func TestSchedulerWeightsIndependentOfModelCount(t *testing.T) {
	now := time.Unix(1000, 0)
	s := weightedSchedulerFixture(now, 3, 2, 1)
	for i := 0; i < 20; i++ {
		s.byAccount[3] = append(s.byAccount[3], &scheduledCell{accountID: 3, model: fmt.Sprintf("extra-%d", i), waitingSince: now})
	}
	counts := [3]int{}
	for i := 0; i < 600; i++ {
		counts[s.pick(now, 1).accountID-1]++
	}
	if counts != [3]int{300, 200, 100} {
		t.Fatalf("model count changed shares: %v", counts)
	}
}

func TestSchedulerWeightsRespectUrgencyAndModelOrder(t *testing.T) {
	now := time.Unix(1000, 0)
	s := weightedSchedulerFixture(now, 10, 1)
	s.priority = func(c *scheduledCell, _ time.Time) int {
		if c.accountID == 1 {
			return 2
		}
		return 0
	}
	for i := 0; i < 12; i++ {
		if got := s.pick(now, 1); got.accountID != 2 {
			t.Fatal("weight bypassed urgency")
		}
	}
	// Ageing still promotes a recovery cell to tier zero.
	s.byAccount[1][0].waitingSince = now.Add(-2 * time.Minute)
	if got := s.pick(now, 1); got.accountID != 1 {
		t.Fatal("ageing did not promote recovery")
	}
	s = weightedSchedulerFixture(now, 3)
	a := s.byAccount[1][0]
	b := &scheduledCell{accountID: 1, model: "m2", waitingSince: now}
	s.byAccount[1] = append(s.byAccount[1], b)
	for i, want := range []*scheduledCell{a, b, a, b} {
		if got := s.pick(now, 1); got != want {
			t.Fatalf("model RR changed at %d", i)
		}
	}
	b.waitingSince = now.Add(-time.Second)
	if s.pick(now, 1) != b {
		t.Fatal("oldest model not selected")
	}
}

func TestSchedulerWeightsLeaveManualRRAndBurstUnchanged(t *testing.T) {
	now := time.Unix(1000, 0)
	s := weightedSchedulerFixture(now, 10, 1, 1)
	s.byAccount[1][0].manual, s.byAccount[2][0].manual = true, true
	for i, want := range []int64{1, 2, 1, 3, 1, 2, 1, 3} {
		if got := s.pick(now, 1); got.accountID != want {
			t.Fatalf("dispatch %d got %d want %d", i, got.accountID, want)
		}
	}
	if s.autoDispatches != 2 {
		t.Fatal("manual dispatch advanced overdue quota")
	}
	if len(s.credits) != 1 {
		t.Fatal("manual accounts accrued credit")
	}
}

func TestSchedulerOverdueQuotaUsesIndependentUnweightedRR(t *testing.T) {
	now := time.Unix(1000, 0)
	s := weightedSchedulerFixture(now, 10, 1, 1, 1)
	for _, id := range []int64{2, 3, 4} {
		s.byAccount[id][0].waitingSince = now.Add(-2 * time.Minute)
	}
	// Account four is overdue but sleeping, so it cannot take quota slots.
	s.byAccount[4][0].ready = now.Add(time.Hour)
	for i := 1; i <= 24; i++ {
		got := s.pick(now, 1)
		if got.accountID == 4 {
			t.Fatal("sleeping overdue cell was dispatched")
		}
		if i%4 == 0 {
			want := int64(2 + (i/4-1)%2)
			if got.accountID != want {
				t.Fatalf("overdue slot %d got %d want %d", i, got.accountID, want)
			}
		}
	}
	// Once runnable, the next RR quota goes to account four, regardless of weight.
	s.byAccount[4][0].ready = time.Time{}
	for i := 0; i < 3; i++ {
		s.pick(now, 1)
	}
	before := make(map[int64]int)
	for id, credit := range s.credits {
		before[id] = credit.value
	}
	if got := s.pick(now, 1); got.accountID != 4 {
		t.Fatal("newly runnable overdue account missed quota")
	}
	for id, credit := range s.credits {
		if credit.value != before[id] {
			t.Fatal("quota slot changed weighted credit")
		}
	}
}

func TestSchedulerCreditResetsAndBounds(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, mode := range []string{"active", "limit", "backoff", "terminal", "manual", "removed", "tier"} {
		t.Run(mode, func(t *testing.T) {
			s := weightedSchedulerFixture(now, 3, 2, 1)
			s.pick(now, 1)
			c := s.byAccount[1][0]
			switch mode {
			case "active":
				c.active = true
			case "limit":
				s.activeByAccount[1] = 1
			case "backoff":
				c.ready = now.Add(time.Second)
			case "terminal":
				c.terminal = true
			case "manual":
				c.manual = true
			case "removed":
				s.accounts = s.accounts[1:]
				delete(s.byAccount, 1)
			case "tier":
				s.priority = func(c *scheduledCell, _ time.Time) int {
					if c.accountID == 1 {
						return 1
					}
					return 0
				}
			}
			s.pick(now, 1)
			if _, ok := s.credits[1]; ok {
				t.Fatal("ineligible account retained credit")
			}
			if len(s.credits) > len(s.accounts) {
				t.Fatal("credit state grew beyond accounts")
			}
		})
	}
	s := weightedSchedulerFixture(now, 3, 2, 1)
	s.pick(now, 1)
	s.priority = func(*scheduledCell, time.Time) int { return 1 }
	candidates, tier := s.candidates(now, 1, false)
	s.syncCredits(candidates, tier)
	for _, credit := range s.credits {
		if credit.value != 0 {
			t.Fatal("tier change retained credit")
		}
	}
	for id, credit := range s.credits {
		credit.value = 1 << 30
		s.credits[id] = credit
	}
	s.syncCredits(candidates, tier)
	for _, credit := range s.credits {
		if credit.value != 30 {
			t.Fatal("credit not bounded")
		}
	}
}

func TestSchedulerCreditResetsOnPlanAndWeightConfigChanges(t *testing.T) {
	now := time.Unix(1000, 0)
	old := GetConfig()
	t.Cleanup(func() { SetConfig(old) })
	cfg := DefaultConfig()
	SetConfig(cfg)
	acc := testAccount(1)
	acc.PlanType = "pro"
	h := NewHarvester(nil, stubStore{accounts: []*auth.Account{acc}}, NewCache())
	s := weightedSchedulerFixture(now, 3)
	s.weight = h.accountPlanWeight
	s.pick(now, 1)
	credit := s.credits[1]
	credit.value = 7
	s.credits[1] = credit
	acc.PlanType = "pro-lite"
	candidates, tier := s.candidates(now, 1, false)
	s.syncCredits(candidates, tier)
	if got := s.credits[1]; got.plan != "prolite" || got.weight != 2 || got.value != 0 {
		t.Fatalf("plan change retained stale credit: %+v", got)
	}
	credit = s.credits[1]
	credit.value = 7
	s.credits[1] = credit
	// Even changing an unused plan's weight invalidates the weight config epoch.
	cfg.PlanWeightPlus = 4
	SetConfig(cfg)
	s.syncCredits(candidates, tier)
	if s.credits[1].value != 0 {
		t.Fatal("config change retained credit")
	}
	// A plan identity change also resets credit when both weights are equal.
	credit = s.credits[1]
	credit.value = 7
	s.credits[1] = credit
	acc.PlanType = "enterprise"
	s.syncCredits(candidates, tier)
	credit = s.credits[1]
	credit.value = 7
	s.credits[1] = credit
	acc.PlanType = "unknown"
	s.syncCredits(candidates, tier)
	if got := s.credits[1]; got.weight != 1 || got.value != 0 {
		t.Fatal("equal-weight plan change retained credit")
	}
}

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
