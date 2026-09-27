package auth

import "strings"

// ExcelBPSCredentialKey is the durable, opt-in account capability flag used by
// the Basispoints Responses adapter. It lives in the existing credentials JSON
// so older databases can safely ignore it.
const ExcelBPSCredentialKey = "openai_excel_bps"

// IsExcelBPSEnabled reports whether this account may use the Excel
// Basispoints adapter. The capability is deliberately limited to ordinary
// OAuth Codex accounts: relay/API-key accounts and agent identities have
// different provider contracts and must continue through their existing paths.
func (a *Account) IsExcelBPSEnabled() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ExcelBPSEnabled &&
		!a.isRelayStyleLocked() &&
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
