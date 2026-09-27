package proxy

import (
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
)

// ==================== Codex 客户端形态目录 ====================
//
// 真实 Codex 客户端的 User-Agent 形状是
//   {originator}/{cli版本} ({os} {os版本}; {arch}) {terminal} ({app名}; {app版本})
// 末尾标记按形态各不相同:codex-tui / codex_exec 复用前缀与 CLI 版本;ChatGPT 桌面端固定
// "Codex Desktop" 加桌面端构建号;VS Code 插件写宿主 IDE 名(VS Code / Cursor / Windsurf)
// 加插件构建号。目录中的历史版本对仅用作未同步时的默认画像。
//
// 下面的权重来自 2026-09-07~08 约 6.5 万条去重请求的下游 UA 统计(按百分比取整),
// 用于:预设默认值、管理页的搭配候选和号池画像分布。

// CodexClientKind 是可模拟的 Codex 客户端形态。
type CodexClientKind string

const (
	CodexClientKindTUI     CodexClientKind = "codex-tui"
	CodexClientKindDesktop CodexClientKind = "codex-desktop"
	CodexClientKindVSCode  CodexClientKind = "codex-vscode"
	CodexClientKindExec    CodexClientKind = "codex-exec"
	CodexClientKindCustom  CodexClientKind = "custom"
)

const (
	CodexUserAgentModeSingle = "single"
	CodexUserAgentModePool   = "pool"
)

type codexUAWeighted struct {
	Value  string
	Weight int
}

type codexUAPlatform struct {
	OSName    string
	OSVersion string
	Arch      string
	Weight    int
}

// codexUAVersionPair 是一组真实出现过的 (CLI 版本, 末尾标记版本) 配对;Weight 为观测占比。
type codexUAVersionPair struct {
	CLIVersion string
	AppVersion string
	Weight     int
}

type codexUAKindSpec struct {
	Kind          CodexClientKind
	ClientName    string // UA 前缀,同时作为 Originator
	AppFollowsCLI bool   // 末尾标记 = (client_name; cli_version),无独立构建号
	// 默认值:单画像模式下字段留空时使用。TUI 沿用历史常量,保证既有部署出站字节不变。
	DefaultPlatform codexUAPlatform
	DefaultTerminal string
	AppNames        []codexUAWeighted // 末尾标记名候选,首个为默认
	Terminals       []codexUAWeighted
	Platforms       []codexUAPlatform
	VersionPairs    []codexUAVersionPair // 未同步时的画像默认值;AppFollowsCLI 时为空
}

var codexUAKindOrder = []CodexClientKind{
	CodexClientKindTUI,
	CodexClientKindDesktop,
	CodexClientKindVSCode,
	CodexClientKindExec,
}

// 默认号池配比(按真实流量占比取整):桌面端过半,VS Code 次之,TUI 再次。
var defaultCodexUAPoolMix = map[CodexClientKind]int{
	CodexClientKindDesktop: 50,
	CodexClientKindVSCode:  30,
	CodexClientKindTUI:     20,
}

var codexUACatalog = map[CodexClientKind]*codexUAKindSpec{
	CodexClientKindTUI: {
		Kind:            CodexClientKindTUI,
		ClientName:      latestCodexClientName,
		AppFollowsCLI:   true,
		DefaultPlatform: codexUAPlatform{OSName: defaultCodexUserAgentOSName, OSVersion: defaultCodexUserAgentOSVersion, Arch: defaultCodexUserAgentArch},
		DefaultTerminal: defaultCodexUserAgentTerminal,
		AppNames:        []codexUAWeighted{{latestCodexClientName, 100}},
		Terminals: []codexUAWeighted{
			{"unknown", 30}, {"WindowsTerminal", 21}, {"kitty", 7}, {"xterm-256color", 7},
			{"vscode/1.135.0", 3}, {"vscode/1.126.0", 3}, {"Apple_Terminal/470.2", 3}, {"Apple_Terminal/455.1", 2},
			{"gnome-terminal", 3}, {"iTerm.app/3.6.11", 2}, {"vscode/1.101.2", 2}, {"xterm", 2}, {"ghostty/1.3.1", 1},
		},
		Platforms: []codexUAPlatform{
			{"Windows", "10.0.26200", "x86_64", 47}, {"NixOS", "26.5.0", "x86_64", 8}, {"Ubuntu", "22.4.0", "x86_64", 7},
			{"Windows", "10.0.19045", "x86_64", 6}, {"Ubuntu", "24.4.0", "x86_64", 4}, {"Ubuntu", "20.4.0", "aarch64", 3},
			{"Mac OS", "14.6.1", "x86_64", 3}, {"Mac OS", "26.6.2", "arm64", 2}, {"Windows", "10.0.22631", "x86_64", 2},
			{"CentOS", "7.0.0", "x86_64", 1}, {"Windows", "10.0.26100", "x86_64", 1}, {"Mac OS", "15.7.7", "x86_64", 1},
			{"Mac OS", "15.7.3", "arm64", 1}, {"Mac OS", "15.5.0", "arm64", 1},
		},
	},
	CodexClientKindDesktop: {
		Kind:            CodexClientKindDesktop,
		ClientName:      "Codex Desktop",
		DefaultPlatform: codexUAPlatform{OSName: "Windows", OSVersion: "10.0.26200", Arch: "x86_64"},
		DefaultTerminal: "unknown",
		AppNames:        []codexUAWeighted{{"Codex Desktop", 100}},
		Terminals:       []codexUAWeighted{{"unknown", 999}, {"dumb", 1}},
		Platforms: []codexUAPlatform{
			{"Windows", "10.0.26200", "x86_64", 47}, {"Mac OS", "26.5.2", "arm64", 8}, {"Windows", "10.0.19045", "x86_64", 7},
			{"Windows", "10.0.22631", "x86_64", 5}, {"Mac OS", "14.4.1", "arm64", 5}, {"Mac OS", "26.5.1", "arm64", 4},
			{"Mac OS", "26.6.2", "arm64", 3}, {"Windows", "10.0.26100", "x86_64", 3}, {"Mac OS", "26.5.0", "arm64", 2},
			{"Windows", "10.0.22621", "x86_64", 2}, {"Mac OS", "26.2.0", "arm64", 1}, {"Mac OS", "26.3.0", "arm64", 1},
			{"Windows", "10.0.26220", "x86_64", 1}, {"Mac OS", "26.4.1", "arm64", 1}, {"Mac OS", "15.7.3", "arm64", 1},
			{"Mac OS", "26.4.0", "arm64", 1},
		},
		// 只收录正式版配对;alpha 构建不进预设。
		VersionPairs: []codexUAVersionPair{
			{"0.153.4", "26.901.51231", 63}, {"0.153.4", "26.901.41600", 7}, {"0.153.3", "26.901.41123", 2},
			{"0.153.1", "26.901.31953", 1}, {"0.153.0", "26.901.22334", 2}, {"0.152.1", "26.831.21537", 1},
			{"0.152.0", "26.831.20005", 3}, {"0.150.1", "26.901.31953", 1}, {"0.149.1", "26.901.51231", 1},
		},
	},
	CodexClientKindVSCode: {
		Kind:            CodexClientKindVSCode,
		ClientName:      "codex_vscode",
		DefaultPlatform: codexUAPlatform{OSName: "Ubuntu", OSVersion: "22.4.0", Arch: "x86_64"},
		DefaultTerminal: "unknown",
		AppNames:        []codexUAWeighted{{"VS Code", 96}, {"Cursor", 3}, {"Windsurf", 1}, {"Positron", 1}},
		Terminals:       []codexUAWeighted{{"unknown", 70}, {"xterm-256color", 29}, {"WindowsTerminal", 1}, {"gnome-terminal", 1}},
		Platforms: []codexUAPlatform{
			{"Ubuntu", "22.4.0", "x86_64", 36}, {"Windows", "10.0.26200", "x86_64", 20}, {"Windows", "10.0.19045", "x86_64", 11},
			{"Ubuntu", "24.4.0", "x86_64", 9}, {"Mac OS", "26.6.2", "arm64", 3}, {"Mac OS", "26.4.0", "arm64", 3},
			{"CentOS", "8.6.2205", "x86_64", 3}, {"Ubuntu", "20.4.0", "x86_64", 2}, {"Windows", "10.0.22631", "x86_64", 2},
			{"Windows", "10.0.26100", "x86_64", 2}, {"Mac OS", "15.7.9", "arm64", 2}, {"CentOS", "7.0.0", "x86_64", 2},
			{"Windows", "10.0.22631", "aarch64", 1}, {"Mac OS", "26.5.2", "arm64", 1},
		},
		// 插件主力仍是 0.153.0,与 CLI 最新版 0.153.4 不同步。
		VersionPairs: []codexUAVersionPair{
			{"0.153.4", "26.901.22334", 1}, {"0.153.0", "26.901.22334", 96}, {"0.147.0", "26.519.32039", 1},
			{"0.144.5", "26.707.91948", 1}, {"0.142.5", "26.623.81905", 1},
		},
	},
	CodexClientKindExec: {
		Kind:            CodexClientKindExec,
		ClientName:      "codex_exec",
		AppFollowsCLI:   true,
		DefaultPlatform: codexUAPlatform{OSName: "Windows", OSVersion: "10.0.19045", Arch: "x86_64"},
		DefaultTerminal: "unknown",
		AppNames:        []codexUAWeighted{{"codex_exec", 100}},
		Terminals:       []codexUAWeighted{{"unknown", 56}, {"dumb", 43}, {"kitty", 1}},
		Platforms: []codexUAPlatform{
			{"Windows", "10.0.19045", "x86_64", 52}, {"Ubuntu", "24.4.0", "x86_64", 40}, {"Ubuntu", "22.4.0", "x86_64", 3},
			{"Windows", "10.0.26100", "x86_64", 2}, {"Windows", "10.0.26200", "x86_64", 1}, {"Mac OS", "26.6.2", "arm64", 1},
			{"NixOS", "26.5.0", "x86_64", 1},
		},
	},
}

func codexUAKindSpecFor(kind CodexClientKind) (*codexUAKindSpec, bool) {
	spec, ok := codexUACatalog[kind]
	return spec, ok
}

// normalizeCodexClientKind 规范化配置里的 client_kind;空串表示"未指定,按 client_name 推断"。
func normalizeCodexClientKind(value string) (CodexClientKind, bool) {
	kind := CodexClientKind(strings.ToLower(strings.TrimSpace(value)))
	switch kind {
	case "":
		return "", true
	case CodexClientKindTUI, CodexClientKindDesktop, CodexClientKindVSCode, CodexClientKindExec, CodexClientKindCustom:
		return kind, true
	default:
		return "", false
	}
}

// inferCodexClientKind 按客户端名推断形态:未指定 kind 的旧配置与手填 "Codex Desktop"
// 都能自动落到对应预设;认不出的名字视为 custom。
func inferCodexClientKind(clientName string) CodexClientKind {
	name := strings.ToLower(strings.TrimSpace(clientName))
	if name == "" {
		return CodexClientKindTUI
	}
	for _, kind := range codexUAKindOrder {
		if spec := codexUACatalog[kind]; strings.EqualFold(spec.ClientName, name) {
			return kind
		}
	}
	return CodexClientKindCustom
}

func effectiveCodexClientKind(cfg CodexUserAgentConfig) CodexClientKind {
	if kind, ok := normalizeCodexClientKind(cfg.ClientKind); ok && kind != "" {
		return kind
	}
	return inferCodexClientKind(cfg.ClientName)
}

func codexUAHash(seed string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte("codex2api:ua-catalog:v1:" + seed))
	return h.Sum32()
}

func pickCodexUAWeighted(items []codexUAWeighted, seed string) string {
	total := 0
	for _, it := range items {
		if it.Weight > 0 {
			total += it.Weight
		}
	}
	if total <= 0 || len(items) == 0 {
		return ""
	}
	r := int(codexUAHash(seed) % uint32(total))
	for _, it := range items {
		if it.Weight <= 0 {
			continue
		}
		if r < it.Weight {
			return it.Value
		}
		r -= it.Weight
	}
	return items[len(items)-1].Value
}

func pickCodexUAPlatform(items []codexUAPlatform, seed string) codexUAPlatform {
	total := 0
	for _, it := range items {
		if it.Weight > 0 {
			total += it.Weight
		}
	}
	if total <= 0 || len(items) == 0 {
		return codexUAPlatform{}
	}
	r := int(codexUAHash(seed) % uint32(total))
	for _, it := range items {
		if it.Weight <= 0 {
			continue
		}
		if r < it.Weight {
			return it
		}
		r -= it.Weight
	}
	return items[len(items)-1]
}

func pickCodexUAVersionPair(items []codexUAVersionPair, seed string) (codexUAVersionPair, bool) {
	total := 0
	for _, it := range items {
		if it.Weight > 0 {
			total += it.Weight
		}
	}
	if total <= 0 || len(items) == 0 {
		return codexUAVersionPair{}, false
	}
	r := int(codexUAHash(seed) % uint32(total))
	for _, it := range items {
		if it.Weight <= 0 {
			continue
		}
		if r < it.Weight {
			return it, true
		}
		r -= it.Weight
	}
	return items[len(items)-1], true
}

func codexVersionAtLeast(version, floor string) bool {
	floor = normalizeCodexClientVersionText(floor)
	if floor == "" {
		return true
	}
	cmp, ok := compareCodexClientVersions(version, floor)
	return !ok || cmp >= 0
}

// codexPairsMeetingFloor 返回不低于版本门槛的配对,保持目录顺序(从新到旧)。
func codexPairsMeetingFloor(pairs []codexUAVersionPair, floor string) []codexUAVersionPair {
	out := make([]codexUAVersionPair, 0, len(pairs))
	for _, p := range pairs {
		if codexVersionAtLeast(p.CLIVersion, floor) {
			out = append(out, p)
		}
	}
	return out
}

// resolveCodexVersionPair 决定出站的 (CLI 版本, 末尾标记版本):
//   - 末尾跟随 CLI 的形态(TUI / exec):沿用既有规则,配置值(空则取当前最新)叠加门槛。
//   - 显式给了构建号:原样使用;只有版本门槛把 CLI 版本抬高时才改配对,优先落到目录里
//     不低于门槛的最近正式配对,目录也没有时保留用户构建号。
//   - 构建号留空(自动配对):CLI 版本只当提示(空则取该形态占比最高的配对),始终停在真实
//     出现过的配对上——精确命中就用,否则取不低于门槛且最接近的配对;门槛高过全部已知
//     配对时用门槛版本配最新构建号。
func resolveCodexVersionPair(spec *codexUAKindSpec, cliVersion, appVersion, versionFloor string) (string, string) {
	cliVersion = normalizeCodexClientVersionText(cliVersion)
	appVersion = strings.TrimSpace(appVersion)
	if spec == nil || spec.AppFollowsCLI || len(spec.VersionPairs) == 0 {
		v := effectiveCodexClientVersion(firstNonEmptyString(cliVersion, effectiveLatestCodexCLIVersion()), versionFloor)
		if spec == nil && appVersion != "" {
			return v, appVersion
		}
		return v, v
	}
	pairs := spec.VersionPairs
	newest := pairs[0]
	heaviest := heaviestCodexPair(pairs)
	if appVersion != "" {
		v := firstNonEmptyString(cliVersion, heaviest.CLIVersion)
		raised := effectiveCodexClientVersion(v, versionFloor)
		if raised == v {
			return v, appVersion
		}
		if p, ok := closestCodexPairAtLeast(pairs, raised); ok {
			return p.CLIVersion, p.AppVersion
		}
		return raised, appVersion
	}
	base := firstNonEmptyString(cliVersion, heaviest.CLIVersion)
	candidates := codexPairsMeetingFloor(pairs, versionFloor)
	if len(candidates) == 0 {
		return effectiveCodexClientVersion(base, versionFloor), newest.AppVersion
	}
	for _, p := range candidates {
		if p.CLIVersion == base {
			return p.CLIVersion, p.AppVersion
		}
	}
	// 最近的不高于 base 的配对(候选从新到旧,首个 <= base 即最近)。
	for _, p := range candidates {
		if cmp, ok := compareCodexClientVersions(p.CLIVersion, base); ok && cmp <= 0 {
			return p.CLIVersion, p.AppVersion
		}
	}
	// 全部高于 base:取最小的那个,即最后一个候选。
	last := candidates[len(candidates)-1]
	return last.CLIVersion, last.AppVersion
}

type codexVersionSelection struct {
	OSName       string
	CLIOverride  string
	AppOverride  string
	VersionFloor string
	Fallback     codexUAVersionPair
}

// resolveCodexCurrentVersions 优先使用同步缓存，旧画像版本对只作默认值。
func resolveCodexCurrentVersions(spec *codexUAKindSpec, choice codexVersionSelection) (string, string) {
	if spec == nil || (spec.Kind != CodexClientKindDesktop && spec.Kind != CodexClientKindVSCode) {
		return resolveCodexVersionPair(spec, choice.CLIOverride, choice.AppOverride, choice.VersionFloor)
	}
	settings := CurrentRuntimeSettings()
	autoCLI := choice.Fallback.CLIVersion
	if strings.TrimSpace(settings.CodexSyncedCLIVersion) != "" {
		autoCLI = effectiveLatestCodexCLIVersion()
	}
	autoApp := choice.Fallback.AppVersion
	hasSyncedApp := false
	if spec.Kind == CodexClientKindVSCode {
		hasSyncedApp = strings.TrimSpace(settings.CodexSyncedVSCodeBuild) != ""
		autoApp = firstNonEmptyString(settings.CodexSyncedVSCodeBuild, autoApp)
	} else if strings.EqualFold(choice.OSName, "Windows") {
		hasSyncedApp = strings.TrimSpace(settings.CodexSyncedDesktopWindowsBuild) != ""
		autoApp = firstNonEmptyString(settings.CodexSyncedDesktopWindowsBuild, autoApp)
	} else if strings.EqualFold(choice.OSName, "Mac OS") {
		hasSyncedApp = strings.TrimSpace(settings.CodexSyncedDesktopMacBuild) != ""
		autoApp = firstNonEmptyString(settings.CodexSyncedDesktopMacBuild, autoApp)
	}
	if !hasSyncedApp && choice.CLIOverride != "" && choice.AppOverride == "" {
		_, autoApp = resolveCodexVersionPair(spec, choice.CLIOverride, "", choice.VersionFloor)
	}
	cli := effectiveCodexClientVersion(firstNonEmptyString(choice.CLIOverride, autoCLI), choice.VersionFloor)
	return cli, firstNonEmptyString(choice.AppOverride, autoApp)
}

// heaviestCodexPair 返回观测占比最高的配对,作为该形态的默认版本(VS Code 插件主力
// 停在 0.153.0 而非 CLI 最新版,按"最新"取默认会选到罕见组合)。
func heaviestCodexPair(pairs []codexUAVersionPair) codexUAVersionPair {
	best := pairs[0]
	for _, p := range pairs[1:] {
		if p.Weight > best.Weight {
			best = p
		}
	}
	return best
}

// closestCodexPairAtLeast 返回不低于 version 的最小已知配对。
func closestCodexPairAtLeast(pairs []codexUAVersionPair, version string) (codexUAVersionPair, bool) {
	var best codexUAVersionPair
	found := false
	for _, p := range pairs {
		if !codexVersionAtLeast(p.CLIVersion, version) {
			continue
		}
		if !found {
			best, found = p, true
			continue
		}
		if cmp, ok := compareCodexClientVersions(p.CLIVersion, best.CLIVersion); ok && cmp < 0 {
			best = p
		}
	}
	return best, found
}

// codexPoolMix 返回生效的号池配比;未配置时用默认配比。
func codexPoolMix(cfg CodexUserAgentConfig) []codexUAWeighted {
	mix := cfg.PoolMix
	if len(mix) == 0 {
		mix = map[string]int{}
		for kind, w := range defaultCodexUAPoolMix {
			mix[string(kind)] = w
		}
	}
	keys := make([]string, 0, len(mix))
	for k := range mix {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]codexUAWeighted, 0, len(keys))
	for _, k := range keys {
		if mix[k] > 0 {
			out = append(out, codexUAWeighted{Value: k, Weight: mix[k]})
		}
	}
	return out
}

// codexPoolPersona 按账号确定性地从目录抽取一套完整画像:同一账号永远得到同一画像,
// 不同账号按配比与真实分布散开。
func codexPoolPersona(cfg CodexUserAgentConfig, accountID int64, versionFloor string) (userAgent, version string, ok bool) {
	mix := codexPoolMix(cfg)
	if len(mix) == 0 {
		return "", "", false
	}
	seed := fmt.Sprintf("%d", accountID)
	kind := CodexClientKind(pickCodexUAWeighted(mix, "kind:"+seed))
	spec, found := codexUAKindSpecFor(kind)
	if !found {
		return "", "", false
	}
	platform := pickCodexUAPlatform(spec.Platforms, "platform:"+seed)
	terminal := pickCodexUAWeighted(spec.Terminals, "terminal:"+seed)
	appName := spec.ClientName
	if !spec.AppFollowsCLI {
		appName = pickCodexUAWeighted(spec.AppNames, "app:"+seed)
	}
	fallback := codexUAVersionPair{}
	if len(spec.VersionPairs) > 0 {
		fallback = heaviestCodexPair(spec.VersionPairs)
		if pair, picked := pickCodexUAVersionPair(spec.VersionPairs, "version:"+seed); picked {
			fallback = pair
		}
	}
	cliVersion, appVersion := resolveCodexCurrentVersions(spec, codexVersionSelection{OSName: platform.OSName, VersionFloor: versionFloor, Fallback: fallback})
	return formatCodexUserAgentWithApp(spec.ClientName, cliVersion, platform.OSName, platform.OSVersion, platform.Arch, terminal, appName, appVersion), cliVersion, true
}

// ==================== 管理端视图 ====================

type CodexUserAgentCatalogOption struct {
	Value  string `json:"value"`
	Weight int    `json:"weight"`
}

type CodexUserAgentCatalogPlatform struct {
	OSName    string `json:"os_name"`
	OSVersion string `json:"os_version"`
	Arch      string `json:"arch"`
	Weight    int    `json:"weight"`
}

type CodexUserAgentCatalogVersionPair struct {
	CLIVersion string `json:"cli_version"`
	AppVersion string `json:"app_version"`
	Weight     int    `json:"weight"`
}

type CodexUserAgentCatalogKind struct {
	Kind            string                             `json:"kind"`
	ClientName      string                             `json:"client_name"`
	AppFollowsCLI   bool                               `json:"app_follows_cli"`
	DefaultAppName  string                             `json:"default_app_name"`
	DefaultPlatform CodexUserAgentCatalogPlatform      `json:"default_platform"`
	DefaultTerminal string                             `json:"default_terminal"`
	AppNames        []CodexUserAgentCatalogOption      `json:"app_names"`
	Terminals       []CodexUserAgentCatalogOption      `json:"terminals"`
	Platforms       []CodexUserAgentCatalogPlatform    `json:"platforms"`
	VersionPairs    []CodexUserAgentCatalogVersionPair `json:"version_pairs"`
}

type CodexUserAgentCatalogView struct {
	Kinds          []CodexUserAgentCatalogKind `json:"kinds"`
	DefaultPoolMix map[string]int              `json:"default_pool_mix"`
}

// CodexUserAgentCatalog 返回管理页构建搭配选择所需的目录快照。
func CodexUserAgentCatalog() CodexUserAgentCatalogView {
	view := CodexUserAgentCatalogView{DefaultPoolMix: map[string]int{}}
	for kind, w := range defaultCodexUAPoolMix {
		view.DefaultPoolMix[string(kind)] = w
	}
	for _, kind := range codexUAKindOrder {
		spec := codexUACatalog[kind]
		item := CodexUserAgentCatalogKind{
			Kind:            string(spec.Kind),
			ClientName:      spec.ClientName,
			AppFollowsCLI:   spec.AppFollowsCLI,
			DefaultAppName:  spec.ClientName,
			DefaultPlatform: CodexUserAgentCatalogPlatform{OSName: spec.DefaultPlatform.OSName, OSVersion: spec.DefaultPlatform.OSVersion, Arch: spec.DefaultPlatform.Arch},
			DefaultTerminal: spec.DefaultTerminal,
		}
		if !spec.AppFollowsCLI && len(spec.AppNames) > 0 {
			item.DefaultAppName = spec.AppNames[0].Value
		}
		for _, a := range spec.AppNames {
			item.AppNames = append(item.AppNames, CodexUserAgentCatalogOption{Value: a.Value, Weight: a.Weight})
		}
		for _, t := range spec.Terminals {
			item.Terminals = append(item.Terminals, CodexUserAgentCatalogOption{Value: t.Value, Weight: t.Weight})
		}
		for _, p := range spec.Platforms {
			item.Platforms = append(item.Platforms, CodexUserAgentCatalogPlatform{OSName: p.OSName, OSVersion: p.OSVersion, Arch: p.Arch, Weight: p.Weight})
		}
		for _, p := range spec.VersionPairs {
			item.VersionPairs = append(item.VersionPairs, CodexUserAgentCatalogVersionPair{CLIVersion: p.CLIVersion, AppVersion: p.AppVersion, Weight: p.Weight})
		}
		view.Kinds = append(view.Kinds, item)
	}
	return view
}

type CodexUserAgentPersona struct {
	Label      string `json:"label,omitempty"`
	AccountID  int64  `json:"account_id,omitempty"`
	UserAgent  string `json:"user_agent"`
	Originator string `json:"originator"`
	Version    string `json:"version"`
}

type CodexUserAgentPreview struct {
	Mode       string                  `json:"mode"`
	Kind       string                  `json:"kind,omitempty"`
	Persona    *CodexUserAgentPersona  `json:"persona,omitempty"`
	Samples    []CodexUserAgentPersona `json:"samples,omitempty"`
	Warnings   []string                `json:"warnings,omitempty"`
	Normalized string                  `json:"normalized"`
}

// PreviewCodexUserAgentConfig 按当前配置算出真实出站身份(UA / Originator / Version),
// 号池模式下对给定账号逐个抽样。versionFloor 与执行链路同义(自动兼容模式下的最低 CLI 版本)。
func PreviewCodexUserAgentConfig(raw, versionFloor string, sampleAccountIDs []int64) (CodexUserAgentPreview, error) {
	normalized, err := NormalizeCodexUserAgentConfigJSON(raw)
	if err != nil {
		return CodexUserAgentPreview{}, err
	}
	cfg := codexUserAgentConfigFromJSON(normalized)
	preview := CodexUserAgentPreview{Mode: CodexUserAgentModeSingle, Normalized: normalized}
	if cfg.Mode == CodexUserAgentModePool {
		preview.Mode = CodexUserAgentModePool
		if len(sampleAccountIDs) == 0 {
			sampleAccountIDs = []int64{1, 2, 3, 4, 5, 6}
		}
		for i, id := range sampleAccountIDs {
			ua, version, ok := codexPoolPersona(cfg, id, versionFloor)
			if !ok {
				return CodexUserAgentPreview{}, errors.New("codex User-Agent pool_mix has no positive weights")
			}
			preview.Samples = append(preview.Samples, CodexUserAgentPersona{
				Label:      fmt.Sprintf("#%d", i+1),
				AccountID:  id,
				UserAgent:  ua,
				Originator: CodexOriginatorForGeneratedUserAgent(ua),
				Version:    version,
			})
		}
		return preview, nil
	}
	ua, version, ok := codexUserAgentFromConfig(normalized, 0, versionFloor)
	if !ok {
		// 空配置:走内置画像池的默认画像,预览按账号 0 展示。
		ua, version = generatedCodexClientHeaders(nil, RuntimeSettings{CodexUserAgentConfig: normalized, CodexMinCLIVersion: versionFloor, ClientCompatMode: ClientCompatModeAuto})
	}
	kind := effectiveCodexClientKind(cfg)
	preview.Kind = string(kind)
	preview.Persona = &CodexUserAgentPersona{UserAgent: ua, Originator: CodexOriginatorForGeneratedUserAgent(ua), Version: version}
	preview.Warnings = codexUserAgentComboWarnings(cfg, kind)
	return preview, nil
}

// codexUserAgentComboWarnings 提示目录里从未出现过的静态画像字段(不阻止保存)。
func codexUserAgentComboWarnings(cfg CodexUserAgentConfig, kind CodexClientKind) []string {
	spec, ok := codexUAKindSpecFor(kind)
	if !ok || cfg.RawUserAgent != "" {
		return nil
	}
	var warnings []string
	if term := strings.TrimSpace(cfg.Terminal); term != "" && !codexUAHasOption(spec.Terminals, term) {
		warnings = append(warnings, "terminal")
	}
	if name := strings.TrimSpace(cfg.AppName); name != "" && !codexUAHasOption(spec.AppNames, name) {
		warnings = append(warnings, "app_name")
	}
	if cfg.OSName != "" || cfg.OSVersion != "" || cfg.Arch != "" {
		platform := codexUAPlatform{
			OSName:    firstNonEmptyString(cfg.OSName, spec.DefaultPlatform.OSName),
			OSVersion: firstNonEmptyString(cfg.OSVersion, spec.DefaultPlatform.OSVersion),
			Arch:      firstNonEmptyString(cfg.Arch, spec.DefaultPlatform.Arch),
		}
		if !codexUAHasPlatform(spec.Platforms, platform) {
			warnings = append(warnings, "platform")
		}
	}
	return warnings
}

func codexUAHasOption(items []codexUAWeighted, value string) bool {
	for _, it := range items {
		if strings.EqualFold(it.Value, value) {
			return true
		}
	}
	return false
}

func codexUAHasPlatform(items []codexUAPlatform, platform codexUAPlatform) bool {
	for _, it := range items {
		if strings.EqualFold(it.OSName, platform.OSName) && it.OSVersion == platform.OSVersion && strings.EqualFold(it.Arch, platform.Arch) {
			return true
		}
	}
	return false
}
