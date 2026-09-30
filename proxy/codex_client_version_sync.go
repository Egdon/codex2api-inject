package proxy

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/codex2api/database"
)

type CodexAppBuildSyncResult struct {
	FetchedVersion   string `json:"fetched_version,omitempty"`
	SyncedVersion    string `json:"synced_version,omitempty"`
	EffectiveVersion string `json:"effective_version"`
	Updated          bool   `json:"updated"`
	Error            string `json:"error,omitempty"`
}

type CodexClientVersionsSyncResult struct {
	CLI            CodexAppBuildSyncResult `json:"cli"`
	DesktopMac     CodexAppBuildSyncResult `json:"desktop_mac"`
	DesktopWindows CodexAppBuildSyncResult `json:"desktop_windows"`
	VSCode         CodexAppBuildSyncResult `json:"vscode"`
}

func codexPersistedBuild(settings *database.SystemSettings, kind string) string {
	switch kind {
	case "desktop-mac":
		return settings.CodexSyncedDesktopMacBuild
	case "desktop-windows":
		return settings.CodexSyncedDesktopWindowsBuild
	case "vscode":
		return settings.CodexSyncedVSCodeBuild
	}
	return ""
}

func codexRuntimeBuild(kind string) string {
	settings := CurrentRuntimeSettings()
	switch kind {
	case "desktop-mac":
		return settings.CodexSyncedDesktopMacBuild
	case "desktop-windows":
		return settings.CodexSyncedDesktopWindowsBuild
	case "vscode":
		return settings.CodexSyncedVSCodeBuild
	}
	return ""
}

func codexStoreRuntimeBuild(kind, version string) {
	UpdateRuntimeSettings(func(settings RuntimeSettings) RuntimeSettings {
		switch kind {
		case "desktop-mac":
			settings.CodexSyncedDesktopMacBuild = version
		case "desktop-windows":
			settings.CodexSyncedDesktopWindowsBuild = version
		case "vscode":
			settings.CodexSyncedVSCodeBuild = version
		}
		return settings
	})
}

func applyCodexAppBuild(ctx context.Context, db *database.DB, kind string, fetched codexFetchOutcome) CodexAppBuildSyncResult {
	result := CodexAppBuildSyncResult{EffectiveVersion: codexRuntimeBuild(kind)}
	if fetched.err != nil {
		result.Error = fetched.err.Error()
		return result
	}
	result.FetchedVersion = fetched.version
	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	current := ""
	if settings != nil {
		current = strings.TrimSpace(codexPersistedBuild(settings, kind))
	}
	if !codexBuildIsNewer(fetched.version, current) {
		if current != "" {
			codexStoreRuntimeBuild(kind, current)
		}
		result.EffectiveVersion = current
		return result
	}
	if err := db.UpdateCodexSyncedAppBuild(ctx, kind, fetched.version); err != nil {
		result.Error = err.Error()
		return result
	}
	codexStoreRuntimeBuild(kind, fetched.version)
	result.Updated, result.EffectiveVersion = true, fetched.version
	return result
}

type codexFetchOutcome struct {
	version string
	err     error
}

// SyncCodexClientVersions 各来源独立同步并分别报告失败，不丢弃其他来源的成功结果。
// 网络拉取并发进行(总耗时取最慢来源而非累加);落库与 RuntimeSettings 更新仍按顺序执行。
func SyncCodexClientVersions(ctx context.Context, db *database.DB, proxyURL string) (*CodexClientVersionsSyncResult, error) {
	if db == nil {
		return nil, fmt.Errorf("数据库不可用，无法同步 Codex 客户端版本")
	}
	fetchers := [...]func(context.Context, string) (string, error){
		FetchLatestCodexCLIVersion,
		FetchCodexDesktopMacBuild,
		FetchCodexVSCodeBuild,
		FetchCodexDesktopWindowsBuild,
	}
	var outcomes [len(fetchers)]codexFetchOutcome
	var wg sync.WaitGroup
	for i, fetch := range fetchers {
		wg.Go(func() {
			version, err := fetch(ctx, proxyURL)
			outcomes[i] = codexFetchOutcome{version: version, err: err}
		})
	}
	wg.Wait()

	result := &CodexClientVersionsSyncResult{}
	cliErr := outcomes[0].err
	var cli *CodexCLIVersionSyncResult
	if cliErr == nil {
		cli, cliErr = applyCodexCLIVersion(ctx, db, &CodexCLIVersionSyncResult{
			BuiltinVersion:   latestCodexCLIVersion,
			EffectiveVersion: effectiveLatestCodexCLIVersion(),
		}, outcomes[0].version)
	}
	result.CLI.EffectiveVersion = effectiveLatestCodexCLIVersion()
	result.CLI.SyncedVersion = CurrentRuntimeSettings().CodexSyncedCLIVersion
	if cliErr != nil {
		result.CLI.Error = cliErr.Error()
	} else {
		result.CLI.FetchedVersion, result.CLI.Updated = cli.FetchedVersion, cli.Updated
	}
	result.DesktopMac = applyCodexAppBuild(ctx, db, "desktop-mac", outcomes[1])
	result.VSCode = applyCodexAppBuild(ctx, db, "vscode", outcomes[2])
	result.DesktopWindows = applyCodexAppBuild(ctx, db, "desktop-windows", outcomes[3])
	return result, nil
}
