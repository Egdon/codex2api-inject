package proxy

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// X-Codex-Turn-State Fernet template cache (v1, sleep-state aligned).
//
// Database rows keyed by (account DBID, exact upstream model) store reusable
// Fernet templates observed on upstream responses. Accept uses Fernet Blocks
// (personal 10 / team 12), never forges, never harvests client request headers,
// never crosses account or model. Compose AFTER guardCodexTurnStateEcho.
//
// Master switch defaults OFF (settings: codex_turn_state_template_cache_enabled).

const (
	turnStatePersonalTemplateBlocks = 10
	turnStatePersonalReplaceBlocks  = 11
	turnStateTeamTemplateBlocks     = 12
	turnStateTeamReplaceBlocks      = 13

	defaultTurnStateTemplateTTL = time.Hour
	defaultTurnStateTemplateMax = 256
	turnStateAcceptSkew         = 30 * time.Second
	turnStateStrikeThreshold    = 2

	turnStateAccountModePersonal = "personal"
	turnStateAccountModeTeam     = "team"
	turnStateAccountModeAuto     = "auto"

	turnStateInjectReplaceOnly = "replace-only"
	turnStateInjectAlways      = "always"
)

type turnStateTemplateConfig struct {
	Enabled      bool
	AccountMode  string
	InjectMode   string
	TTL          time.Duration
	LogDecisions bool
	MaxEntries   int
	DryRun       bool
	// Optional env length overrides (test / advanced). When set and mappable to
	// Fernet block counts, they force TemplateBlocks/ReplaceBlocks for all modes.
	ForceTemplateBlocks int
	ForceReplaceBlocks  int
}

type turnStateLengthPolicy struct {
	Mode           string
	TemplateBlocks int
	ReplaceBlocks  int
	TemplateLength int
	ReplaceLength  int
}

func turnStateEncodedLength(blocks int) int {
	return base64.URLEncoding.EncodedLen(57 + 16*blocks)
}

func blocksForEncodedLength(n int) (int, bool) {
	for b := 1; b <= 32; b++ {
		if turnStateEncodedLength(b) == n {
			return b, true
		}
	}
	return 0, false
}

func loadTurnStateTemplateConfig() turnStateTemplateConfig {
	cfg := turnStateTemplateConfig{
		Enabled:      CurrentRuntimeSettings().CodexTurnStateTemplateCache,
		AccountMode:  NormalizeCodexTurnStateAccountMode(CurrentRuntimeSettings().CodexTurnStateAccountMode),
		InjectMode:   turnStateInjectReplaceOnly,
		TTL:          defaultTurnStateTemplateTTL,
		LogDecisions: parseTurnStateBoolEnv(os.Getenv("CODEX_TURN_STATE_LOG_DECISIONS")),
		MaxEntries:   defaultTurnStateTemplateMax,
		DryRun:       parseTurnStateBoolEnv(os.Getenv("CODEX_TURN_STATE_DRY_RUN")),
	}
	if v := strings.TrimSpace(os.Getenv("CODEX_TURN_STATE_TTL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.TTL = d
		} else if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
			cfg.TTL = time.Duration(sec) * time.Second
		}
	}
	if v := strings.TrimSpace(os.Getenv("CODEX_TURN_STATE_MAX_ENTRIES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxEntries = n
		}
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_TURN_STATE_INJECT_MODE"))) {
	case turnStateInjectAlways:
		cfg.InjectMode = turnStateInjectAlways
	default:
		cfg.InjectMode = turnStateInjectReplaceOnly
	}
	if v := strings.TrimSpace(os.Getenv("CODEX_TURN_STATE_TEMPLATE_LENGTH")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if b, ok := blocksForEncodedLength(n); ok {
				cfg.ForceTemplateBlocks = b
			} else {
				cfg.Enabled = false
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv("CODEX_TURN_STATE_REPLACE_LENGTH")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if b, ok := blocksForEncodedLength(n); ok {
				cfg.ForceReplaceBlocks = b
			} else {
				cfg.Enabled = false
			}
		}
	}
	if cfg.ForceTemplateBlocks > 0 && cfg.ForceReplaceBlocks > 0 &&
		cfg.ForceTemplateBlocks == cfg.ForceReplaceBlocks {
		cfg.Enabled = false
	}
	return cfg
}

func (c turnStateTemplateConfig) injectAlways() bool {
	return c.InjectMode == turnStateInjectAlways
}

func (c turnStateTemplateConfig) policyFor(account *auth.Account) turnStateLengthPolicy {
	mode := turnStateAccountModePersonal
	switch c.AccountMode {
	case turnStateAccountModePersonal, turnStateAccountModeTeam:
		mode = c.AccountMode
	default: // auto
		if hint := turnStatePlanHint(account); hint != "" {
			mode = hint
		}
	}
	p := turnStateLengthPolicy{Mode: mode}
	if mode == turnStateAccountModeTeam {
		p.TemplateBlocks = turnStateTeamTemplateBlocks
		p.ReplaceBlocks = turnStateTeamReplaceBlocks
	} else {
		p.TemplateBlocks = turnStatePersonalTemplateBlocks
		p.ReplaceBlocks = turnStatePersonalReplaceBlocks
	}
	if c.ForceTemplateBlocks > 0 {
		p.TemplateBlocks = c.ForceTemplateBlocks
	}
	if c.ForceReplaceBlocks > 0 {
		p.ReplaceBlocks = c.ForceReplaceBlocks
	}
	p.TemplateLength = turnStateEncodedLength(p.TemplateBlocks)
	p.ReplaceLength = turnStateEncodedLength(p.ReplaceBlocks)
	return p
}

func turnStatePlanHint(account *auth.Account) string {
	if account == nil {
		return ""
	}
	// A selected workspace may differ from the token's default workspace.
	if account.AccountIDOverridden() {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(account.GetPlanType())) {
	case "team", "business":
		return turnStateAccountModeTeam
	case "free", "plus", "pro":
		return turnStateAccountModePersonal
	default:
		return ""
	}
}

func parseTurnStateBoolEnv(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

// ==================== strict Fernet parse (sleep-state aligned) ====================

type turnStateToken struct {
	Value  string
	Issued time.Time
	Blocks int
}

func parseTurnStateToken(value string) (turnStateToken, error) {
	var t turnStateToken
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 2048 || strings.ContainsAny(value, "\r\n\t ") {
		return t, errors.New("invalid state encoding")
	}
	core := strings.TrimRight(value, "=")
	if len(value)-len(core) > 2 {
		return t, errors.New("invalid state padding")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(core)
	if err != nil || len(raw) < 73 || raw[0] != 0x80 || (len(raw)-57)%16 != 0 {
		return t, errors.New("unrecognized state envelope")
	}
	issued := binary.BigEndian.Uint64(raw[1:9])
	if issued < 1577836800 || issued >= 4102444800 {
		return t, errors.New("state timestamp out of range")
	}
	return turnStateToken{
		Value:  value,
		Issued: time.Unix(int64(issued), 0),
		Blocks: (len(raw) - 57) / 16,
	}, nil
}

// turnStateTokenTimeValid checks issued/TTL skew only (no block-policy).
// Used by global purge so personal lookups do not wipe team-length entries.
func turnStateTokenTimeValid(t turnStateToken, now time.Time, ttl time.Duration) bool {
	if t.Value == "" {
		return false
	}
	if t.Issued.After(now.Add(turnStateAcceptSkew)) {
		return false
	}
	return now.Before(t.Issued.Add(ttl - turnStateAcceptSkew))
}

// Accept usable only if Blocks match, issued not >skew in the future, and
// now is before issued+(TTL-skew). Matches sleep-state Policy.Accept.
func turnStateTokenAccept(t turnStateToken, now time.Time, ttl time.Duration, expectedBlocks int) bool {
	if t.Blocks != expectedBlocks {
		return false
	}
	return turnStateTokenTimeValid(t, now, ttl)
}

type turnStateTemplateKey struct {
	AccountID int64
	Model     string
}

type turnStateTemplateEntry struct {
	Value    string
	IssuedAt time.Time
	Strikes  int
}

type turnStateTemplateStore struct {
	mu           sync.Mutex
	entries      map[turnStateTemplateKey]turnStateTemplateEntry
	observations map[turnStateTemplateKey]turnStateObservation
	db           *database.DB
	now          func() time.Time
}

func newTurnStateTemplateStore() *turnStateTemplateStore {
	return &turnStateTemplateStore{
		entries:      make(map[turnStateTemplateKey]turnStateTemplateEntry),
		observations: make(map[turnStateTemplateKey]turnStateObservation),
		now:          time.Now,
	}
}

var globalTurnStateTemplates = newTurnStateTemplateStore()

func resetTurnStateTemplateStoreForTest() {
	globalTurnStateTemplates.mu.Lock()
	defer globalTurnStateTemplates.mu.Unlock()
	globalTurnStateTemplates.db = nil
	globalTurnStateTemplates.entries = make(map[turnStateTemplateKey]turnStateTemplateEntry)
	globalTurnStateTemplates.observations = make(map[turnStateTemplateKey]turnStateObservation)
	globalTurnStateTemplates.now = time.Now
}

func setTurnStateTemplateNowForTest(now func() time.Time) {
	globalTurnStateTemplates.mu.Lock()
	defer globalTurnStateTemplates.mu.Unlock()
	if now == nil {
		globalTurnStateTemplates.now = time.Now
		return
	}
	globalTurnStateTemplates.now = now
}

func (s *turnStateTemplateStore) capture(cfg turnStateTemplateConfig, policy turnStateLengthPolicy, accountID int64, model string, values ...string) bool {
	if !cfg.Enabled || accountID <= 0 {
		return false
	}
	model = strings.TrimSpace(model)
	if model == "" || len(values) != 1 {
		return false
	}
	value := values[0]
	if value == "" {
		return false
	}
	tok, err := parseTurnStateToken(value)
	if err != nil {
		// Parse failure / invalid envelope → never store, never fallback issued=now.
		return false
	}
	now := s.now()
	if !turnStateTokenAccept(tok, now, cfg.TTL, policy.TemplateBlocks) {
		return false
	}
	key := turnStateTemplateKey{AccountID: accountID, Model: model}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(cfg, now)
	if _, exists := s.entryLocked(key); !exists {
		s.evictOldestLocked(cfg.MaxEntries)
	}
	prev, _ := s.entryLocked(key)
	if prev.IssuedAt.After(tok.Issued) {
		return false
	}
	return s.saveEntryLocked(key, turnStateTemplateEntry{Value: tok.Value, IssuedAt: tok.Issued, Strikes: 0})
}

func (s *turnStateTemplateStore) lookup(cfg turnStateTemplateConfig, policy turnStateLengthPolicy, accountID int64, model string) (string, bool) {
	if !cfg.Enabled || accountID <= 0 {
		return "", false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return "", false
	}
	key := turnStateTemplateKey{AccountID: accountID, Model: model}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeExpiredLocked(cfg, now)
	entry, ok := s.entryLocked(key)
	if !ok {
		return "", false
	}
	tok, err := parseTurnStateToken(entry.Value)
	if err != nil || !turnStateTokenAccept(tok, now, cfg.TTL, policy.TemplateBlocks) {
		s.deleteEntryLocked(key)
		return "", false
	}
	return entry.Value, true
}

func (s *turnStateTemplateStore) observePostInject(cfg turnStateTemplateConfig, policy turnStateLengthPolicy, accountID int64, model, responseValue string) (cleared bool, strikes int) {
	if !cfg.Enabled || accountID <= 0 {
		return false, 0
	}
	model = strings.TrimSpace(model)
	if model == "" || strings.TrimSpace(responseValue) == "" {
		return false, 0
	}
	key := turnStateTemplateKey{AccountID: accountID, Model: model}
	now := s.now()
	tok, err := parseTurnStateToken(responseValue)
	suspect := err != nil || !turnStateTokenAccept(tok, now, cfg.TTL, policy.TemplateBlocks)

	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entryLocked(key)
	if !ok {
		return false, 0
	}
	if !suspect {
		entry.Strikes = 0
		s.saveEntryLocked(key, entry)
		return false, 0
	}
	// A response shape alone does not revoke a still-live signed template.
	// Keep the template until its original TTL; count failed recovery observations.
	entry.Strikes = min(turnStateStrikeThreshold, entry.Strikes+1)
	strikes = entry.Strikes

	s.saveEntryLocked(key, entry)
	return false, strikes
}

func (s *turnStateTemplateStore) clearAccount(accountID int64) {
	if accountID <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		ctx, cancel := templateDBContext()
		defer cancel()
		if s.db.DeleteCodexTurnStateTemplate(ctx, accountID, "") != nil {
			log.Print("[codex-turn-state] database account clear failed")
		}
	}
	for key := range s.entries {
		if key.AccountID == accountID {
			s.deleteEntryLocked(key)
		}
	}
	for key := range s.observations {
		if key.AccountID == accountID {
			delete(s.observations, key)
		}
	}
}

func (s *turnStateTemplateStore) clearKey(accountID int64, model string) {
	if accountID <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteEntryLocked(turnStateTemplateKey{AccountID: accountID, Model: strings.TrimSpace(model)})
}

func (s *turnStateTemplateStore) purgeExpiredLocked(cfg turnStateTemplateConfig, now time.Time) {
	for key, entry := range s.entries {
		tok, err := parseTurnStateToken(entry.Value)
		// Drop only on parse failure or timestamp/TTL invalidity. Block-policy
		// mismatches stay for lookup/capture/strike — a personal caller must not
		// purge a valid team-length template (and vice versa).
		if err != nil || !turnStateTokenTimeValid(tok, now, cfg.TTL) {
			s.deleteEntryLocked(key)
		}
	}
}

func (s *turnStateTemplateStore) evictOldestLocked(maxEntries int) {
	if s.db != nil {
		ctx, cancel := templateDBContext()
		defer cancel()
		cfg := loadTurnStateTemplateConfig()
		if s.db.PruneCodexTurnStateTemplates(ctx, s.now().Add(-cfg.TTL+turnStateAcceptSkew).Unix(), max(0, maxEntries-1)) != nil {
			log.Print("[codex-turn-state] database prune failed")
		}
		return
	}
	if maxEntries < 1 {
		maxEntries = 1
	}
	for len(s.entries) >= maxEntries {
		var oldestKey turnStateTemplateKey
		var oldest turnStateTemplateEntry
		first := true
		for key, entry := range s.entries {
			if first || entry.IssuedAt.Before(oldest.IssuedAt) ||
				(entry.IssuedAt.Equal(oldest.IssuedAt) && (key.AccountID < oldestKey.AccountID ||
					(key.AccountID == oldestKey.AccountID && key.Model < oldestKey.Model))) {
				oldestKey = key
				oldest = entry
				first = false
			}
		}
		delete(s.entries, oldestKey)
	}
}

func (s *turnStateTemplateStore) lenForTest() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func (s *turnStateTemplateStore) strikesForTest(accountID int64, model string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, _ := s.entryLocked(turnStateTemplateKey{AccountID: accountID, Model: model})
	return entry.Strikes
}

func accountEligibleForTurnStateTemplate(account *auth.Account) bool {
	if account == nil || account.ID() <= 0 {
		return false
	}
	// Codex ChatGPT OAuth / Agent Identity only — skip OpenAI Responses / other relays.
	if account.IsRelayStyle() || account.IsOpenAIResponsesAPI() {
		return false
	}
	return true
}

// CaptureCodexTurnStateTemplate stores an upstream-minted template when enabled
// and the sole header value Accept(template policy). Never stores replace/degraded
// shapes. Never harvest client request headers. When this request rewrote outbound
// state, observes response shape; a failed observation does not extend or revoke TTL.
func CaptureCodexTurnStateTemplate(ctx context.Context, account *auth.Account, model string, headers http.Header) {
	cfg := loadTurnStateTemplateConfig()
	if !accountEligibleForTurnStateTemplate(account) || headers == nil || strings.TrimSpace(model) == "" {
		return
	}
	if !CodexTurnStateModelAllowed(account, model) {
		return
	}
	policy := cfg.policyFor(account)
	values := headers.Values(codexTurnStateHeader)
	trimmed := make([]string, 0, len(values))
	for _, v := range values {
		if t := strings.TrimSpace(v); t != "" {
			trimmed = append(trimmed, t)
		}
	}
	if len(trimmed) == 0 {
		return
	}
	if probe := turnStateRefreshFromContext(ctx); probe != nil {
		probe.observe(account, model, trimmed)
		return
	}
	responseValue := trimmed[0]
	if len(trimmed) != 1 {
		responseValue = "ambiguous"
	}
	cleared := false
	if turnStateTemplateRewrittenFromContext(ctx) {
		var strikes int
		cleared, strikes = globalTurnStateTemplates.observePostInject(cfg, policy, account.ID(), model, responseValue)
		if cfg.LogDecisions {
			if cleared {
				logTurnStateTemplateDecision(cfg, "strike-clear", account.ID(), model, len(responseValue),
					"post-inject shape failed Accept; cleared after "+strconv.Itoa(strikes)+" strikes")
			} else if strikes > 0 {
				logTurnStateTemplateDecision(cfg, "strike", account.ID(), model, len(responseValue),
					"post-inject shape failed Accept; strikes="+strconv.Itoa(strikes))
			}
		}
	}
	globalTurnStateTemplates.observeStatus(cfg, account, model, responseValue, cleared)
	stored := globalTurnStateTemplates.capture(cfg, policy, account.ID(), model, trimmed...)
	if stored {
		logTurnStateTemplateDecision(cfg, "harvest", account.ID(), model, len(trimmed[0]), "template stored")
		return
	}
	if cfg.LogDecisions && len(trimmed) == 1 {
		if tok, err := parseTurnStateToken(trimmed[0]); err == nil && tok.Blocks == policy.ReplaceBlocks {
			logTurnStateTemplateDecision(cfg, "skip", account.ID(), model, len(trimmed[0]),
				"upstream issued degraded state (blocks=replace)")
		}
	}
}

// ClearCodexTurnStateTemplatesForAccount drops cached templates for a DBID.
func ClearCodexTurnStateTemplatesForAccount(accountID int64) {
	globalTurnStateTemplates.clearAccount(accountID)
}

func decideCodexTurnStateHeader(cfg turnStateTemplateConfig, policy turnStateLengthPolicy, inbound, tmpl string, haveTmpl bool) (decision, reason, replacement string) {
	inboundTok, inboundErr := parseTurnStateToken(inbound)
	parsed := inboundErr == nil
	isDegraded := parsed && inboundTok.Blocks == policy.ReplaceBlocks
	if haveTmpl && inbound != tmpl {
		if cfg.injectAlways() {
			return "inject", injectTurnStateReason(inbound, parsed, isDegraded, policy), tmpl
		}
		if isDegraded {
			return "substitute", "inbound blocks=replace", tmpl
		}
	}
	switch {
	case haveTmpl && inbound == tmpl:
		return "pass", "header already current", ""
	case isDegraded && !haveTmpl:
		return "pass", "no live template for bucket", ""
	default:
		return "pass", "nothing to do", ""
	}
}

func injectTurnStateReason(value string, parsed, degraded bool, policy turnStateLengthPolicy) string {
	switch {
	case value == "":
		return "added (request carried no state)"
	case degraded:
		return "replaced degraded state"
	case parsed:
		return "replaced non-template state (blocks)"
	default:
		return "replaced non-template state (len " + strconv.Itoa(len(value)) + ")"
	}
}

// ApplyCodexTurnStateTemplate rewrites outbound X-Codex-Turn-State from the
// selected account's cached template. Call AFTER guardCodexTurnStateEcho.
// Clear then Set to avoid duplicate casings. Never logs the state value.
// ctx carries usage-log audit (turn-state decision/lengths); nil ctx skips audit.
func ApplyCodexTurnStateTemplate(ctx context.Context, headers http.Header, account *auth.Account, model string) {
	cfg := loadTurnStateTemplateConfig()
	if !cfg.Enabled || headers == nil || !CodexTurnStateInjectionEnabled(account) {
		return
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	if !CodexTurnStateModelAllowed(account, model) {
		return
	}
	if applyTurnStateRefreshHeader(ctx, headers, account, model) {
		return
	}
	if ctx != nil && ctx.Value(turnStateAdminProbeKey{}) == true {
		cfg.InjectMode = turnStateInjectAlways
	}
	policy := cfg.policyFor(account)
	inbound := strings.TrimSpace(headers.Get(codexTurnStateHeader))
	tmpl, ok := globalTurnStateTemplates.lookup(cfg, policy, account.ID(), model)
	decision, reason, replacement := decideCodexTurnStateHeader(cfg, policy, inbound, tmpl, ok)
	outboundLen := len(inbound)
	rewritten := false
	if replacement != "" && !cfg.DryRun {
		headers.Del(codexTurnStateHeader)
		headers.Set(codexTurnStateHeader, replacement)
		outboundLen = len(replacement)
		rewritten = decision == "substitute" || decision == "inject"
	} else if replacement != "" && cfg.DryRun {
		outboundLen = len(replacement)
	}
	recordTurnStateTemplateAudit(ctx, decision, len(inbound), outboundLen, rewritten)
	if audit := turnStateTemplateAuditFromContext(ctx); audit != nil {
		audit.mu.Lock()
		audit.replacement = replacement
		audit.mu.Unlock()
	}
	logTurnStateTemplateDecision(cfg, decision, account.ID(), model, len(inbound), reason)
}

func logTurnStateTemplateDecision(cfg turnStateTemplateConfig, decision string, accountID int64, model string, length int, reason string) {
	if !cfg.LogDecisions {
		return
	}
	// NEVER log the state value — decision + account id + model + lengths only.
	log.Printf("[codex-turn-state] %s account=%d model=%q len=%d (%s)", decision, accountID, model, length, reason)
}

// ==================== usage-log turn-state audit ====================

type turnStateTemplateAuditContextKey struct{}

type turnStateTemplateAudit struct {
	mu          sync.Mutex
	decision    string
	inboundLen  int
	outboundLen int
	rewritten   bool
	recorded    bool
	replacement string
}

func withTurnStateTemplateAudit(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if turnStateTemplateAuditFromContext(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, turnStateTemplateAuditContextKey{}, &turnStateTemplateAudit{})
}

// replaceTurnStateTemplateAudit always installs a fresh audit slot. Use at the
// start of each Responses WebSocket turn so multi-turn reuse of the same
// request context cannot inherit a prior turn's override metadata.
func replaceTurnStateTemplateAudit(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, turnStateTemplateAuditContextKey{}, &turnStateTemplateAudit{})
}

func turnStateTemplateAuditFromContext(ctx context.Context) *turnStateTemplateAudit {
	if ctx == nil {
		return nil
	}
	audit, _ := ctx.Value(turnStateTemplateAuditContextKey{}).(*turnStateTemplateAudit)
	return audit
}

func turnStateTemplateRewrittenFromContext(ctx context.Context) bool {
	audit := turnStateTemplateAuditFromContext(ctx)
	if audit == nil {
		return false
	}
	audit.mu.Lock()
	defer audit.mu.Unlock()
	return audit.recorded && audit.rewritten
}

func attachTurnStateTemplateAudit(c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}
	c.Request = c.Request.WithContext(withTurnStateTemplateAudit(c.Request.Context()))
}

// attachFreshTurnStateTemplateAudit replaces any existing audit on the request
// context. Intended for per-turn Responses WebSocket handling.
func attachFreshTurnStateTemplateAudit(c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}
	c.Request = c.Request.WithContext(replaceTurnStateTemplateAudit(c.Request.Context()))
}

func recordTurnStateTemplateAudit(ctx context.Context, decision string, inboundLen, outboundLen int, rewritten bool) {
	audit := turnStateTemplateAuditFromContext(ctx)
	if audit == nil {
		return
	}
	audit.mu.Lock()
	defer audit.mu.Unlock()
	// Repeated header assembly may see the already substituted value. Keep
	// this attempt's rewrite mark; Begin resets it before failover/retry.
	if audit.recorded && decision == "pass" &&
		(audit.decision == "substitute" || audit.decision == "inject") {
		return
	}
	audit.decision = decision
	audit.inboundLen = inboundLen
	audit.outboundLen = outboundLen
	audit.rewritten = rewritten
	audit.recorded = true
}

func populateTurnStateTemplateMetaFromRequest(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || c.Request == nil || input == nil {
		return
	}
	audit := turnStateTemplateAuditFromContext(c.Request.Context())
	if audit == nil {
		return
	}
	audit.mu.Lock()
	defer audit.mu.Unlock()
	if !audit.recorded {
		return
	}
	// Only annotate when a rewrite was decided (substitute/inject). Pass stays blank
	// so the Usage table mirrors UA: silence unless something changed.
	if audit.decision != "substitute" && audit.decision != "inject" {
		return
	}
	input.TurnStateOverridden = audit.rewritten
	note := ""
	switch audit.decision {
	case "substitute":
		note = strconv.Itoa(audit.inboundLen) + "→" + strconv.Itoa(audit.outboundLen)
	case "inject":
		if audit.inboundLen == 0 {
			note = "inject"
		} else {
			note = "inject " + strconv.Itoa(audit.inboundLen) + "→" + strconv.Itoa(audit.outboundLen)
		}
	}
	input.TurnStateRewriteNote = note
}
