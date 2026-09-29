package auth

import "testing"

func TestExcelBPSGateOnlyAllowsOptedInOAuthAccounts(t *testing.T) {
	oauth := &Account{AccessToken: "access-token", ExcelBPSEnabled: true}
	if !oauth.IsExcelBPSEnabled() {
		t.Fatal("opted-in OAuth account was not eligible")
	}
	apiKey := &Account{AccessToken: "access-token", APIKey: "api-key", ExcelBPSEnabled: true}
	if apiKey.IsExcelBPSEnabled() {
		t.Fatal("API-key account was eligible for Excel BPS")
	}
	relay := &Account{AccessToken: "access-token", UpstreamType: UpstreamOpenAIResponses, BaseURL: "https://example.invalid", APIKey: "relay-key", ExcelBPSEnabled: true}
	if relay.IsExcelBPSEnabled() {
		t.Fatal("Responses relay account was eligible for Excel BPS")
	}
	disabled := &Account{AccessToken: "access-token"}
	if disabled.IsExcelBPSEnabled() {
		t.Fatal("unflagged account was eligible for Excel BPS")
	}
	limited := &Account{AccessToken: "access-token", ExcelBPSEnabled: true, Models: []string{"gpt-5.5"}}
	if !limited.IsExcelBPSAvailableForModel("gpt-5.5") || limited.IsExcelBPSAvailableForModel("gpt-5.4") {
		t.Fatal("model allowlist was not enforced for Excel BPS")
	}
}

func TestExcelBPSGlobalDefaultAndAccountModes(t *testing.T) {
	t.Cleanup(func() { SetExcelBPSGlobalEnabled(false) })
	for _, tc := range []struct {
		name            string
		global          bool
		enabled, optOut bool
		apiKey          string
		relay           bool
		wantMode        string
		wantEffective   bool
	}{
		{name: "inherit global off", wantMode: ExcelBPSModeInherit},
		{name: "inherit global on", global: true, wantMode: ExcelBPSModeInherit, wantEffective: true},
		{name: "on global off", enabled: true, wantMode: ExcelBPSModeOn, wantEffective: true},
		{name: "off global on", global: true, optOut: true, wantMode: ExcelBPSModeOff},
		{name: "explicit on wins over stale opt-out", global: true, enabled: true, optOut: true, wantMode: ExcelBPSModeOn, wantEffective: true},
		{name: "global never enables API-key accounts", global: true, apiKey: "api-key", wantMode: ExcelBPSModeInherit},
		{name: "global never enables relay accounts", global: true, apiKey: "relay-key", relay: true, wantMode: ExcelBPSModeInherit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			SetExcelBPSGlobalEnabled(tc.global)
			account := &Account{AccessToken: "access-token", APIKey: tc.apiKey, ExcelBPSEnabled: tc.enabled, ExcelBPSOptOut: tc.optOut}
			if tc.relay {
				account.UpstreamType, account.BaseURL = UpstreamOpenAIResponses, "https://example.invalid"
			}
			if got := account.ExcelBPSMode(); got != tc.wantMode {
				t.Fatalf("mode = %q, want %q", got, tc.wantMode)
			}
			if got := account.IsExcelBPSEnabled(); got != tc.wantEffective {
				t.Fatalf("effective = %t, want %t", got, tc.wantEffective)
			}
		})
	}
}
