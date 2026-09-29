package auth

import "testing"

func TestBPSMasterKeepsSavedAccountOptInDistinct(t *testing.T) {
	old := OpenAIExcelBPSEnabled()
	t.Cleanup(func() { SetOpenAIExcelBPSEnabled(old) })
	// Zero-value storage deliberately means enabled for legacy deployments.
	openAIExcelBPSDisabled.Store(false)
	if !OpenAIExcelBPSEnabled() {
		t.Fatal("master default must be true")
	}
	account := &Account{AccessToken: "synthetic"}
	if account.IsExcelBPSEnabled() {
		t.Fatal("account default must remain false")
	}
	account.SetExcelBPSEnabled(true)
	SetOpenAIExcelBPSEnabled(false)
	if !account.IsExcelBPSEnabled() || OpenAIExcelBPSEnabled() {
		t.Fatal("master overwrote saved opt-in")
	}
}

func TestBPSRejectsIncompleteForeignCredentialKinds(t *testing.T) {
	for _, account := range []*Account{
		{AccessToken: "synthetic", ExcelBPSEnabled: true, UpstreamType: UpstreamOpenAIResponses},
		{AccessToken: "synthetic", ExcelBPSEnabled: true, UpstreamType: UpstreamClaude},
		{AccessToken: "synthetic", ExcelBPSEnabled: true, UpstreamType: UpstreamGrok},
		{AccessToken: "synthetic", ExcelBPSEnabled: true, UpstreamType: UpstreamAntigravity},
		{AccessToken: "synthetic", ExcelBPSEnabled: true, CodexAuthMode: CodexAuthModeAgentIdentity},
	} {
		if account.IsExcelBPSEligible() || account.IsExcelBPSEnabled() {
			t.Fatal("nonordinary OAuth credential accepted")
		}
	}
}
