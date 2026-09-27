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
