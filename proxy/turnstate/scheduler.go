package turnstate

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

const (
	maxHarvestCells  = 4096
	manualBurstLimit = 3
)

// A scheduler retains only active dedup entries; bounded latest-cell history is
// independent from monotonically increasing batch counters.
// All scheduler fields and public job fields are protected by Harvester.mu.
type harvestScheduler struct {
	ctx             context.Context
	cancel          context.CancelFunc
	done            chan struct{}
	wake            chan struct{}
	cells           map[string]*scheduledCell
	accounts        []int64
	byAccount       map[int64][]*scheduledCell
	activeByAccount map[int64]int
	active          int
	cursor          int
	manualBurst     int
	priority        func(*scheduledCell, time.Time) int
	// weight only reads cached runtime plan data; nil preserves legacy RR fixtures.
	weight         func(int64, Config) (string, int)
	credits        map[int64]accountCredit
	creditTier     int
	weightConfig   [3]int
	autoDispatches int
	overdueCursor  int
}

type accountCredit struct {
	plan   string
	weight int
	value  int
}

type harvestCandidate struct {
	accountIndex int
	cellIndex    int
	tier         int
	cell         *scheduledCell
}

type scheduledCell struct {
	batchSequence        int64
	batchID              string
	policyEpoch          int64
	expectedGroups       database.AstraPolicyExpectation
	ordinaryMisses       int
	missThresholdPercent int
	key                  string
	accountID            int64
	model                string
	index                int
	manual               bool
	force                bool
	active               bool
	terminal             bool
	attempt              int
	max                  int
	ready                time.Time
	generation           *cellGeneration
	version              uint64
	breakerLease         uint64
	waitingSince         time.Time
	policyRecovery       bool
	regionPlan           [4]string
	regionConfig         regionPlanConfig
}

func cloneJob(job *Job) *Job {
	if job == nil {
		return nil
	}
	cp := *job
	cp.Cells = append([]JobCell(nil), job.Cells...)
	cp.AccountIDs = append([]int64(nil), job.AccountIDs...)
	return &cp
}

func (h *Harvester) CurrentJob() *Job {
	h.mu.Lock()
	defer h.mu.Unlock()
	return cloneJob(h.job)
}

func (h *Harvester) Start(ctx context.Context) {
	h.startOnce.Do(func() {
		h.mu.Lock()
		h.rootCtx = ctx
		s := h.scheduler
		h.mu.Unlock()
		if s != nil {
			context.AfterFunc(ctx, s.cancel)
		}
		go h.loop(ctx)
	})
}

// Multiple enable events collapse into one pending scan; no extra worker or
// timer is created and the scanner rechecks current settings before admission.
func (h *Harvester) requestAutoScan() {
	select {
	case h.autoWake <- struct{}{}:
	default:
	}
}

func (h *Harvester) loop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	h.scanAuto(ctx) // Startup: no initial one-minute wait.
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.scanAuto(ctx)
		case <-h.autoWake:
			h.scanAuto(ctx)
		}
	}
}

// Submission never waits for a busy job. IntervalMinutes is deliberately not
// a scan gate: freshness and cooldown decide whether each cell is due.
func (h *Harvester) scanAuto(ctx context.Context) {
	cfg := GetConfig()
	if !cfg.AutoHarvest || !HarvestReady(cfg) || ctx.Err() != nil {
		return
	}
	_, _, _ = h.submit(ctx, 0, nil, "", false, false)
}

func (h *Harvester) CancelJob() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scheduler == nil || h.job.FinishedUnix != 0 || h.job.Done == h.job.Total {
		return
	}
	h.job.Cancel = true
	h.job.Status = "cancelling"
	h.scheduler.cancel()
	h.signalLocked(h.scheduler)
}

func (h *Harvester) Enqueue(accountID int64, model string, force bool) (*Job, error) {
	job, _, err := h.submit(context.Background(), accountID, nil, model, force, true)
	return job, err
}

func (h *Harvester) EnqueueSelected(accountIDs []int64, model string, force bool) (*Job, error) {
	if len(accountIDs) == 0 || len(accountIDs) > 500 {
		return nil, fmt.Errorf("请选择 1–500 个账号")
	}
	ids := uniqueIDs(accountIDs)
	if len(ids) != len(accountIDs) {
		return nil, fmt.Errorf("账号 ID 必须为正且不能重复")
	}
	for _, id := range ids {
		if h.store.FindByID(id) == nil {
			return nil, fmt.Errorf("账号 #%d 不存在", id)
		}
	}
	job, _, err := h.submit(context.Background(), 0, ids, model, force, true)
	return job, err
}

// Run preserves the synchronous API; the minute scanner uses submit directly.
func (h *Harvester) Run(ctx context.Context, accountID int64, model string, force bool) {
	_, s, err := h.submit(ctx, accountID, nil, model, force, false)
	if err != nil || s == nil {
		return
	}
	select {
	case <-s.done:
	case <-ctx.Done():
		s.cancel()
		<-s.done
	}
}

func (h *Harvester) submit(ctx context.Context, accountID int64, ids []int64, model string, force, manual bool) (*Job, *harvestScheduler, error) {
	cfg := GetConfig()
	if accountID < 0 {
		return nil, nil, fmt.Errorf("账号 ID 必须为正")
	}
	if err := ValidateHarvestProxyConfig(cfg); err != nil {
		return nil, nil, err
	}
	if !HarvestReady(cfg) {
		return nil, nil, fmt.Errorf("所选采集代理未配置完整，无法探测")
	}
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	model = strings.TrimSpace(model)
	accounts := h.harvestAccounts(accountID)
	if len(ids) > 0 {
		accounts = h.selectedAccounts(ids)
	}
	// Bound temporary admission as well as the actual queue. Manual requests
	// exceeding the bound fail atomically; automatic overflow waits for a scan.
	if !manual {
		allowed := accounts[:0]
		for _, acc := range accounts {
			if h.automaticPolicyAllowed(acc.ID(), astraModel, cfg) || !h.PolicySnapshot(acc.ID()).Demoted {
				allowed = append(allowed, acc)
			}
		}
		accounts = allowed
	}
	cells := h.dueCellsBounded(accounts, cfg, model, force, maxHarvestCells+1)
	if !manual {
		allowed := cells[:0]
		for _, cell := range cells {
			if h.automaticPolicyAllowed(cell.acc.ID(), cell.model, cfg) {
				allowed = append(allowed, cell)
			}
		}
		cells = allowed
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.scheduler
	if !manual && len(cells) == 0 && s == nil {
		return cloneJob(h.job), nil, nil
	}
	if s != nil && s.ctx.Err() != nil {
		return nil, nil, fmt.Errorf("探测任务正在取消")
	}
	missing := 0
	for _, c := range cells {
		if s == nil || s.cells[ticketKey(c.acc.ID(), c.model)] == nil {
			missing++
		}
	}
	used := 0
	if s != nil {
		used = len(s.cells)
	}
	if manual && used+missing > maxHarvestCells {
		return nil, nil, fmt.Errorf("探测队列已满（最多 %d 格），请稍后重试", maxHarvestCells)
	}
	if s == nil {
		parent := ctx
		if h.rootCtx != nil {
			parent = h.rootCtx
		}
		child, cancel := context.WithCancel(parent)
		s = &harvestScheduler{ctx: child, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1), cells: make(map[string]*scheduledCell), byAccount: make(map[int64][]*scheduledCell), activeByAccount: make(map[int64]int)}
		s.priority = h.cellPriority
		s.weight = h.accountPlanWeight
		kind := "auto"
		if manual {
			kind = "manual"
		}
		h.job = &Job{ID: randomSID()[:12], Kind: kind, AccountID: accountID, AccountIDs: append([]int64(nil), ids...), Model: model, Status: "running", Message: "探测中"}
		h.scheduler = s
		go h.schedule(s)
	} else if manual && h.job.Kind == "auto" {
		h.job.Kind = "mixed"
	} else if !manual && h.job.Kind == "manual" && missing > 0 {
		h.job.Kind = "mixed"
	}
	for _, c := range cells {
		key := ticketKey(c.acc.ID(), c.model)
		if existing := s.cells[key]; existing != nil {
			if manual && !existing.terminal {
				existing.manual = true
				existing.force = existing.force || force
			}
			continue
		}
		if len(s.cells) >= maxHarvestCells {
			break
		}
		generation, version := h.retainGeneration(c.acc.ID(), c.model)
		index := h.historySlotLocked(s, key)
		task := &scheduledCell{key: key, accountID: c.acc.ID(), model: c.model, index: index, manual: manual, force: force, max: cfg.MaxAttempts, generation: generation, version: version}
		task.syncRegionPlan(cfg)
		h.preparePolicyBatch(ctx, task, cfg)
		task.policyRecovery = cfg.AstraPolicyEnabled && strings.EqualFold(task.model, astraModel) && h.PolicySnapshot(task.accountID).Demoted
		task.waitingSince = time.Now()
		s.cells[key] = task
		if _, ok := s.byAccount[task.accountID]; !ok {
			s.accounts = append(s.accounts, task.accountID)
		}
		s.byAccount[task.accountID] = append(s.byAccount[task.accountID], task)
		h.job.Cells[index] = JobCell{AccountID: task.accountID, Email: accountEmail(c.acc), Model: task.model, Max: task.max, Phase: "queued", Status: "排队"}
		h.job.Total++
	}
	h.signalLocked(s)
	return cloneJob(h.job), s, nil
}

func (h *Harvester) historySlotLocked(s *harvestScheduler, key string) int {
	for i, cell := range h.job.Cells {
		if ticketKey(cell.AccountID, cell.Model) == key {
			return i
		}
	}
	if len(h.job.Cells) < maxHarvestCells {
		h.job.Cells = append(h.job.Cells, JobCell{})
		return len(h.job.Cells) - 1
	}
	for i, cell := range h.job.Cells {
		if s.cells[ticketKey(cell.AccountID, cell.Model)] == nil {
			return i
		}
	}
	panic("turnstate history admission invariant")
}

func (h *Harvester) signalLocked(s *harvestScheduler) {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func retryDelay(attempt int) time.Duration {
	return time.Second * time.Duration(1<<min(max(attempt-1, 0), 5))
}

// Each class keeps the existing account/model RR tie order. Automatic work
// adds smooth WRR only within the best effective urgency tier.
func (s *harvestScheduler) candidates(now time.Time, accountLimit int, manual bool) ([]harvestCandidate, int) {
	var candidates []harvestCandidate
	bestTier := -1
	for n := 0; n < len(s.accounts); n++ {
		i := (s.cursor + n) % len(s.accounts)
		id := s.accounts[i]
		if s.activeByAccount[id] >= accountLimit {
			continue
		}
		best, tier := -1, 0
		for j, c := range s.byAccount[id] {
			if c.terminal || c.active || c.manual != manual || c.ready.After(now) {
				continue
			}
			priority := 0
			if s.priority != nil {
				priority = s.priority(c, now)
			}
			if cellOverdue(c, now) {
				priority = 0
			}
			if best < 0 || priority < tier || priority == tier && c.waitingSince.Before(s.byAccount[id][best].waitingSince) {
				best, tier = j, priority
			}
		}
		if best >= 0 {
			candidates = append(candidates, harvestCandidate{i, best, tier, s.byAccount[id][best]})
			if bestTier < 0 || tier < bestTier {
				bestTier = tier
			}
		}
	}
	return candidates, bestTier
}

func cellOverdue(c *scheduledCell, now time.Time) bool {
	return !c.waitingSince.IsZero() && now.Sub(c.waitingSince) >= 2*time.Minute
}

// Sync on every pick, including manual picks, without accruing credit. Only
// runnable accounts in the selected tier retain state, bounded by queue size.
func (s *harvestScheduler) syncCredits(candidates []harvestCandidate, tier int) {
	cfg := GetConfig()
	weights := [3]int{cfg.PlanWeightPro, cfg.PlanWeightProlite, cfg.PlanWeightPlus}
	if s.credits == nil || s.creditTier != tier || s.weightConfig != weights {
		s.credits = make(map[int64]accountCredit)
	}
	s.creditTier, s.weightConfig = tier, weights
	eligible := make(map[int64]bool, len(candidates))
	for _, candidate := range candidates {
		if candidate.tier != tier {
			continue
		}
		id := candidate.cell.accountID
		plan, weight := s.weight(id, cfg)
		weight = clampInt(weight, 1, 10)
		credit := s.credits[id]
		if credit.plan != plan || credit.weight != weight {
			credit = accountCredit{plan: plan, weight: weight}
		}
		s.credits[id] = credit
		eligible[id] = true
	}
	for id := range s.credits {
		if !eligible[id] {
			delete(s.credits, id)
		}
	}
	bound := 10 * len(s.credits)
	for id, credit := range s.credits {
		credit.value = clampInt(credit.value, -bound, bound)
		s.credits[id] = credit
	}
}

func (s *harvestScheduler) dispatchCandidate(candidate harvestCandidate) *scheduledCell {
	list := s.byAccount[candidate.cell.accountID]
	copy(list[candidate.cellIndex:], list[candidate.cellIndex+1:])
	list[len(list)-1] = candidate.cell
	s.cursor = (candidate.accountIndex + 1) % len(s.accounts)
	return candidate.cell
}

func (s *harvestScheduler) pickAutomatic(candidates []harvestCandidate, tier int, now time.Time) *scheduledCell {
	if len(candidates) == 0 {
		return nil
	}
	selected := -1
	if s.weight != nil {
		// Every fourth automatic dispatch reserves an unweighted overdue RR
		// quantum. Manual traffic cannot advance this counter or cursor.
		s.autoDispatches = (s.autoDispatches + 1) % 4
		if s.autoDispatches == 0 {
			distance := len(s.accounts)
			for i, candidate := range candidates {
				if candidate.tier != tier || !cellOverdue(candidate.cell, now) {
					continue
				}
				d := (candidate.accountIndex - s.overdueCursor + len(s.accounts)) % len(s.accounts)
				if d < distance {
					selected, distance = i, d
				}
			}
			if selected >= 0 {
				s.overdueCursor = (candidates[selected].accountIndex + 1) % len(s.accounts)
				return s.dispatchCandidate(candidates[selected])
			}
		}
		total, highest := 0, 0
		bound := 10 * len(s.credits)
		for i, candidate := range candidates {
			if candidate.tier != tier {
				continue
			}
			id := candidate.cell.accountID
			credit := s.credits[id]
			credit.value = clampInt(credit.value+credit.weight, -bound, bound)
			s.credits[id] = credit
			total += credit.weight
			if selected < 0 || credit.value > highest {
				selected, highest = i, credit.value
			}
		}
		id := candidates[selected].cell.accountID
		credit := s.credits[id]
		credit.value = clampInt(credit.value-total, -bound, bound)
		s.credits[id] = credit
	} else {
		for i, candidate := range candidates {
			if candidate.tier == tier {
				selected = i
				break
			}
		}
	}
	return s.dispatchCandidate(candidates[selected])
}

// Three manual quanta at most can pass runnable automatic work. Nil-weight
// fixtures retain the original RR picker, including its ageing behavior.
func (s *harvestScheduler) pick(now time.Time, accountLimit int) *scheduledCell {
	automatic, autoTier := s.candidates(now, accountLimit, false)
	if s.weight != nil {
		s.syncCredits(automatic, autoTier)
	}
	if s.manualBurst >= manualBurstLimit {
		if c := s.pickAutomatic(automatic, autoTier, now); c != nil {
			s.manualBurst = 0
			return c
		}
	}
	manual, manualTier := s.candidates(now, accountLimit, true)
	for _, candidate := range manual {
		if candidate.tier == manualTier {
			s.manualBurst = min(s.manualBurst+1, manualBurstLimit)
			return s.dispatchCandidate(candidate)
		}
	}
	if c := s.pickAutomatic(automatic, autoTier, now); c != nil {
		s.manualBurst = 0
		return c
	}
	return nil
}

// FindByID and GetPlanType read runtime caches, never policy/database state.
func (h *Harvester) accountPlanWeight(id int64, cfg Config) (string, int) {
	plan := ""
	if acc := h.store.FindByID(id); acc != nil {
		plan = normalizeHarvestPlan(acc.GetPlanType())
	}
	return plan, harvestPlanWeight(cfg, plan)
}

// Priority chooses the best tier across accounts within a manual/auto class;
// ties retain RR account order, then longest waiting model within the account.
// Admission caches policy demotion, so pick never waits on policy DB refresh.
func (h *Harvester) cellPriority(c *scheduledCell, now time.Time) int {
	if GetConfig().AstraPolicyEnabled && c.policyRecovery {
		return 2
	}
	ticket, ok := h.cache.Get(c.accountID, c.model)
	if !ok || ticket.Token == "" || ticket.Length != FullBloodChars || ticket.IssuedUnix > now.Unix()+60 || RemainingSeconds(ticket.IssuedUnix, now.Unix()) <= 300 {
		return 0
	}
	return 1
}

func (h *Harvester) schedule(s *harvestScheduler) {
	for {
		h.mu.Lock()
		if s.ctx.Err() != nil {
			h.job.Cancel = true
			h.job.Status = "cancelling"
			for _, c := range s.cells {
				if !c.active && !c.terminal {
					h.terminalLocked(s, c, "cancelled", "已取消", "")
				}
			}
		}
		if h.job.Done == h.job.Total && s.active == 0 {
			h.job.Status = "done"
			h.job.Message = "探测完成"
			if h.job.Total == 0 {
				h.job.Message = "没有符合条件的待探测格子"
			}
			if h.job.Cancel {
				h.job.Status, h.job.Message = "cancelled", "探测已取消"
			}
			h.job.FinishedUnix = time.Now().Unix()
			h.scheduler = nil
			s.cancel()
			close(s.done)
			h.mu.Unlock()
			return
		}
		cfg := GetConfig()
		if s.ctx.Err() == nil {
			for s.active < clampInt(cfg.Concurrency, 1, 12) {
				now := time.Now()
				if !h.breaker.available(now) {
					break
				}
				c := s.pick(now, clampInt(cfg.AccountConcurrency, 1, 4))
				if c == nil {
					break
				}
				c.breakerLease = h.breaker.reserve()
				c.active = true
				s.active++
				s.activeByAccount[c.accountID]++
				if s.activeByAccount[c.accountID] >= clampInt(cfg.AccountConcurrency, 1, 4) {
					delete(s.credits, c.accountID)
				}
				cell := &h.job.Cells[c.index]
				cell.Phase, cell.Status, cell.Active = "running", "探测", true
				task := *c     // Workers must never read mutable admission/promotion fields.
				task.attempt++ // Only committed when the acquisition probe actually starts.
				go h.work(s, c, task, cfg)
			}
		}
		delay := time.Minute
		if h.breaker.state == "open" {
			delay = min(delay, max(time.Until(h.breaker.retryAt), time.Millisecond))
		}
		for _, c := range s.cells {
			if !c.active && !c.terminal && !c.ready.IsZero() {
				if d := time.Until(c.ready); d > 0 && d < delay {
					delay = d
				}
			}
		}
		cancelled := s.ctx.Err() != nil
		h.mu.Unlock()
		timer := time.NewTimer(delay)
		if cancelled {
			<-s.wake // wait for active workers; do not spin on the cancelled context
		} else {
			select {
			case <-s.wake:
			case <-timer.C:
			case <-s.ctx.Done():
			}
		}
		timer.Stop()
	}
}

func (h *Harvester) terminalLocked(s *harvestScheduler, c *scheduledCell, phase, status, detail string) {
	if c.terminal {
		return
	}
	c.terminal = true
	cell := &h.job.Cells[c.index]
	cell.Phase, cell.Status, cell.Detail, cell.Active = phase, status, detail, false
	h.job.Done++
	switch phase {
	case "succeeded":
		h.job.Succeeded++
	case "exhausted":
		h.job.Exhausted++
	case "skipped":
		h.job.Skipped++
	case "cancelled":
		h.job.CancelledCount++
	}
	delete(s.cells, c.key)
	list := s.byAccount[c.accountID]
	for i, other := range list {
		if other == c {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(s.byAccount, c.accountID)
		delete(s.activeByAccount, c.accountID)
		delete(s.credits, c.accountID)
		for i, id := range s.accounts {
			if id == c.accountID {
				s.accounts = append(s.accounts[:i], s.accounts[i+1:]...)
				if s.cursor > i {
					s.cursor--
				}
				if s.overdueCursor > i {
					s.overdueCursor--
				}
				break
			}
		}
		if len(s.accounts) > 0 {
			s.cursor %= len(s.accounts)
			s.overdueCursor %= len(s.accounts)
		} else {
			s.cursor = 0
			s.overdueCursor = 0
		}
	} else {
		s.byAccount[c.accountID] = list
	}
	h.releaseGeneration(c.key, c.generation)
}

func (h *Harvester) work(s *harvestScheduler, c *scheduledCell, task scheduledCell, cfg Config) {
	// Settings/participation may change after dispatch but before this goroutine runs.
	cfg = GetConfig()
	task.syncRegionPlan(cfg)
	result := attemptResult{phase: "skipped", status: "跳过", detail: "账号或模型已不可用"}
	acc := h.store.FindByID(task.accountID)
	eligible := acc != nil && len(filterHarvestAccounts([]*auth.Account{acc}, cfg)) > 0 && HarvestReady(cfg)
	foundModel := false
	for _, model := range cfg.Models {
		if strings.EqualFold(model, task.model) {
			foundModel = true
			break
		}
	}
	if s.ctx.Err() != nil {
		result = cancelledResult()
	} else if !h.generationCurrent(task.key, task.generation, task.version) {
		result = staleResult()
	} else if !task.manual && (!cfg.AutoHarvest || !h.automaticPolicyAllowed(task.accountID, task.model, cfg)) {
		result = attemptResult{phase: "skipped", status: "跳过", detail: "自动收割已关闭"}
	} else if !HarvestReady(cfg) {
		result = attemptResult{phase: "skipped", status: "跳过", detail: "所选采集代理配置无效或不完整"}
	} else if eligible && foundModel {
		if !task.force && len(h.dueCells([]*auth.Account{acc}, cfg, task.model, false)) == 0 {
			result = attemptResult{phase: "skipped", status: "跳过", detail: "票据仍新鲜或处于冷却期"}
		} else {
			result = h.attempt(s.ctx, cfg, acc, task, func() {
				h.mu.Lock()
				h.job.Cells[c.index].Phase, h.job.Cells[c.index].Status = "confirming", "确认"
				h.mu.Unlock()
			})
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	c.regionPlan, c.regionConfig = task.regionPlan, task.regionConfig
	c.active = false
	h.breaker.release(task.breakerLease)
	c.breakerLease = 0
	s.active--
	s.activeByAccount[c.accountID]--
	if s.ctx.Err() != nil {
		result = cancelledResult()
	}
	if result.ordinaryMiss {
		c.ordinaryMisses++
	}
	if result.phase == "retrying" {
		now := time.Now()
		c.ready = now.Add(result.delay(c.attempt))
		c.waitingSince = now
		cell := &h.job.Cells[c.index]
		cell.Phase, cell.Status, cell.Detail, cell.Active = "retrying", "等待重试", result.detail, false
	} else {
		h.finishPolicyBatch(s.ctx, c, result)
		h.terminalLocked(s, c, result.phase, result.status, result.detail)
	}
	h.job.Message = fmt.Sprintf("%s %s %d/%d %s", emailOrID(h.job.Cells[c.index].Email, c.accountID), c.model, c.attempt, c.max, h.job.Cells[c.index].Status)
	h.signalLocked(s)
}
