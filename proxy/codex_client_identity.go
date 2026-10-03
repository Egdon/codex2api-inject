package proxy

import (
	"net/http"
	"strings"

	"github.com/codex2api/auth"
)

type CodexClientIdentityInput struct {
	Account      *auth.Account
	APIKey       string
	DeviceConfig *DeviceProfileConfig
	Headers      http.Header
}

type CodexOutboundClientIdentity struct {
	UserAgent string
	Version   string
	Generated bool
}

// ResolveCodexOutboundClientIdentity 将版本不可用错误传到出站前的调用者。
func ResolveCodexOutboundClientIdentity(input CodexClientIdentityInput) (CodexOutboundClientIdentity, error) {
	if IsDeviceProfileStabilizationEnabled(input.DeviceConfig) {
		return codexDeviceClientIdentity(input), nil
	}
	userAgent := strings.TrimSpace(input.Headers.Get("User-Agent"))
	originator := strings.TrimSpace(input.Headers.Get("Originator"))
	settings := CurrentRuntimeSettings()
	if shouldGenerateCodexClientHeaders(settings, userAgent, originator) {
		ua, version, err := generatedCodexClientHeadersChecked(input.Account, settings)
		return CodexOutboundClientIdentity{UserAgent: ua, Version: version, Generated: true}, err
	}
	if IsCodexOfficialClientByHeaders(userAgent, originator) && userAgent != "" {
		version := firstNonEmptyHeader(input.Headers, "Version", codexVersionFromUserAgent(userAgent, latestCodexCLIVersion))
		return CodexOutboundClientIdentity{UserAgent: userAgent, Version: version}, nil
	}
	accountID := int64(0)
	if input.Account != nil {
		accountID = input.Account.ID()
	}
	floor := codexIdentityVersionFloor(settings)
	ua, version, configured, err := codexUserAgentFromConfigChecked(settings.CodexUserAgentConfig, accountID, floor)
	if err != nil || configured {
		return CodexOutboundClientIdentity{UserAgent: ua, Version: version, Generated: true}, err
	}
	version = effectiveLatestCodexCLIVersion()
	if !codexVersionAtLeast(version, floor) {
		return CodexOutboundClientIdentity{}, codexClientVersionUnavailable("codex-cli", "", floor)
	}
	return CodexOutboundClientIdentity{UserAgent: replaceCodexUserAgentVersion(defaultCodexCLIUserAgent, version), Version: version}, nil
}

func codexIdentityVersionFloor(settings RuntimeSettings) string {
	if settings.ClientCompatMode == ClientCompatModeAuto {
		return settings.CodexMinCLIVersion
	}
	return ""
}

func codexDeviceClientIdentity(input CodexClientIdentityInput) CodexOutboundClientIdentity {
	profile := ResolveDeviceProfile(input.Account, input.APIKey, input.Headers, input.DeviceConfig)
	return CodexOutboundClientIdentity{UserAgent: firstNonEmptyString(profile.UserAgent, defaultCodexCLIUserAgent), Version: codexVersionFromProfile(profile, input.DeviceConfig.PackageVersion)}
}

func generatedCodexClientHeadersChecked(account *auth.Account, settings RuntimeSettings) (string, string, error) {
	floor := codexIdentityVersionFloor(settings)
	accountID := int64(0)
	if account != nil {
		accountID = account.ID()
	}
	ua, version, configured, err := codexUserAgentFromConfigChecked(settings.CodexUserAgentConfig, accountID, floor)
	if err != nil || configured {
		return ua, version, err
	}
	profile := ProfileForAccount(accountID)
	version = effectiveLatestCodexCLIVersion()
	if !codexVersionAtLeast(version, floor) {
		return "", "", codexClientVersionUnavailable("codex-cli", "", floor)
	}
	ua = firstNonEmptyString(profile.UserAgent, defaultCodexCLIUserAgent)
	return replaceCodexUserAgentVersion(ua, version), version, nil
}
