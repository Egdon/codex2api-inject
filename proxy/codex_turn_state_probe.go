package proxy

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type turnStateAdminProbeKey struct{}
type turnStateRefreshKey struct{}

// WithCodexTurnStateAdminProbe enables cache injection for stateless admin probes.
func WithCodexTurnStateAdminProbe(ctx context.Context) context.Context {
	return context.WithValue(withTurnStateTemplateAudit(ctx), turnStateAdminProbeKey{}, true)
}

// Auto-review has no supported template policy and must not affect account health.
func CodexTurnStateModelAllowed(account *auth.Account, model string) bool {
	if account == nil || account.IsRelayStyle() || strings.TrimSpace(model) == "" || strings.EqualFold(strings.TrimSpace(model), "codex-auto-review") {
		return false
	}
	_, scope, _ := account.CodexTurnStateConfig()
	return auth.CodexTurnStateModelsMatch(scope, model)
}

// A refresh first acquires a candidate and then verifies it on a second request.
// Neither request can persist a candidate or disturb an existing usable template.
type CodexTurnStateRefresh struct {
	mu        sync.Mutex
	accountID int64
	model     string
	inject    string
	candidate string
	proxyURL  string
	observed  bool
}

func NewCodexTurnStateRefresh(ctx context.Context, accountID int64, model string, prior *CodexTurnStateRefresh, proxyURL ...string) (context.Context, *CodexTurnStateRefresh) {
	probe := &CodexTurnStateRefresh{accountID: accountID, model: model}
	if len(proxyURL) > 0 {
		probe.proxyURL = strings.TrimSpace(proxyURL[0])
	}
	if prior != nil {
		prior.mu.Lock()
		probe.proxyURL = prior.proxyURL
		probe.inject = prior.candidate
		prior.mu.Unlock()
	}
	return context.WithValue(WithCodexTurnStateAdminProbe(ctx), turnStateRefreshKey{}, probe), probe
}

func turnStateRefreshFromContext(ctx context.Context) *CodexTurnStateRefresh {
	if ctx == nil {
		return nil
	}
	p, _ := ctx.Value(turnStateRefreshKey{}).(*CodexTurnStateRefresh)
	return p
}

func (p *CodexTurnStateRefresh) HasCandidate() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.candidate != ""
}

func (p *CodexTurnStateRefresh) observe(account *auth.Account, model string, values []string) {
	if p.accountID != account.ID() || p.model != model {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observed = true
	p.candidate = ""
	if len(values) != 1 {
		return
	}
	cfg := loadTurnStateTemplateConfig()
	token, err := parseTurnStateToken(values[0])
	if err != nil || !turnStateTokenAccept(token, time.Now(), cfg.TTL, cfg.policyFor(account).TemplateBlocks) {
		return
	}
	p.candidate = values[0]
}

// SaveVerified must only be called after the validation response completes.
// Upstream need not reissue an unchanged state. In that case retain the acquired
// candidate and its original timestamp; an explicitly invalid response fails.
func (p *CodexTurnStateRefresh) SaveVerified(account *auth.Account) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !CodexTurnStateInjectionEnabled(account) || p.inject == "" || account.ID() != p.accountID || !CodexTurnStateModelAllowed(account, p.model) {
		return false
	}
	cfg := loadTurnStateTemplateConfig()
	value := p.candidate
	if !p.observed {
		value = p.inject
	}
	if !globalTurnStateTemplates.capture(cfg, cfg.policyFor(account), p.accountID, p.model, value) {
		return false
	}
	globalTurnStateTemplates.observeStatus(cfg, account, p.model, p.inject, false)
	if p.observed {
		globalTurnStateTemplates.observeStatus(cfg, account, p.model, value, false)
	} else {
		// Successful use without reissue is not a second shape observation.
		globalTurnStateTemplates.noteRewrite(cfg, account, p.model)
	}
	return true
}

func PopulateCodexTurnStateProbeUsage(c *gin.Context, input *database.UsageLogInput) {
	populateTurnStateTemplateMetaFromRequest(c, input)
}

// Refresh attempts deliberately override manual values only within the probe;
// normal connection/quality tests retain the account's manual override priority.
func turnStateRefreshInjection(ctx context.Context, account *auth.Account, model string) (string, bool) {
	p := turnStateRefreshFromContext(ctx)
	if p == nil {
		return "", false
	}
	if p.accountID != account.ID() || p.model != model {
		return "", true
	}
	return p.inject, true
}

func applyTurnStateRefreshHeader(ctx context.Context, headers http.Header, account *auth.Account, model string) bool {
	injected, refresh := turnStateRefreshInjection(ctx, account, model)
	if !refresh {
		return false
	}
	headers.Del(codexTurnStateHeader)
	if injected != "" {
		headers.Set(codexTurnStateHeader, injected)
	}
	return true
}

// Account opt-out has priority over the global template switch for every injection path.
func CodexTurnStateInjectionEnabled(account *auth.Account) bool {
	return accountEligibleForTurnStateTemplate(account) && CurrentRuntimeSettings().CodexTurnStateTemplateCache && !account.IsCodexTurnStateDisabled()
}

// CodexTurnStateRefreshProxy is scoped to one account's explicit refresh. It
// never changes the account's ordinary proxy, and validation keeps the same exit.
func CodexTurnStateRefreshProxy(ctx context.Context, account *auth.Account) string {
	p := turnStateRefreshFromContext(ctx)
	if p == nil || account == nil || p.accountID != account.ID() {
		return ""
	}
	return p.proxyURL
}
