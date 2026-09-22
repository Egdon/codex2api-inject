package admin

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

const codexTurnStateRenewalRetryInterval = 10 * time.Second

// StartCodexTurnStateRenewal starts one bounded worker pool. No account/page
// polling or incoming request is needed to keep an existing template alive.
func (h *Handler) StartCodexTurnStateRenewal(ctx context.Context) {
	if h == nil || h.db == nil || h.store == nil {
		return
	}
	h.codexTurnStateRenewalStartOnce.Do(func() {
		h.codexTurnStateRenewalWG.Add(1)
		go func() {
			defer h.codexTurnStateRenewalWG.Done()
			var workers sync.WaitGroup
			defer workers.Wait()
			slots := make(chan struct{}, 4)
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for ctx.Err() == nil {
				scanCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				rows, err := h.db.ListCodexTurnStateRenewalCandidates(scanCtx, time.Now().Unix())
				if err == nil {
					err = h.db.PruneCodexTurnStateRenewals(scanCtx)
					if err == nil {
						err = h.db.RecoverCodexTurnStateHistory(scanCtx, time.Now().UnixMilli())
					}
				}
				cancel()
				if err != nil && ctx.Err() == nil {
					log.Printf("[turn-state-renewal] template scan failed: %v", err)
				}
				for _, row := range rows {
					if ctx.Err() != nil {
						break
					}
					account := h.store.FindByID(row.AccountID)
					if !proxy.CodexTurnStateTemplateRenewalDue(account, row, time.Now()) || !account.ModelCatalogEligible() {
						continue
					}
					select {
					case slots <- struct{}{}:
						workers.Add(1)
						go func() {
							defer workers.Done()
							defer func() { <-slots }()
							h.renewCodexTurnStateModel(ctx, row, time.Now)
						}()
					default:
						// Reconsider on the next tick; never queue unbounded probe goroutines.
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	})
}

func (h *Handler) WaitCodexTurnStateRenewal() { h.codexTurnStateRenewalWG.Wait() }

func (h *Handler) renewCodexTurnStateModel(ctx context.Context, row database.CodexTurnStateTemplate, now func() time.Time) {
	account := h.store.FindByID(row.AccountID)
	if ctx.Err() != nil || !proxy.CodexTurnStateTemplateRenewalDue(account, row, now()) || !account.ModelCatalogEligible() {
		return
	}
	if _, running := codexTurnStateRefreshRunning.LoadOrStore(row.AccountID, true); running {
		return
	}
	defer codexTurnStateRefreshRunning.Delete(row.AccountID)

	claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	// The lease covers database operations, the 60-second probe and retry delay if the
	// process dies. Completion below starts the normal delay from failure time.
	attempt, err := h.db.ClaimCodexTurnStateRenewal(claimCtx, row, now().Unix(), now().Add(90*time.Second).Unix())
	cancel()
	if err != nil {
		log.Printf("[turn-state-renewal] claim failed account=%d: %v", row.AccountID, err)
		return
	}
	if attempt == 0 {
		return
	}
	started := now()
	routeCtx, routeCancel := context.WithTimeout(ctx, 5*time.Second)
	route, routeErr := h.codexTurnStateRenewalProxy(routeCtx, account, row, attempt)
	if routeErr == nil {
		routeErr = h.db.RecordCodexTurnStateRenewalProxy(routeCtx, row, attempt, route.id, route.hash)
	}
	historyID := h.startCodexTurnStateHistory(routeCtx, account, row, attempt, route, started)
	routeCancel()
	saved := false
	var probeErr error
	if routeErr == nil {
		log.Printf("[turn-state-renewal] account=%d model=%s attempt=%d/%d proxy_id=%d route=%s started", row.AccountID, row.Model, attempt, database.CodexTurnStateRenewalMaxAttempts, route.id, route.source)
		// Acquisition and verification snapshot the same exit for this attempt.
		saved, probeErr = h.refreshCodexTurnStateModel(ctx, account, row.Model, route.url)
	}
	finishCtx, finishCancel := context.WithTimeout(ctx, 5*time.Second)
	defer finishCancel()
	current, exists, readErr := h.db.GetCodexTurnStateTemplate(finishCtx, row.AccountID, row.Model)
	// Acceptance of the same opaque token is not renewal. Only a later upstream
	// issuance extends validity; never bump issued_at to our local clock.
	renewed := saved && readErr == nil && exists && current.IssuedAt > row.IssuedAt
	nextAttempt := now().Add(codexTurnStateRenewalRetryInterval)
	nextSecond := nextAttempt.Unix()
	if nextAttempt.Nanosecond() != 0 {
		nextSecond++
	}
	if err := h.db.FinishCodexTurnStateRenewal(finishCtx, row, attempt, nextSecond); err != nil && ctx.Err() == nil {
		log.Printf("[turn-state-renewal] finish failed account=%d: %v", row.AccountID, err)
	}
	reason := "no_new_verified_template"
	status := "failed"
	expiresAfter := int64(0)
	if renewed {
		reason = "renewed"
		status = "success"
		expiresAfter = proxy.CodexTurnStateTemplateExpiresAt(current.IssuedAt).UnixMilli()
	}
	if probeErr != nil {
		reason = "probe_failed"
	}
	if routeErr != nil {
		reason = "no_available_proxy_or_route_error"
	}
	if ctx.Err() != nil && !renewed {
		status = "interrupted"
		reason = "worker_interrupted"
	}
	if ctx.Err() == nil {
		log.Printf("[turn-state-renewal] account=%d model=%s attempt=%d/%d proxy_id=%d route=%s renewed=%t result=%s", row.AccountID, row.Model, attempt, database.CodexTurnStateRenewalMaxAttempts, route.id, route.source, renewed, reason)
	}
	if historyID > 0 {
		// Graceful cancellation still gets a terminal record before the DB closes.
		historyCtx, historyCancel := context.WithTimeout(context.Background(), 3*time.Second)
		finished := now()
		if err := h.db.FinishCodexTurnStateHistory(historyCtx, historyID, status, reason, finished.UnixMilli(), max(0, finished.Sub(started).Milliseconds()), expiresAfter); err != nil {
			log.Printf("[turn-state-renewal] history finish failed account=%d: %v", row.AccountID, err)
		}
		historyCancel()
	}

	if renewed {
		h.invalidateAccountSnapshotCaches()
	}
}

type codexTurnStateRenewalProxy struct {
	url        string
	id         int64
	hash       string
	source     string
	displayURL string
	name       string
	ip         string
}

func turnStateRenewalProxyHash(url string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(url))))
}

func (h *Handler) codexTurnStateRenewalProxy(ctx context.Context, account *auth.Account, row database.CodexTurnStateTemplate, attempt int) (codexTurnStateRenewalProxy, error) {
	initial := strings.TrimSpace(account.CodexTurnStateProxy())
	initialExit := initial
	if initialExit == "" {
		initialExit = h.store.ResolveProxyForAccount(account)
	}
	// ListEnabledProxies intentionally does not require the ordinary pool switch:
	// only this explicit renewal attempt uses the chosen exit, without rebinding.
	nodes, err := h.db.ListEnabledProxies(ctx)
	if err != nil {
		return codexTurnStateRenewalProxy{}, err
	}
	if attempt == 1 {
		route := codexTurnStateRenewalProxy{url: initial, hash: turnStateRenewalProxyHash(initialExit), source: "account-default"}
		if initial != "" {
			route.source = "issuance"
			route.displayURL = initial
			for _, node := range nodes {
				if strings.TrimSpace(node.URL) == initial {
					route.id, route.name, route.ip = node.ID, node.Label, node.TestIP
					break
				}
			}
		}
		if initial == "" {
			egress := proxy.ResolveCodexEgress(account, "https://chatgpt.com/backend-api/codex/responses", initialExit)
			switch egress.Kind {
			case proxy.CodexEgressResin:
				route.source = "resin"
				route.displayURL = egress.URL
			case proxy.CodexEgressDirect:
				route.source = "direct"
			default:
				route.displayURL = egress.DialProxyURL
				for _, node := range nodes {
					if strings.TrimSpace(node.URL) == egress.DialProxyURL {
						route.id, route.name, route.ip = node.ID, node.Label, node.TestIP
						break
					}
				}
			}
		}
		return route, nil
	}
	used, err := h.db.CodexTurnStateRenewalProxyHashes(ctx, row)
	if err != nil {
		return codexTurnStateRenewalProxy{}, err
	}
	// Also exclude the configured initial exit when upgrading a pre-rotation job
	// whose earlier attempts did not record proxy hashes.
	used[turnStateRenewalProxyHash(initialExit)] = true
	for _, node := range nodes {
		url := strings.TrimSpace(node.URL)
		hash := turnStateRenewalProxyHash(url)
		if url != "" && !used[hash] {
			return codexTurnStateRenewalProxy{url: url, id: node.ID, hash: hash, source: "pool", displayURL: url, name: node.Label, ip: node.TestIP}, nil
		}
	}
	return codexTurnStateRenewalProxy{source: "pool"}, fmt.Errorf("no untried enabled proxy available")
}
