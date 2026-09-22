package proxy

import (
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// CodexTurnStateTemplateRenewalDue only renews a usable upstream template during
// its last ten minutes. Expiry is identical to injection, including clock skew.
func CodexTurnStateTemplateRenewalDue(account *auth.Account, row database.CodexTurnStateTemplate, now time.Time) bool {
	cfg := loadTurnStateTemplateConfig()
	if !cfg.Enabled || !CodexTurnStateInjectionEnabled(account) || cfg.DryRun || account.ID() != row.AccountID || !CodexTurnStateModelAllowed(account, row.Model) || row.Strikes >= turnStateStrikeThreshold {
		return false
	}
	token, err := parseTurnStateToken(row.Value)
	return err == nil && token.Issued.Unix() == row.IssuedAt &&
		turnStateTokenAccept(token, now, cfg.TTL, cfg.policyFor(account).TemplateBlocks) &&
		!now.Before(token.Issued.Add(cfg.TTL-turnStateAcceptSkew-10*time.Minute))
}

// CodexTurnStateTemplateExpiresAt shares injection's effective TTL with history snapshots.
func CodexTurnStateTemplateExpiresAt(issuedAt int64) time.Time {
	return time.Unix(issuedAt, 0).Add(loadTurnStateTemplateConfig().TTL - turnStateAcceptSkew)
}
