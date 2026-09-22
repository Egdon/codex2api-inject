package proxy

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codex2api/auth"
)

// Status contains only upstream shape observations, never the opaque state value.
// It is a heuristic, not an assessment of the model's reasoning ability.
type CodexTurnStateStatus struct {
	InjectionEnabled bool                        `json:"injection_enabled"`
	State            string                      `json:"state"`
	Mode             string                      `json:"mode"`
	TemplateLength   int                         `json:"template_length"`
	ReplaceLength    int                         `json:"replace_length"`
	Models           []CodexTurnStateModelStatus `json:"models"`
}

type CodexTurnStateModelStatus struct {
	Model             string     `json:"model"`
	State             string     `json:"state"`
	Length            int        `json:"length"`
	Consecutive       int        `json:"consecutive"`
	ObservedAt        time.Time  `json:"observed_at"`
	TemplateCached    bool       `json:"template_cached"`
	TemplateExpiresAt *time.Time `json:"template_expires_at,omitempty"`
}

type turnStateObservation struct {
	Class      string
	Count      int
	Length     int
	ObservedAt time.Time
	Recovering bool
	Mode       string
}

// Badge thresholds follow account type, independently of the operator's cache
// mode override. Unknown workspace types fall back to the configured mode.
func turnStateStatusPolicy(cfg turnStateTemplateConfig, account *auth.Account) turnStateLengthPolicy {
	if mode := turnStatePlanHint(account); mode != "" {
		cfg.AccountMode = mode
	}
	cfg.ForceTemplateBlocks, cfg.ForceReplaceBlocks = 0, 0
	return cfg.policyFor(account)
}

func (s *turnStateTemplateStore) purgeObservationsLocked(cfg turnStateTemplateConfig, now time.Time) {
	for key, observation := range s.observations {
		if !now.Before(observation.ObservedAt.Add(cfg.TTL)) {
			delete(s.observations, key)
		}
	}
}

func (s *turnStateTemplateStore) makeObservationRoomLocked(cfg turnStateTemplateConfig, key turnStateTemplateKey, now time.Time) {
	s.purgeObservationsLocked(cfg, now)
	if _, ok := s.observations[key]; ok {
		return
	}
	for len(s.observations) >= max(1, cfg.MaxEntries) {
		var oldestKey turnStateTemplateKey
		var oldest time.Time
		for k, value := range s.observations {
			if oldest.IsZero() || value.ObservedAt.Before(oldest) {
				oldestKey, oldest = k, value.ObservedAt
			}
		}
		delete(s.observations, oldestKey)
	}
}

func (s *turnStateTemplateStore) observeStatus(cfg turnStateTemplateConfig, account *auth.Account, model, value string, cleared bool) {
	policy := turnStateStatusPolicy(cfg, account)
	key := turnStateTemplateKey{AccountID: account.ID(), Model: strings.TrimSpace(model)}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.makeObservationRoomLocked(cfg, key, now)
	class := "unknown"
	if token, err := parseTurnStateToken(value); err == nil && turnStateTokenTimeValid(token, now, cfg.TTL) {
		switch token.Blocks {
		case policy.TemplateBlocks:
			class = "healthy"
		case policy.ReplaceBlocks:
			class = "degraded"
		}
	}
	observation := s.observations[key]
	if observation.Mode != policy.Mode {
		observation = turnStateObservation{}
	}
	if observation.Class == class {
		observation.Count = min(2, observation.Count+1)
	} else {
		observation.Class, observation.Count = class, 1
	}
	observation.Mode = policy.Mode
	observation.Length = len(value)
	observation.ObservedAt = now
	if cleared || (class == "healthy" && observation.Count >= 2) {
		observation.Recovering = false
	}
	s.observations[key] = observation
}

func (s *turnStateTemplateStore) noteRewrite(cfg turnStateTemplateConfig, account *auth.Account, model string) {
	key := turnStateTemplateKey{AccountID: account.ID(), Model: strings.TrimSpace(model)}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.makeObservationRoomLocked(cfg, key, now)
	observation := s.observations[key]
	if observation.Mode != turnStateStatusPolicy(cfg, account).Mode {
		observation = turnStateObservation{}
	}
	observation.Recovering = true
	observation.ObservedAt = now
	observation.Mode = turnStateStatusPolicy(cfg, account).Mode
	s.observations[key] = observation
}

// GetCodexTurnStateStatus combines bounded recent observations with database
// templates. It never scans usage logs or harvests client-supplied values.
func GetCodexTurnStateStatus(account *auth.Account) *CodexTurnStateStatus {
	if !accountEligibleForTurnStateTemplate(account) {
		return nil
	}
	cfg := loadTurnStateTemplateConfig()
	policy := turnStateStatusPolicy(cfg, account)
	result := &CodexTurnStateStatus{InjectionEnabled: CodexTurnStateInjectionEnabled(account), State: "unknown", Mode: policy.Mode, TemplateLength: policy.TemplateLength, ReplaceLength: policy.ReplaceLength, Models: []CodexTurnStateModelStatus{}}
	s := globalTurnStateTemplates
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeObservationsLocked(cfg, now)
	s.purgeExpiredLocked(cfg, now)
	entries := s.accountEntriesLocked(account.ID())
	models := make(map[turnStateTemplateKey]turnStateObservation)
	for key, observation := range s.observations {
		if key.AccountID == account.ID() && observation.Mode == policy.Mode && CodexTurnStateModelAllowed(account, key.Model) {
			models[key] = observation
		}
	}
	for key, entry := range entries {
		token, err := parseTurnStateToken(entry.Value)
		if err != nil || !turnStateTokenTimeValid(token, now, cfg.TTL) {
			continue
		}
		if !CodexTurnStateModelAllowed(account, key.Model) {
			continue
		}
		if _, ok := models[key]; !ok {
			models[key] = turnStateObservation{Mode: policy.Mode, ObservedAt: entry.IssuedAt}
		}
	}
	priority := map[string]int{"unknown": 0, "healthy": 1, "ready": 2, "recovering": 3, "degraded": 4}
	for key, observation := range models {
		state := "unknown"
		if observation.Count >= 2 {
			state = observation.Class
		}
		entry, cached := entries[key]
		var expires *time.Time
		if cached {
			token, err := parseTurnStateToken(entry.Value)
			cached = cfg.Enabled && err == nil && turnStateTokenAccept(token, now, cfg.TTL, cfg.policyFor(account).TemplateBlocks)
			if cached {
				expiry := token.Issued.Add(cfg.TTL - turnStateAcceptSkew)
				expires = &expiry
			}
		}
		if cached && !cfg.DryRun && result.InjectionEnabled {
			if observation.Recovering {
				state = "recovering"
			} else if state != "healthy" {
				state = "ready"
			}
		}
		result.Models = append(result.Models, CodexTurnStateModelStatus{Model: key.Model, State: state, Length: observation.Length, Consecutive: observation.Count, ObservedAt: observation.ObservedAt, TemplateCached: cached, TemplateExpiresAt: expires})
		if priority[state] > priority[result.State] {
			result.State = state
		}
	}
	sort.Slice(result.Models, func(i, j int) bool { return result.Models[i].Model < result.Models[j].Model })
	return result
}

// BeginCodexTurnStateTemplateAttempt prevents failover/retries from inheriting
// an earlier attempt's rewrite flag, while keeping the usage-log audit slot.
func BeginCodexTurnStateTemplateAttempt(ctx context.Context) context.Context {
	ctx = withTurnStateTemplateAudit(ctx)
	audit := turnStateTemplateAuditFromContext(ctx)
	audit.mu.Lock()
	audit.decision, audit.replacement = "", ""
	audit.inboundLen, audit.outboundLen = 0, 0
	audit.rewritten, audit.recorded = false, false
	audit.mu.Unlock()
	return ctx
}

// Confirm is called only after a successful transport send. Custom/manual
// overrides that replaced the auto template must not claim automatic recovery.
func ConfirmCodexTurnStateTemplate(ctx context.Context, headers http.Header, account *auth.Account, model string) {
	audit := turnStateTemplateAuditFromContext(ctx)
	if audit == nil || !CodexTurnStateInjectionEnabled(account) {
		return
	}
	audit.mu.Lock()
	rewritten := audit.rewritten && audit.replacement != "" && headers.Get(codexTurnStateHeader) == audit.replacement
	if audit.rewritten && !rewritten {
		audit.rewritten = false
		audit.decision = "pass"
	}
	audit.mu.Unlock()
	if rewritten {
		globalTurnStateTemplates.noteRewrite(loadTurnStateTemplateConfig(), account, model)
	}
}
