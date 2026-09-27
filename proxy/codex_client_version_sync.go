package proxy

import (
	"context"
	"fmt"
	"strings"

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

type codexAppBuildSource struct {
	Kind  string
	Fetch func(context.Context) (string, error)
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

func syncCodexAppBuild(ctx context.Context, db *database.DB, source codexAppBuildSource) CodexAppBuildSyncResult {
	result := CodexAppBuildSyncResult{EffectiveVersion: codexRuntimeBuild(source.Kind)}
	fetched, err := source.Fetch(ctx)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.FetchedVersion = fetched
	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	current := ""
	if settings != nil {
		current = strings.TrimSpace(codexPersistedBuild(settings, source.Kind))
	}
	if !codexBuildIsNewer(fetched, current) {
		if current != "" {
			codexStoreRuntimeBuild(source.Kind, current)
		}
		result.EffectiveVersion = current
		return result
	}
	if err := db.UpdateCodexSyncedAppBuild(ctx, source.Kind, fetched); err != nil {
		result.Error = err.Error()
		return result
	}
	codexStoreRuntimeBuild(source.Kind, fetched)
	result.Updated, result.EffectiveVersion = true, fetched
	return result
}

// SyncCodexClientVersions 各来源独立同步并分别报告失败，不丢弃其他来源的成功结果。
func SyncCodexClientVersions(ctx context.Context, db *database.DB, proxyURL string) (*CodexClientVersionsSyncResult, error) {
	if db == nil {
		return nil, fmt.Errorf("数据库不可用，无法同步 Codex 客户端版本")
	}
	result := &CodexClientVersionsSyncResult{}
	cli, err := SyncCodexCLIVersion(ctx, db, proxyURL)
	result.CLI.EffectiveVersion = effectiveLatestCodexCLIVersion()
	result.CLI.SyncedVersion = CurrentRuntimeSettings().CodexSyncedCLIVersion
	if err != nil {
		result.CLI.Error = err.Error()
	} else {
		result.CLI.FetchedVersion, result.CLI.Updated = cli.FetchedVersion, cli.Updated
	}
	result.DesktopMac = syncCodexAppBuild(ctx, db, codexAppBuildSource{Kind: "desktop-mac", Fetch: func(ctx context.Context) (string, error) {
		return FetchCodexDesktopMacBuild(ctx, proxyURL)
	}})
	result.VSCode = syncCodexAppBuild(ctx, db, codexAppBuildSource{Kind: "vscode", Fetch: func(ctx context.Context) (string, error) {
		return FetchCodexVSCodeBuild(ctx, proxyURL)
	}})
	result.DesktopWindows = syncCodexAppBuild(ctx, db, codexAppBuildSource{Kind: "desktop-windows", Fetch: func(ctx context.Context) (string, error) {
		return FetchCodexDesktopWindowsBuild(ctx, proxyURL)
	}})
	return result, nil
}
