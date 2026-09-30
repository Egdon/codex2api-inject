package auth

import (
	"strings"
	"sync/atomic"
)

// ExcelBPSCredentialKey is the durable, opt-in account capability flag used by
// the Basispoints Responses adapter. It lives in the existing credentials JSON
// so older databases can safely ignore it.
const ExcelBPSCredentialKey = "openai_excel_bps"

// ExcelBPSOptOutCredentialKey excludes an account from the global Basispoints
// default. It has no effect while the account itself opts in, and no effect
// while the global default is off.
const ExcelBPSOptOutCredentialKey = "openai_excel_bps_opt_out"

// Account-level Basispoints modes as shown to administrators.
const (
	ExcelBPSModeInherit = "inherit"
	ExcelBPSModeOn      = "on"
	ExcelBPSModeOff     = "off"
)

// excelBPSGlobalEnabled mirrors the runtime "use Basispoints by default"
// setting. The proxy runtime settings store publishes it; auth cannot import
// proxy, so the value is kept here next to the account-level resolution.
var excelBPSGlobalEnabled atomic.Bool

// SetExcelBPSGlobalEnabled publishes the global Basispoints default.
func SetExcelBPSGlobalEnabled(enabled bool) {
	excelBPSGlobalEnabled.Store(enabled)
}

// ExcelBPSGlobalEnabled reports the global Basispoints default.
func ExcelBPSGlobalEnabled() bool {
	return excelBPSGlobalEnabled.Load()
}

// IsExcelBPSEnabled reports whether this account currently uses the Excel
// Basispoints adapter: either the account opts in, or the global default is on
// and the account has not opted out. The capability is deliberately limited to
// ordinary OAuth Codex accounts: relay/API-key accounts and agent identities
// have different provider contracts and must continue through their existing
// paths regardless of either setting.
func (a *Account) IsExcelBPSEnabled() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.excelBPSSelectedLocked() && a.isExcelBPSEligibleLocked()
}

// IsExcelBPSConfigured reports saved Basispoints intent, including the global
// default. Credential availability, model rules and transient route health must
// not re-enable legacy injection for a configured account. Routing continues to
// use IsExcelBPSEnabled, which additionally checks upstream eligibility.
func (a *Account) IsExcelBPSConfigured() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.excelBPSSelectedLocked()
}

// IsExcelBPSEligible reports whether this account type can use Basispoints at
// all, independently of the account and global settings.
func (a *Account) IsExcelBPSEligible() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isExcelBPSEligibleLocked()
}

// ExcelBPSMode returns the configured account mode (not the effective value).
func (a *Account) ExcelBPSMode() string {
	if a == nil {
		return ExcelBPSModeInherit
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return ExcelBPSModeFor(a.ExcelBPSEnabled, a.ExcelBPSOptOut)
}

// ExcelBPSModeFor maps the two persisted flags to the account mode. An explicit
// opt-in wins over a stale opt-out so older writers keep their meaning.
func ExcelBPSModeFor(enabled, optOut bool) string {
	switch {
	case enabled:
		return ExcelBPSModeOn
	case optOut:
		return ExcelBPSModeOff
	default:
		return ExcelBPSModeInherit
	}
}

func (a *Account) excelBPSSelectedLocked() bool {
	if a.ExcelBPSEnabled {
		return true
	}
	return !a.ExcelBPSOptOut && excelBPSGlobalEnabled.Load()
}

func (a *Account) isExcelBPSEligibleLocked() bool {
	return !a.isRelayStyleLocked() &&
		!a.isCodexAgentIdentityLocked() &&
		strings.TrimSpace(a.APIKey) == "" &&
		strings.TrimSpace(a.AccessToken) != ""
}

// IsExcelBPSAvailableForModel combines the account opt-in with the existing
// Codex model allowlist. An empty allowlist retains the normal all-model
// behavior; a populated list is an explicit per-account capability boundary.
func (a *Account) IsExcelBPSAvailableForModel(model string) bool {
	return a.IsExcelBPSEnabled() && a.SupportsCodexModel(model)
}

// SetExcelBPSEnabled updates the in-memory capability after a persisted admin
// change. The persistence write is owned by the admin/database layer.
func (a *Account) SetExcelBPSEnabled(enabled bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.ExcelBPSEnabled = enabled
	a.mu.Unlock()
}

// SetExcelBPSOptOut updates the in-memory global-default exclusion after a
// persisted admin change.
func (a *Account) SetExcelBPSOptOut(optOut bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.ExcelBPSOptOut = optOut
	a.mu.Unlock()
}

// ApplyAccountExcelBPSEnabled publishes an admin capability change to the live
// scheduler object without waiting for the next outbox poll.
func (s *Store) ApplyAccountExcelBPSEnabled(dbID int64, enabled bool) bool {
	if s == nil {
		return false
	}
	account := s.FindByID(dbID)
	if account == nil {
		return false
	}
	account.SetExcelBPSEnabled(enabled)
	return true
}

// ApplyAccountExcelBPSOptOut publishes an admin exclusion change to the live
// scheduler object without waiting for the next outbox poll.
func (s *Store) ApplyAccountExcelBPSOptOut(dbID int64, optOut bool) bool {
	if s == nil {
		return false
	}
	account := s.FindByID(dbID)
	if account == nil {
		return false
	}
	account.SetExcelBPSOptOut(optOut)
	return true
}
