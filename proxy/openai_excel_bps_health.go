package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// Basispoints route health. A BPS 429 cools only this account's Basispoints
// route for Retry-After. A BPS 403 pauses the route — per (account, model) for
// basispoints_model_access_changed, otherwise for the whole account — until a
// background probe shows it recovered. Paused routes send requests straight
// to native Codex; the account's Codex scheduling state is never changed.
const (
	excelBPSAccessURL          = "https://bps.openai.com/basispoints/api/responses/access?include_models=true"
	excelBPSHealthScanInterval = 15 * time.Second
	excelBPSProbeConcurrency   = 3
	excelBPSProbeTimeout       = 45 * time.Second
	excelBPSRateLimitDefault   = time.Minute
	excelBPSRateLimitMin       = time.Second
	excelBPSRateLimitMax       = 10 * time.Minute
	excelBPSHealthNamespace    = "excel_bps_health"
	excelBPSHealthKey          = "account_pauses"
	excelBPSHealthTTL          = 30 * 24 * time.Hour
	excelBPSHealthIOTimeout    = 2 * time.Second

	excelBPSPauseForbidden   = "forbidden"
	excelBPSPauseModelAccess = "model_access"
)

type excelBPSPause struct {
	AccountID int64 `json:"account_id"`
	// Model is set for a model-level pause and empty for an account pause.
	Model string `json:"model,omitempty"`
	// ProbeModel is the model an account-level recovery probe generates with.
	ProbeModel  string    `json:"probe_model,omitempty"`
	Reason      string    `json:"reason"`
	PausedAt    time.Time `json:"paused_at"`
	LastProbeAt time.Time `json:"last_probe_at,omitempty"`
	Failures    int       `json:"failures,omitempty"`
}

func (p *excelBPSPause) nextProbe(interval time.Duration) time.Time {
	last := p.PausedAt
	if p.LastProbeAt.After(last) {
		last = p.LastProbeAt
	}
	return last.Add(interval)
}

// ExcelBPSPauseView is the administrator view of an account's route health.
type ExcelBPSPauseView struct {
	// Scope is "account" (every model) or "models" (only Models).
	Scope            string     `json:"scope,omitempty"`
	Reason           string     `json:"reason,omitempty"`
	Models           []string   `json:"models,omitempty"`
	PausedAt         *time.Time `json:"paused_at,omitempty"`
	LastProbeAt      *time.Time `json:"last_probe_at,omitempty"`
	NextProbeAt      *time.Time `json:"next_probe_at,omitempty"`
	Failures         int        `json:"failures,omitempty"`
	RateLimitedUntil *time.Time `json:"rate_limited_until,omitempty"`
}

type excelBPSAccess struct {
	Allowed bool     `json:"allowed"`
	Models  []string `json:"models"`
}

type excelBPSHealthState struct {
	mu        sync.Mutex
	pauses    map[string]*excelBPSPause
	cooldowns map[int64]time.Time
	accounts  map[int64]*auth.Account
	proxies   map[int64]string
	probing   map[string]bool
	store     *auth.Store
	backing   cache.TokenCache
	persistMu sync.Mutex
	loopOnce  sync.Once
	now       func() time.Time
}

var excelBPSHealth = &excelBPSHealthState{}

// Probe seams for focused tests; nil uses the account's credentials and proxy
// against the real Basispoints endpoints.
var (
	excelBPSAccessProbe     func(context.Context, *auth.Account, string) (excelBPSAccess, error)
	excelBPSGenerationProbe func(context.Context, *auth.Account, string, string) error
)

type excelBPSProbeContextKey struct{}

func excelBPSPauseKey(accountID int64, model string) string {
	return strconv.FormatInt(accountID, 10) + "\x00" + strings.ToLower(strings.TrimSpace(model))
}

func excelBPSPauseEnabled() bool {
	return !CurrentRuntimeSettings().CodexBasispoints403PauseDisabled
}

func excelBPSProbeInterval() time.Duration {
	return time.Duration(CurrentRuntimeSettings().CodexBasispoints403ProbeIntervalMin) * time.Minute
}

func (s *excelBPSHealthState) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *excelBPSHealthState) initLocked() {
	if s.pauses == nil {
		s.pauses = make(map[string]*excelBPSPause)
		s.cooldowns = make(map[int64]time.Time)
		s.accounts = make(map[int64]*auth.Account)
		s.proxies = make(map[int64]string)
		s.probing = make(map[string]bool)
	}
}

// configure attaches the account store and, when shared across instances, the
// runtime cache that keeps account-level pauses across restarts.
func (s *excelBPSHealthState) configure(store *auth.Store, tc cache.TokenCache) {
	s.mu.Lock()
	s.initLocked()
	if store != nil {
		s.store = store
	}
	if tc != nil && tc.SharedAcrossInstances() {
		s.backing = tc
	}
	backing := s.backing
	s.mu.Unlock()
	if backing != nil {
		s.load(backing)
	}
	s.ensureLoop()
}

func (s *excelBPSHealthState) load(backing cache.TokenCache) {
	ctx, cancel := context.WithTimeout(context.Background(), excelBPSHealthIOTimeout)
	defer cancel()
	raw, ok, err := backing.GetRuntime(ctx, excelBPSHealthNamespace, excelBPSHealthKey)
	if err != nil || !ok {
		return
	}
	var saved []excelBPSPause
	if json.Unmarshal(raw, &saved) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range saved {
		pause := saved[i]
		if pause.AccountID <= 0 || pause.Model != "" {
			continue
		}
		key := excelBPSPauseKey(pause.AccountID, "")
		if s.pauses[key] == nil {
			s.pauses[key] = &pause
		}
	}
}

// schedulePersist writes the current account-level pauses without blocking the
// request that changed them. Writes are serialized and always store the state
// current at write time, so the last write reflects the latest change.
func (s *excelBPSHealthState) schedulePersist() {
	s.mu.Lock()
	backing := s.backing
	s.mu.Unlock()
	if backing == nil {
		return
	}
	go func() {
		s.persistMu.Lock()
		defer s.persistMu.Unlock()
		s.mu.Lock()
		saved := make([]excelBPSPause, 0)
		for _, pause := range s.pauses {
			if pause.Model == "" {
				saved = append(saved, *pause)
			}
		}
		s.mu.Unlock()
		raw, err := json.Marshal(saved)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), excelBPSHealthIOTimeout)
		defer cancel()
		if err := backing.SetRuntime(ctx, excelBPSHealthNamespace, excelBPSHealthKey, raw, excelBPSHealthTTL); err != nil {
			log.Printf("[excel-bps] route health store write failed: %v", err)
		}
	}()
}

func (s *excelBPSHealthState) ensureLoop() {
	s.loopOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(excelBPSHealthScanInterval)
			defer ticker.Stop()
			for range ticker.C {
				s.runDue(context.Background())
			}
		}()
	})
}

// blocks reports whether the Basispoints route of account is cooling down or
// paused for model.
func (s *excelBPSHealthState) blocks(account *auth.Account, model string) bool {
	if account == nil {
		return false
	}
	id := account.ID()
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLocked()
	if until, ok := s.cooldowns[id]; ok {
		if now.Before(until) {
			return true
		}
		delete(s.cooldowns, id)
	}
	if !excelBPSPauseEnabled() {
		return false
	}
	return s.pauses[excelBPSPauseKey(id, "")] != nil || s.pauses[excelBPSPauseKey(id, model)] != nil
}

// excelBPSRetryAfter parses Retry-After (seconds or an HTTP date).
func excelBPSRetryAfter(header http.Header, now time.Time) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	wait := excelBPSRateLimitDefault
	if seconds, err := strconv.Atoi(value); err == nil {
		wait = time.Duration(seconds) * time.Second
	} else if at, err := http.ParseTime(value); err == nil {
		wait = at.Sub(now)
	}
	return min(max(wait, excelBPSRateLimitMin), excelBPSRateLimitMax)
}

// observeFailure records the route effect of a Basispoints HTTP rejection.
// It is called before any output was written and never from recovery probes.
func (s *excelBPSHealthState) observeFailure(ctx context.Context, account *auth.Account, model string, status int, code string, header http.Header, proxyURL string) {
	if account == nil || ctx.Value(excelBPSProbeContextKey{}) != nil {
		return
	}
	id := account.ID()
	now := s.clock()
	switch status {
	case http.StatusTooManyRequests:
		until := now.Add(excelBPSRetryAfter(header, now))
		s.mu.Lock()
		s.initLocked()
		if until.After(s.cooldowns[id]) {
			s.cooldowns[id] = until
		}
		s.mu.Unlock()
		log.Printf("[excel-bps] account=%d rate_limited until=%s", id, until.UTC().Format(time.RFC3339))
	case http.StatusForbidden:
		if !excelBPSPauseEnabled() {
			return
		}
		pause := &excelBPSPause{AccountID: id, ProbeModel: model, Reason: excelBPSPauseForbidden, PausedAt: now}
		if code == "basispoints_model_access_changed" {
			pause = &excelBPSPause{AccountID: id, Model: strings.ToLower(strings.TrimSpace(model)), Reason: excelBPSPauseModelAccess, PausedAt: now}
			if pause.Model == "" {
				return
			}
		}
		key := excelBPSPauseKey(id, pause.Model)
		s.mu.Lock()
		s.initLocked()
		existing := s.pauses[key] != nil
		if !existing {
			s.pauses[key] = pause
		}
		s.accounts[id] = account
		s.proxies[id] = proxyURL
		s.mu.Unlock()
		if !existing {
			log.Printf("[excel-bps] account=%d paused scope=%s reason=%s model=%s", id, excelBPSPauseScope(pause), pause.Reason, model)
			if pause.Model == "" {
				s.schedulePersist()
			}
		}
		s.ensureLoop()
	}
}

func excelBPSPauseScope(pause *excelBPSPause) string {
	if pause.Model == "" {
		return "account"
	}
	return "model"
}

// clear removes every pause and cooldown of an account.
func (s *excelBPSHealthState) clear(accountID int64) bool {
	s.mu.Lock()
	s.initLocked()
	cleared := false
	persist := false
	for key, pause := range s.pauses {
		if pause.AccountID == accountID {
			delete(s.pauses, key)
			cleared = true
			persist = persist || pause.Model == ""
		}
	}
	if _, ok := s.cooldowns[accountID]; ok {
		delete(s.cooldowns, accountID)
		cleared = true
	}
	s.mu.Unlock()
	if persist {
		s.schedulePersist()
	}
	return cleared
}

func (s *excelBPSHealthState) view(accountID int64) *ExcelBPSPauseView {
	now := s.clock()
	interval := excelBPSProbeInterval()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLocked()
	view := &ExcelBPSPauseView{}
	found := false
	if until, ok := s.cooldowns[accountID]; ok && now.Before(until) {
		value := until
		view.RateLimitedUntil = &value
		found = true
	}
	if !excelBPSPauseEnabled() {
		if found {
			return view
		}
		return nil
	}
	setTimes := func(pause *excelBPSPause) {
		pausedAt, next := pause.PausedAt, pause.nextProbe(interval)
		view.PausedAt, view.NextProbeAt, view.Failures = &pausedAt, &next, pause.Failures
		if !pause.LastProbeAt.IsZero() {
			last := pause.LastProbeAt
			view.LastProbeAt = &last
		}
	}
	if pause := s.pauses[excelBPSPauseKey(accountID, "")]; pause != nil {
		view.Scope, view.Reason = "account", pause.Reason
		setTimes(pause)
		return view
	}
	var earliest *excelBPSPause
	for _, pause := range s.pauses {
		if pause.AccountID != accountID || pause.Model == "" {
			continue
		}
		view.Models = append(view.Models, pause.Model)
		if earliest == nil || pause.nextProbe(interval).Before(earliest.nextProbe(interval)) {
			earliest = pause
		}
	}
	if earliest != nil {
		sort.Strings(view.Models)
		view.Scope, view.Reason = "models", excelBPSPauseModelAccess
		setTimes(earliest)
		found = true
	}
	if !found {
		return nil
	}
	return view
}

func (s *excelBPSHealthState) resolveAccount(accountID int64) (*auth.Account, string, bool) {
	s.mu.Lock()
	store, account, proxyURL := s.store, s.accounts[accountID], s.proxies[accountID]
	s.mu.Unlock()
	if store != nil {
		live := store.FindByID(accountID)
		if live == nil {
			return nil, "", false
		}
		account = live
		proxyURL = store.ResolveProxyForAccount(live)
	}
	return account, proxyURL, account != nil
}

// runDue probes every pause whose interval elapsed, at most
// excelBPSProbeConcurrency at a time. It returns after all started probes end.
func (s *excelBPSHealthState) runDue(ctx context.Context) {
	if !excelBPSPauseEnabled() {
		return
	}
	now := s.clock()
	interval := excelBPSProbeInterval()
	s.mu.Lock()
	s.initLocked()
	var due []string
	for key, pause := range s.pauses {
		if s.probing[key] || now.Before(pause.nextProbe(interval)) {
			continue
		}
		// An account pause covers its models; probe the account first.
		if pause.Model != "" && s.pauses[excelBPSPauseKey(pause.AccountID, "")] != nil {
			continue
		}
		s.probing[key] = true
		due = append(due, key)
	}
	s.mu.Unlock()
	sort.Strings(due)
	slots := make(chan struct{}, excelBPSProbeConcurrency)
	var wg sync.WaitGroup
	for _, key := range due {
		slots <- struct{}{}
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			defer func() { <-slots }()
			s.probe(ctx, key)
		}(key)
	}
	wg.Wait()
}

func (s *excelBPSHealthState) probe(ctx context.Context, key string) {
	defer func() {
		s.mu.Lock()
		delete(s.probing, key)
		s.mu.Unlock()
	}()
	s.mu.Lock()
	current := s.pauses[key]
	var pause excelBPSPause
	if current != nil {
		pause = *current
	}
	s.mu.Unlock()
	if current == nil {
		return
	}
	account, proxyURL, ok := s.resolveAccount(pause.AccountID)
	if !ok || !account.IsExcelBPSEnabled() {
		// Deleted, no longer eligible, or no longer configured for Basispoints.
		s.mu.Lock()
		if s.pauses[key] == current {
			delete(s.pauses, key)
		}
		s.mu.Unlock()
		if pause.Model == "" {
			s.schedulePersist()
		}
		return
	}
	probeCtx, cancel := context.WithTimeout(context.WithValue(ctx, excelBPSProbeContextKey{}, true), excelBPSProbeTimeout)
	defer cancel()
	err := s.checkRecovered(probeCtx, account, proxyURL, &pause)
	now := s.clock()
	s.mu.Lock()
	if s.pauses[key] != current {
		// Cleared or replaced while probing.
		s.mu.Unlock()
		return
	}
	if err == nil {
		delete(s.pauses, key)
	} else {
		current.LastProbeAt = now
		current.Failures++
	}
	s.mu.Unlock()
	if err == nil {
		log.Printf("[excel-bps] account=%d resumed scope=%s reason=%s", pause.AccountID, excelBPSPauseScope(&pause), pause.Reason)
	} else {
		log.Printf("[excel-bps] account=%d probe_failed scope=%s reason=%s failures=%d: %v", pause.AccountID, excelBPSPauseScope(&pause), pause.Reason, pause.Failures+1, err)
	}
	if pause.Model == "" {
		s.schedulePersist()
	}
}

func (s *excelBPSHealthState) checkRecovered(ctx context.Context, account *auth.Account, proxyURL string, pause *excelBPSPause) error {
	accessProbe, generationProbe := excelBPSAccessProbe, excelBPSGenerationProbe
	if accessProbe == nil {
		accessProbe = probeExcelBPSAccess
	}
	if generationProbe == nil {
		generationProbe = probeExcelBPSGeneration
	}
	access, err := accessProbe(ctx, account, proxyURL)
	if err != nil {
		return err
	}
	if !access.Allowed {
		return errors.New("Basispoints access is not allowed")
	}
	if pause.Model != "" {
		for _, model := range access.Models {
			if strings.EqualFold(strings.TrimSpace(model), pause.Model) {
				return nil
			}
		}
		return errors.New("model is not in the Basispoints access list")
	}
	model := pause.ProbeModel
	if model == "" && len(access.Models) > 0 {
		model = access.Models[0]
	}
	if model == "" {
		return errors.New("no Basispoints model is available for a recovery probe")
	}
	return generationProbe(ctx, account, proxyURL, model)
}

func probeExcelBPSAccess(ctx context.Context, account *auth.Account, proxyURL string) (excelBPSAccess, error) {
	token := account.GetAccessToken()
	accountID := excelBPSAccountID(account, token)
	if token == "" || accountID == "" {
		return excelBPSAccess{}, errors.New("account Basispoints identity is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, excelBPSAccessURL, nil)
	if err != nil {
		return excelBPSAccess{}, err
	}
	setExcelBPSHeaders(req, token, accountID)
	req.Header.Set("Accept", "application/json")
	resp, err := excelBPSDo(req, account, proxyURL)
	if err != nil {
		return excelBPSAccess{}, errors.New("Basispoints access request failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return excelBPSAccess{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return excelBPSAccess{}, fmt.Errorf("Basispoints access returned HTTP %d", resp.StatusCode)
	}
	var access excelBPSAccess
	if err := json.Unmarshal(raw, &access); err != nil {
		return excelBPSAccess{}, errors.New("Basispoints access response is invalid")
	}
	return access, nil
}

// probeExcelBPSGeneration sends one minimal generation and requires the exact
// nonce back. It never replays a user request and uses a memory-only replay.
func probeExcelBPSGeneration(ctx context.Context, account *auth.Account, proxyURL, model string) error {
	nonce := "bps-probe-" + uuid.NewString()
	raw, err := json.Marshal(map[string]any{
		"model": model, "stream": true, "reasoning": map[string]any{"effort": "low"},
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "Reply with exactly this token and nothing else: " + nonce}},
	})
	if err != nil {
		return err
	}
	upstream, err := prepareExcelBPSUpstream(ctx, account, raw, "bps-health-probe:"+nonce, "", proxyURL, false, false)
	if err != nil {
		return err
	}
	defer upstream.response.Body.Close()
	converted := upstream.bridge.Stream(upstream.response.Body)
	defer converted.Close()
	var answer strings.Builder
	completed := false
	readErr := ReadSSEStreamWithEvent(converted, func(_ string, data []byte) bool {
		switch gjson.GetBytes(data, "type").String() {
		case "response.output_text.delta":
			answer.WriteString(gjson.GetBytes(data, "delta").String())
		case "response.completed":
			completed = true
			return false
		case "response.failed", "response.incomplete", "error":
			return false
		}
		return true
	})
	if readErr != nil {
		return readErr
	}
	if !completed {
		return errors.New("Basispoints recovery probe did not complete")
	}
	if strings.TrimSpace(answer.String()) != nonce {
		return errors.New("Basispoints recovery probe returned a different answer")
	}
	return nil
}

// ClearExcelBPSPause removes every Basispoints pause and cooldown of an account.
func ClearExcelBPSPause(accountID int64) bool {
	return excelBPSHealth.clear(accountID)
}

// ExcelBPSPauseFor returns the administrator view of an account's Basispoints
// route health, or nil when the route is healthy.
func ExcelBPSPauseFor(accountID int64) *ExcelBPSPauseView {
	return excelBPSHealth.view(accountID)
}
