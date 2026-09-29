package proxy

import (
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// excelBPSRouteAvailable is the single account/model gate for the Excel
// Basispoints adapter. It combines the effective account setting (explicit
// opt-in, or the global default without an opt-out), the account's Codex model
// allowlist and the optional global Basispoints model list. Request-shaped
// reasons to stay on native Codex are decided later by handleExcelBPS.
func excelBPSRouteAvailable(account *auth.Account, model string) bool {
	if account == nil || !account.IsExcelBPSAvailableForModel(model) {
		return false
	}
	return excelBPSModelListed(CurrentRuntimeSettings().CodexBasispointsModels, model)
}

// excelBPSModelListed reports whether model passes the normalized global list.
// An empty list does not restrict models.
func excelBPSModelListed(list, model string) bool {
	models := database.ParseCodexBasispointsModels(list)
	if len(models) == 0 {
		return true
	}
	model = strings.ToLower(strings.TrimSpace(model))
	for _, listed := range models {
		if listed == model {
			return true
		}
	}
	return false
}
