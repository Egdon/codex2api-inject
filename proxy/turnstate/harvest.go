package turnstate

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

const harvestUpstream = "https://chatgpt.com/backend-api/codex/responses"
const harvestClientVersion = "0.153.4"
const harvestUserAgent = "codex-tui/" + harvestClientVersion + " (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; " + harvestClientVersion + ")"

type Store interface {
	Accounts() []*auth.Account
	FindByID(id int64) *auth.Account
}

type JobCell struct {
	AccountID int64  `json:"account_id"`
	Email     string `json:"email,omitempty"`
	Model     string `json:"model"`
	Attempt   int    `json:"attempt"`
	Max       int    `json:"max"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Phase     string `json:"phase"`
	Active    bool   `json:"active"`
	Region    string `json:"region,omitempty"`
	Provider  string `json:"provider"`
}

type Job struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	AccountID      int64     `json:"account_id,omitempty"`
	AccountIDs     []int64   `json:"account_ids,omitempty"`
	Model          string    `json:"model,omitempty"`
	Status         string    `json:"status"`
	Done           int       `json:"done"`
	Total          int       `json:"total"`
	Message        string    `json:"message"`
	Cells          []JobCell `json:"cells,omitempty"`
	Cancel         bool      `json:"-"`
	FinishedUnix   int64     `json:"finished_unix"`
	Succeeded      int       `json:"succeeded"`
	Exhausted      int       `json:"exhausted"`
	Skipped        int       `json:"skipped"`
	CancelledCount int       `json:"cancelled_count"`
}

type Harvester struct {
	db    *database.DB
	store Store
	cache *Cache

	policyMu     sync.Mutex
	policyStates map[int64]database.AstraPolicyState
	policyLoaded time.Time
	policyErrors map[int64]string
	policyError  bool
	mu           sync.Mutex
	job          *Job
	scheduler    *harvestScheduler
	rootCtx      context.Context
	startOnce    sync.Once
	autoWake     chan struct{}
	breaker      harvestBreaker
	// publishMu serializes generation checks and the complete memory/DB publish.
	publishMu   sync.Mutex
	generations map[string]*cellGeneration
	tlsSessions tls.ClientSessionCache
	probeFn     func(ctx context.Context, cfg Config, acc *auth.Account, model, inject string) (string, http.Header, error)
}

func NewHarvester(db *database.DB, store Store, cache *Cache) *Harvester {
	return &Harvester{autoWake: make(chan struct{}, 1), db: db, store: store, cache: cache, generations: make(map[string]*cellGeneration), tlsSessions: tls.NewLRUClientSessionCache(64)}
}

func (h *Harvester) LoadFromDB(ctx context.Context) error {
	if h.db == nil {
		return nil
	}
	raw, err := h.db.LoadTurnStateConfig(ctx)
	if err != nil {
		return err
	}
	cfg, err := ParseConfigJSON(raw)
	if err != nil {
		return err
	}
	SetConfig(cfg)
	rows, err := h.db.ListTurnStateTickets(ctx)
	if err != nil {
		return err
	}
	tickets := make([]CachedTicket, 0, len(rows))
	for _, row := range rows {
		tickets = append(tickets, cachedFromRow(row))
	}
	h.cache.ReplaceAll(tickets)
	return nil
}

func cachedFromRow(row database.TurnStateTicket) CachedTicket {
	harvest := int64(0)
	if !row.LastHarvestAt.IsZero() {
		harvest = row.LastHarvestAt.Unix()
	}
	return CachedTicket{
		AccountID:      row.AccountID,
		Model:          row.Model,
		Token:          row.Token,
		IssuedUnix:     row.IssuedUnix,
		Length:         row.Length,
		Blocks:         row.Blocks,
		ConfirmWarning: row.ConfirmWarning,
		Exhausted:      row.Exhausted,
		Attempts:       row.Attempts,
		LastError:      row.LastError,
		LastHarvestAt:  harvest,
		CooldownUntil:  row.CooldownUntil,
	}
}

func (h *Harvester) persistTicket(ctx context.Context, t CachedTicket) {
	h.cache.Put(t)
	if h.db == nil {
		return
	}
	row := database.TurnStateTicket{
		AccountID:      t.AccountID,
		Model:          t.Model,
		Token:          t.Token,
		IssuedUnix:     t.IssuedUnix,
		Length:         t.Length,
		Blocks:         t.Blocks,
		ConfirmWarning: t.ConfirmWarning,
		Exhausted:      t.Exhausted,
		Attempts:       t.Attempts,
		LastError:      t.LastError,
		CooldownUntil:  t.CooldownUntil,
	}
	if t.LastHarvestAt > 0 {
		row.LastHarvestAt = time.Unix(t.LastHarvestAt, 0).UTC()
	}
	if err := h.db.UpsertTurnStateTicket(ctx, row); err != nil {
		log.Printf("[turn-state] ticket persistence failed for account %d", t.AccountID)
	}
}

func (h *Harvester) deleteTicket(ctx context.Context, accountID int64, model string) error {
	h.cache.Delete(accountID, model)
	if h.db == nil {
		return nil
	}
	return h.db.DeleteTurnStateTicket(ctx, accountID, model)
}

type harvestCell struct {
	acc   *auth.Account
	model string
}

func (h *Harvester) harvestAccounts(accountID int64) []*auth.Account {
	cfg := GetConfig()
	var out []*auth.Account
	if accountID > 0 {
		if acc := h.store.FindByID(accountID); acc != nil {
			out = append(out, acc)
		}
		return filterHarvestAccounts(out, cfg)
	}
	return filterHarvestAccounts(h.store.Accounts(), cfg)
}

func (h *Harvester) selectedAccounts(ids []int64) []*auth.Account {
	cfg := GetConfig()
	accounts := make([]*auth.Account, 0, len(ids))
	for _, id := range ids {
		if acc := h.store.FindByID(id); acc != nil {
			accounts = append(accounts, acc)
		}
	}
	return filterHarvestAccounts(accounts, cfg)
}

func filterHarvestAccounts(accounts []*auth.Account, cfg Config) []*auth.Account {
	out := make([]*auth.Account, 0, len(accounts))
	for _, acc := range accounts {
		if acc == nil || acc.ID() <= 0 {
			continue
		}
		if atomic.LoadInt32(&acc.Disabled) != 0 || atomic.LoadInt32(&acc.Locked) != 0 {
			continue
		}
		if acc.IsCodexAgentIdentity() {
			continue
		}
		acc.Mu().RLock()
		upstream := strings.TrimSpace(acc.UpstreamType)
		accountID := strings.TrimSpace(acc.AccountID)
		refresh := strings.TrimSpace(acc.RefreshToken)
		acc.Mu().RUnlock()
		if strings.EqualFold(upstream, auth.UpstreamGrok) ||
			strings.EqualFold(upstream, auth.UpstreamClaude) ||
			strings.EqualFold(upstream, auth.UpstreamAntigravity) ||
			strings.EqualFold(upstream, auth.UpstreamOpenAIResponses) {
			continue
		}
		if strings.TrimSpace(acc.GetAccessToken()) == "" || accountID == "" || refresh == "" {
			continue
		}
		if !AccountHarvestEnabled(cfg, acc.ID()) {
			continue
		}
		out = append(out, acc)
	}
	return out
}

func (h *Harvester) dueCells(accounts []*auth.Account, cfg Config, onlyModel string, force bool) []harvestCell {
	return h.dueCellsBounded(accounts, cfg, onlyModel, force, 0)
}

func (h *Harvester) dueCellsBounded(accounts []*auth.Account, cfg Config, onlyModel string, force bool, limit int) []harvestCell {
	now := time.Now().Unix()
	skip := int64(cfg.SkipTTLMinutes) * 60
	onlyModel = strings.TrimSpace(onlyModel)
	out := make([]harvestCell, 0)
	for _, acc := range accounts {
		for _, model := range cfg.Models {
			if onlyModel != "" && !strings.EqualFold(model, onlyModel) {
				continue
			}
			policyRecovery := cfg.AstraPolicyEnabled && strings.EqualFold(model, astraModel) && h.PolicySnapshot(acc.ID()).Demoted && h.automaticPolicyAllowed(acc.ID(), model, cfg)
			if !force && !policyRecovery {
				if t, ok := h.cache.Get(acc.ID(), model); ok {
					if remaining := RemainingSeconds(t.IssuedUnix, now); remaining > skip && t.Length == FullBloodChars && !t.Exhausted {
						continue
					}
					if t.CooldownUntil > now {
						continue
					}
				}

			}
			out = append(out, harvestCell{acc: acc, model: model})
			if limit > 0 && len(out) >= limit {
				return out
			}
		}
	}
	return out
}

// setCooldown is retained for existing package tests; production cooldowns are
// published together with the ticket, never in a separate unbounded map.
func (h *Harvester) setCooldown(accountID int64, model string, until time.Time) {
	h.publishMu.Lock()
	defer h.publishMu.Unlock()
	t, _ := h.cache.Get(accountID, model)
	t.AccountID, t.Model, t.CooldownUntil = accountID, model, until.Unix()
	h.cache.Put(t)
}

func emailOrID(email string, id int64) string {
	if strings.TrimSpace(email) != "" {
		return strings.TrimSpace(email)
	}
	return fmt.Sprintf("#%d", id)
}

func accountEmail(acc *auth.Account) string {
	if acc == nil {
		return ""
	}
	acc.Mu().RLock()
	defer acc.Mu().RUnlock()
	return strings.TrimSpace(acc.Email)
}

func isUnauthorized(err error) bool {
	return strings.Contains(err.Error(), "status 401")
}

func overloadedStatus(h http.Header) bool {
	if h == nil {
		return false
	}
	return strings.Contains(strings.ToLower(h.Get("X-Codex-Error")+" "+h.Get("Openai-Error")), "overload")
}

func randomSID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func (h *Harvester) probe(ctx context.Context, cfg Config, acc *auth.Account, model, inject string) (string, http.Header, error) {
	if h.probeFn != nil {
		return h.probeFn(ctx, cfg, acc, model, inject)
	}
	cfg = NormalizeConfig(cfg)
	proxyURL, err := harvestProxyURL(cfg)
	if err != nil {
		return "", nil, err
	}
	transport := &http.Transport{
		OnProxyConnectResponse: func(_ context.Context, _ *url.URL, _ *http.Request, resp *http.Response) error {
			return proxyConnectFailure(cfg.HarvestProxyProvider, resp)
		},
		Proxy:                 http.ProxyURL(proxyURL),
		DialContext:           (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 15 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, ClientSessionCache: h.tlsSessions},
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       10 * time.Second,
		MaxIdleConns:          1,
		MaxConnsPerHost:       1,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	body := fmt.Sprintf(`{"model":%q,"instructions":"Reply with exactly: pong","store":false,"stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ping"}]}]}`, model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, harvestUpstream, strings.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+acc.GetAccessToken())
	req.Header.Set("Chatgpt-Account-Id", h.probeAccountID(acc))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", harvestUserAgent)
	req.Header.Set("Version", harvestClientVersion)
	req.Header.Set("Originator", "codex-tui")
	req.Header.Set("X-Codex-Beta-Features", "remote_compaction_v2")
	if inject != "" {
		req.Header.Set("X-Codex-Turn-State", inject)
	}
	resp, err := client.Do(req)
	if err != nil {
		var failure *probeFailure
		if errors.As(err, &failure) {
			return "", nil, failure
		}
		return "", nil, &probeFailure{kind: probeNetwork}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusProxyAuthRequired {
		return "", resp.Header.Clone(), &probeFailure{kind: probeProxyAuth}
	}
	token := strings.TrimSpace(resp.Header.Get("X-Codex-Turn-State"))
	if resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
		return token, resp.Header.Clone(), fmt.Errorf("status 401")
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return token, resp.Header.Clone(), &probeFailure{kind: probeHTTP, status: resp.StatusCode, retryAfter: boundedRetryAfter(resp.Header)}
	}
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		low := strings.ToLower(string(snippet))
		if strings.Contains(low, "overload") || strings.Contains(low, "server_is_overloaded") {
			return token, resp.Header.Clone(), &probeFailure{kind: probeOverload, status: resp.StatusCode, retryAfter: boundedRetryAfter(resp.Header)}
		}
		if strings.Contains(low, "encrypted content") {
			return token, resp.Header.Clone(), fmt.Errorf("decrypt: %s", resp.Status)
		}
		return token, resp.Header.Clone(), &probeFailure{kind: probeHTTP, status: resp.StatusCode, retryAfter: boundedRetryAfter(resp.Header)}
	}
	// Abort as soon as headers are in; optionally drain one line so the
	// connection closes cleanly without waiting for the model.
	_ = resp.Body.Close()
	if token == "" && inject == "" {
		return "", resp.Header.Clone(), fmt.Errorf("empty turn-state")
	}
	if inject != "" {
		return inject, resp.Header.Clone(), nil
	}
	return token, resp.Header.Clone(), nil
}

func (h *Harvester) ManualPaste(ctx context.Context, accountID int64, model, token string) (CachedTicket, error) {
	token = NormalizeToken(token)
	if token == "" {
		return CachedTicket{}, fmt.Errorf("empty token")
	}
	parsed, ok := ParseFernet(token)
	t := CachedTicket{
		AccountID:     accountID,
		Model:         model,
		Token:         token,
		LastHarvestAt: time.Now().Unix(),
		Exhausted:     false,
		CooldownUntil: 0,
	}
	if ok {
		t.IssuedUnix = parsed.IssuedUnix
		t.Length = parsed.Length
		t.Blocks = parsed.Blocks
	} else {
		t.Length = len(token)
		t.LastError = "not a Fernet token"
	}
	h.publishMu.Lock()
	defer h.publishMu.Unlock()
	h.invalidateCellLocked(accountID, model)
	h.persistTicket(ctx, t)
	return t, nil
}

func (h *Harvester) Clear(ctx context.Context, accountID int64, model string) error {
	h.publishMu.Lock()
	defer h.publishMu.Unlock()
	h.invalidateCellLocked(accountID, model)
	return h.deleteTicket(ctx, accountID, model)
}

func (h *Harvester) probeAccountID(acc *auth.Account) string {
	if acc == nil {
		return ""
	}
	acc.Mu().RLock()
	defer acc.Mu().RUnlock()
	return strings.TrimSpace(acc.AccountID)
}

// SaveConfig serializes settings writes with participation updates so neither
// endpoint can discard changes from the other.
func (h *Harvester) SaveConfig(ctx context.Context, req Config, keepPassword, keepDisabled bool) (Config, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cfg := GetConfig()
	preserveHarvestPasswords(&req, cfg, keepPassword)
	if keepDisabled {
		req.DisabledAccountIDs = cfg.DisabledAccountIDs
	}
	req = NormalizeConfig(req)
	if err := ValidateHarvestProxyConfig(req); err != nil {
		return Config{}, err
	}
	if h.db == nil {
		return Config{}, fmt.Errorf("数据库不可用")
	}
	req.astraPolicyEpoch = cfg.astraPolicyEpoch
	if astraPolicyEnabled(req) {
		found := false
		for _, model := range req.Models {
			if strings.EqualFold(model, astraModel) {
				found = true
				break
			}
		}
		if !found {
			return Config{}, fmt.Errorf("Astra policy requires gpt-6-astra in configured models")
		}
	}
	if req.AstraPolicyEnabled {
		if err := h.db.ValidateAstraPolicyGroups(ctx, req.AstraFailureGroupID, req.AstraRecoveryGroupID); err != nil {
			return Config{}, err
		}
	}
	if astraPolicySettingsChanged(cfg, req) || (astraPolicyEnabled(req) && cfg.astraPolicyEpoch == 0) {
		req.astraPolicyEpoch = max(time.Now().UnixNano(), cfg.astraPolicyEpoch+1)
	}
	encoded, err := EncodeConfig(req)
	if err != nil {
		return Config{}, err
	}
	if err := h.db.SaveTurnStateConfig(ctx, encoded); err != nil {
		return Config{}, err
	}
	SetConfig(req)
	if req.AutoHarvest && !cfg.AutoHarvest {
		// Nonblocking: scanAuto takes mu after this settings transaction exits.
		h.requestAutoScan()
	}
	return req, nil
}

// SetHarvestParticipationBatch validates all IDs and commits the change once.
func (h *Harvester) SetHarvestParticipationBatch(ctx context.Context, ids []int64, enabled bool) (Config, error) {
	if len(ids) == 0 || len(ids) > 500 {
		return Config{}, fmt.Errorf("请选择 1–500 个账号")
	}
	unique := uniqueIDs(ids)
	if len(unique) != len(ids) {
		return Config{}, fmt.Errorf("账号 ID 必须为正且不能重复")
	}
	for _, id := range unique {
		if h.store.FindByID(id) == nil {
			return Config{}, fmt.Errorf("账号 #%d 不存在", id)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cfg := GetConfig()
	selected := make(map[int64]bool, len(unique))
	for _, id := range unique {
		selected[id] = true
	}
	next := make([]int64, 0, len(cfg.DisabledAccountIDs)+len(unique))
	for _, id := range cfg.DisabledAccountIDs {
		if !selected[id] {
			next = append(next, id)
		}
	}
	if !enabled {
		next = append(next, unique...)
	}
	cfg.DisabledAccountIDs = uniqueIDs(next)
	if h.db == nil {
		return Config{}, fmt.Errorf("数据库不可用")
	}
	encoded, err := EncodeConfig(cfg)
	if err != nil {
		return Config{}, err
	}
	if err := h.db.SaveTurnStateConfig(ctx, encoded); err != nil {
		return Config{}, err
	}
	SetConfig(cfg)
	return cfg, nil
}
