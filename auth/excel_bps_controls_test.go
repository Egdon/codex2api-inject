package auth

import "testing"

// The old master-switch tests enforced a second, default-on gate. Configured
// intent instead follows the upstream tri-state setting, independently of
// transient credentials and route eligibility used by IsExcelBPSEnabled.
func TestExcelBPSConfiguredIntent(t *testing.T) {
	old := ExcelBPSGlobalEnabled()
	t.Cleanup(func() { SetExcelBPSGlobalEnabled(old) })
	for _, global := range []bool{false, true} {
		SetExcelBPSGlobalEnabled(global)
		for _, enabled := range []bool{false, true} {
			for _, optOut := range []bool{false, true} {
				for _, account := range []*Account{
					{ExcelBPSEnabled: enabled, ExcelBPSOptOut: optOut},
					{AccessToken: "synthetic", ExcelBPSEnabled: enabled, ExcelBPSOptOut: optOut, Models: []string{"other-model"}},
					{APIKey: "synthetic", ExcelBPSEnabled: enabled, ExcelBPSOptOut: optOut, UpstreamType: UpstreamOpenAIResponses},
				} {
					want := enabled || (!optOut && global)
					if got := account.IsExcelBPSConfigured(); got != want {
						t.Fatalf("global=%t enabled=%t optOut=%t configured=%t want %t", global, enabled, optOut, got, want)
					}
				}
			}
		}
	}
	if (*Account)(nil).IsExcelBPSConfigured() {
		t.Fatal("nil account is configured")
	}
}
