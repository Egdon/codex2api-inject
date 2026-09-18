package turnstate

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/codex2api/auth"
)

// Entries exist only while queued/running cells refer to them. Pointer identity
// avoids ABA when the last reference is released and a later job reuses a key.
type cellGeneration struct {
	version uint64
	refs    int
}

func (h *Harvester) retainGeneration(accountID int64, model string) (*cellGeneration, uint64) {
	h.publishMu.Lock()
	defer h.publishMu.Unlock()
	key := ticketKey(accountID, model)
	g := h.generations[key]
	if g == nil {
		g = &cellGeneration{}
		h.generations[key] = g
	}
	g.refs++
	return g, g.version
}

func (h *Harvester) releaseGeneration(key string, g *cellGeneration) {
	h.publishMu.Lock()
	defer h.publishMu.Unlock()
	g.refs--
	if g.refs == 0 && h.generations[key] == g {
		delete(h.generations, key)
	}
}

func (h *Harvester) invalidateCellLocked(accountID int64, model string) {
	if g := h.generations[ticketKey(accountID, model)]; g != nil {
		g.version++
	}
}

func (h *Harvester) generationCurrent(key string, g *cellGeneration, version uint64) bool {
	h.publishMu.Lock()
	defer h.publishMu.Unlock()
	return h.generations[key] == g && g.version == version
}

// Both ManualPaste/Clear and workers use this same lock across memory and DB.
// A stale worker can neither resurrect a cleared row nor overwrite pasted data.
func (h *Harvester) publishAttempt(ctx context.Context, task scheduledCell, ticket CachedTicket) bool {
	h.publishMu.Lock()
	defer h.publishMu.Unlock()
	if ctx.Err() != nil || h.generations[task.key] != task.generation || task.generation.version != task.version {
		return false
	}
	h.persistTicket(ctx, ticket)
	return true
}

type attemptResult struct {
	ordinaryMiss   bool
	new292         bool
	confirmed      bool
	obtainedAtNano int64
	issuedUnix     int64
	retryKind      probeFailureKind
	retryAfter     time.Duration
	phase          string
	status         string
	detail         string
}

func cancelledResult() attemptResult { return attemptResult{phase: "cancelled", status: "已取消"} }
func staleResult() attemptResult {
	return attemptResult{phase: "skipped", status: "跳过", detail: "票据已被手动更新或清除"}
}

// Never expose transport errors, proxy credentials, response bodies, or tokens
// through snapshots/logs. Only known error classifications cross this boundary.
func probeErrorDetail(err error) string {
	if isUnauthorized(err) {
		return "401 跳过，等待主进程刷新 token"
	}
	low := strings.ToLower(err.Error())
	switch {
	case strings.Contains(low, "overload"):
		return "上游过载"
	case strings.Contains(low, "decrypt"):
		return "上游解密失败"
	case strings.Contains(low, "empty turn-state"):
		return "未返回 turn-state"
	default:
		return "代理探测失败"
	}
}

// A worker quantum is exactly one attempt plus, for a full token, one confirm.
// No retries or backoff sleep occur while a worker owns a scheduler slot.
func (h *Harvester) attempt(ctx context.Context, cfg Config, acc *auth.Account, task scheduledCell, confirming func()) attemptResult {
	if ctx.Err() != nil {
		return cancelledResult()
	}
	if !h.generationCurrent(task.key, task.generation, task.version) {
		return staleResult()
	}
	// Use the batch's immutable region for both acquisition and confirmation.
	// This is a local config copy; concurrent accounts and global config are untouched.
	if region := task.regionForAttempt(); region != "" {
		cfg.ZooRegion = region
	}
	existing, _ := h.cache.Get(acc.ID(), task.model)
	existing.AccountID, existing.Model = acc.ID(), task.model
	token, hdr, err := h.observedProbe(ctx, cfg, acc, task, "")
	if ctx.Err() != nil {
		return cancelledResult()
	}
	if !h.generationCurrent(task.key, task.generation, task.version) {
		return staleResult()
	}
	detail := ""
	ordinaryMiss := false
	retryKind, retryAfter := classifyRetry(err, hdr)
	obtainedAtNano := time.Now().UnixNano()
	if err == nil && overloadedStatus(hdr) {
		err = &probeFailure{kind: probeOverload}
		retryKind = probeOverload
	}
	if err != nil {
		detail = probeErrorDetail(err)
		if isUnauthorized(err) {
			existing.Attempts, existing.LastError, existing.LastHarvestAt = task.attempt, detail, time.Now().Unix()
			existing.CooldownUntil = time.Now().Add(time.Duration(max(cfg.CooldownMinutes, 1)) * time.Minute).Unix()
			if !h.publishAttempt(ctx, task, existing) {
				return staleResult()
			}
			return attemptResult{phase: "skipped", status: "401", detail: detail}
		}
	} else if parsed, ok := ParseFernet(token); ok && parsed.Length == FullBloodChars && parsed.IssuedUnix <= time.Now().Unix()+60 && RemainingSeconds(parsed.IssuedUnix, time.Now().Unix()) > 0 {
		confirming()
		warning := false
		confirmHdr := hdr
		_, confirmHeaders, confirmErr := h.observedProbe(ctx, cfg, acc, task, token)
		// Cancellation is not a confirmation warning and must never publish a token.
		if ctx.Err() != nil {
			return cancelledResult()
		}
		if confirmErr != nil {
			warning = true
			detail = "确认警告：" + probeErrorDetail(confirmErr)
		} else {
			confirmHdr = confirmHeaders
		}
		if overloadedStatus(confirmHdr) {
			warning, detail = true, "确认警告：上游过载"
		}
		ticket := CachedTicket{AccountID: acc.ID(), Model: task.model, Token: token, IssuedUnix: parsed.IssuedUnix, Length: parsed.Length, Blocks: parsed.Blocks, ConfirmWarning: warning, Attempts: task.attempt, LastError: detail, LastHarvestAt: time.Now().Unix()}
		if !h.publishAttempt(ctx, task, ticket) {
			return staleResult()
		}
		status := "292"
		if warning {
			status = "292 确认警告"
		}
		return attemptResult{phase: "succeeded", status: status, detail: detail, new292: true, confirmed: !warning, obtainedAtNano: obtainedAtNano, issuedUnix: parsed.IssuedUnix}
	} else {
		parsed, valid := ParseFernet(token)
		ordinaryMiss = valid && parsed.Length != FullBloodChars && parsed.IssuedUnix <= time.Now().Unix()+60 && RemainingSeconds(parsed.IssuedUnix, time.Now().Unix()) > 0
		detail = fmt.Sprintf("got %s len=%d", ClassOf(len(token)), len(token))
	}
	if task.attempt < task.max {
		return attemptResult{phase: "retrying", status: "等待重试", detail: detail, ordinaryMiss: ordinaryMiss, retryKind: retryKind, retryAfter: retryAfter}
	}
	// A failed harvest retains the prior token and its confirmation warning.
	existing.Attempts, existing.Exhausted = task.attempt, true
	existing.CooldownUntil = time.Now().Add(time.Duration(max(cfg.CooldownMinutes, 1)) * time.Minute).Unix()
	existing.LastHarvestAt = time.Now().Unix()
	existing.LastError = fmt.Sprintf("尝试耗尽 %d 次：%s；%d 分钟后再试", task.max, detail, max(cfg.CooldownMinutes, 1))
	if !h.publishAttempt(ctx, task, existing) {
		return staleResult()
	}
	return attemptResult{phase: "exhausted", status: "耗尽", detail: existing.LastError, ordinaryMiss: ordinaryMiss}
}

// Compatibility helper for existing package-level unit tests. Public harvest
// entrypoints always use the bounded shared scheduler, not this synchronous loop.
func (h *Harvester) harvestCell(ctx context.Context, cfg Config, acc *auth.Account, model string) {
	g, version := h.retainGeneration(acc.ID(), model)
	task := scheduledCell{key: ticketKey(acc.ID(), model), accountID: acc.ID(), model: model, generation: g, version: version, max: max(cfg.MaxAttempts, 1)}
	task.regionPlan = newBatchRegionPlan(cfg)
	defer h.releaseGeneration(task.key, g)
	for task.attempt = 1; task.attempt <= task.max; task.attempt++ {
		if result := h.attempt(ctx, cfg, acc, task, func() {}); result.phase != "retrying" {
			return
		}
	}
}
