package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/internal/version"
	"github.com/gin-gonic/gin"
)

var systemUpdateSources = map[string]string{"patched": "Egdon/codex2api-inject", "official": "james-6-23/codex2api"}
var errSystemUpdatePlan = errors.New("更新计划无效或过期，请重新检查更新并确认")
var systemCanonicalTag = regexp.MustCompile(`^(patched-v|v)(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// The lock is process-global, not per source or Handler. Once installed, another
// replacement is forbidden until process exit, even if automatic restart fails.
var systemUpdateGlobal struct {
	sync.Mutex
	restartPending bool
}

type systemSourceCache struct {
	release *systemGitHubRelease
	expires time.Time
}
type systemUpdatePlan struct {
	source, tag, fingerprint, identity, platform string
	expires                                      time.Time
}
type systemUpdateRequest struct {
	Source           string `json:"source"`
	TargetTag        string `json:"target_tag"`
	PlanToken        string `json:"plan_token"`
	ConfirmOfficial  bool   `json:"confirm_official"`
	ConfirmMigration bool   `json:"confirm_migration"`
}
type systemSourceReleaseClient interface {
	FetchLatestReleaseForSource(context.Context, string) (*systemGitHubRelease, error)
}

func canonicalSystemTag(source, tag string) ([3]int, bool) {
	var parts [3]int
	match := systemCanonicalTag.FindStringSubmatch(tag)
	if match == nil || (source != "patched" && source != "official") || (source == "patched") != (match[1] == "patched-v") {
		return parts, false
	}
	for i := range parts {
		n, err := strconv.Atoi(match[i+2])
		if err != nil {
			return parts, false
		}
		parts[i] = n
	}
	return parts, true
}
func currentSystemTag(source, current string) ([3]int, bool) {
	// Official release ldflags historically omit the v prefix. Never normalize targets.
	if source == "official" && !strings.HasPrefix(current, "v") {
		current = "v" + current
	}
	return canonicalSystemTag(source, current)
}

type systemTaggedReleaseClient interface {
	FetchReleaseByTag(context.Context, string, string) (*systemGitHubRelease, error)
}

func (u *systemUpdater) fetchPlannedRelease(ctx context.Context, source, tag string) (*systemGitHubRelease, error) {
	if client, ok := u.client.(systemTaggedReleaseClient); ok {
		return client.FetchReleaseByTag(ctx, source, tag)
	}
	// Legacy fakes may only supply latest; fingerprint checking still fails closed.
	return u.fetchSourceRelease(ctx, source, true)
}
func (u *systemUpdater) source() string {
	if u.currentSource == "" {
		return "patched"
	}
	return u.currentSource
}
func (u *systemUpdater) identity() string {
	return u.source() + "\x00" + u.currentVersion + "\x00" + version.UpstreamBase + "\x00" + version.Revision
}
func (u *systemUpdater) baseSourceInfo(source string) *systemUpdateInfo {
	_, canonical := currentSystemTag(u.source(), u.currentVersion)
	info := &systemUpdateInfo{CurrentVersion: u.currentVersion, CurrentSource: u.source(), CurrentLocal: !canonical, Source: source, RuntimeOS: u.goos, RuntimeArch: u.goarch, Mode: "binary", Supported: (u.goos == "linux" || u.goos == "darwin") && (u.goarch == "amd64" || u.goarch == "arm64"), RequiresOfficialConfirmation: source == "official", RequiresMigrationConfirmation: !canonical || source != u.source()}
	if !info.Supported {
		info.UnsupportedReason = "仅支持 Linux/macOS 的 amd64/arm64 在线更新"
	}
	if u.runningInContainer != nil && u.runningInContainer() {
		info.Warning = "容器内更新仅替换当前二进制，容器重建后会恢复镜像版本；建议更新镜像。"
	}
	return info
}
func (u *systemUpdater) unavailableSourceInfo(source string) *systemUpdateInfo {
	info := u.baseSourceInfo(source)
	info.Status = "unavailable"
	info.Supported = false
	info.UnsupportedReason = "更新源暂时不可用，请稍后重试"
	return info
}
func (h *Handler) GetSystemBuild(c *gin.Context) {
	u := h.systemUpdater()
	_, canonical := currentSystemTag(u.source(), u.currentVersion)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"version": u.currentVersion, "source": u.source(), "upstream_base": version.UpstreamBase, "revision": version.Revision, "local": !canonical, "runtime_os": runtime.GOOS, "runtime_arch": runtime.GOARCH})
}
func (u *systemUpdater) fetchSourceRelease(ctx context.Context, source string, fresh bool) (*systemGitHubRelease, error) {
	if _, ok := systemUpdateSources[source]; !ok {
		return nil, fmt.Errorf("invalid source")
	}
	u.releaseCacheMu.Lock()
	defer u.releaseCacheMu.Unlock()
	if u.sourceCaches == nil {
		u.sourceCaches = make(map[string]systemSourceCache)
	}
	if cached, ok := u.sourceCaches[source]; !fresh && ok && time.Now().Before(cached.expires) {
		return cloneSystemGitHubRelease(cached.release), nil
	}
	var release *systemGitHubRelease
	var err error
	if client, ok := u.client.(systemSourceReleaseClient); ok {
		release, err = client.FetchLatestReleaseForSource(ctx, source)
	} else if source == "patched" {
		release, err = u.client.FetchLatestRelease(ctx)
	} else {
		err = fmt.Errorf("client does not support requested source")
	}
	if err != nil {
		delete(u.sourceCaches, source)
		return nil, err
	}
	if release == nil {
		delete(u.sourceCaches, source)
		return nil, fmt.Errorf("empty release")
	}
	u.sourceCaches[source] = systemSourceCache{cloneSystemGitHubRelease(release), time.Now().Add(systemUpdateReleaseCacheTTL)}
	return cloneSystemGitHubRelease(release), nil
}
func systemReleaseFingerprint(release *systemGitHubRelease) string {
	data, _ := json.Marshal(release)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func validateSystemAssetURL(asset *systemGitHubAsset, source, tag string) error {
	repo, ok := systemUpdateSources[source]
	if !ok || asset == nil {
		return fmt.Errorf("invalid source or asset")
	}
	if err := validateSystemUpdateURL(asset.BrowserDownloadURL); err != nil {
		return err
	}
	parsed, err := url.Parse(asset.BrowserDownloadURL)
	if err != nil {
		return err
	}
	expected := "/" + repo + "/releases/download/" + tag + "/" + asset.Name
	if parsed.Host != "github.com" || parsed.Path != expected || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery {
		return fmt.Errorf("asset must belong to selected repository and tag")
	}
	return nil
}
func exactSystemAsset(release *systemGitHubRelease, name string) *systemGitHubAsset {
	var found *systemGitHubAsset
	for i := range release.Assets {
		if release.Assets[i].Name == name {
			if found != nil {
				return nil
			}
			found = &release.Assets[i]
		}
	}
	return found
}
func (u *systemUpdater) inspectSourceRelease(source string, release *systemGitHubRelease) (*systemUpdateInspection, error) {
	latest, ok := canonicalSystemTag(source, release.TagName)
	if !ok || release.Draft || release.Prerelease {
		return nil, fmt.Errorf("release is not a canonical stable %s release", source)
	}
	info := u.baseSourceInfo(source)
	info.LatestVersion = release.TagName
	info.TargetTag = release.TagName
	info.ReleaseURL = "https://github.com/" + systemUpdateSources[source] + "/releases/tag/" + release.TagName
	info.PublishedAt = release.PublishedAt
	info.Status = "latest"
	if info.RequiresMigrationConfirmation {
		info.Status = "migration"
	} else {
		current, _ := currentSystemTag(source, u.currentVersion)
		for i := range current {
			if current[i] != latest[i] {
				info.HasUpdate = current[i] < latest[i]
				break
			}
		}
		if info.HasUpdate {
			info.Status = "available"
		}
	}
	archiveVersion := release.TagName
	if source == "official" {
		archiveVersion = strings.TrimPrefix(archiveVersion, "v")
	}
	name := fmt.Sprintf("codex2api_%s_%s_%s.tar.gz", archiveVersion, u.goos, u.goarch)
	asset := exactSystemAsset(release, name)
	checksum := exactSystemAsset(release, "SHA256SUMS.txt")
	if asset == nil {
		info.Supported = false
		info.UnsupportedReason = "未找到平台更新包"
	} else {
		if err := validateSystemAssetURL(asset, source, release.TagName); err != nil {
			return nil, err
		}
		info.AssetName = asset.Name
		if asset.Size <= 0 || asset.Size > systemUpdateMaxDownloadBytes {
			return nil, fmt.Errorf("invalid release asset size")
		}
		digest, err := hex.DecodeString(strings.TrimPrefix(asset.Digest, "sha256:"))
		if checksum == nil && (!strings.HasPrefix(asset.Digest, "sha256:") || err != nil || len(digest) != 32) {
			return nil, fmt.Errorf("missing SHA256 verification")
		}
	}
	if checksum != nil {
		if err := validateSystemAssetURL(checksum, source, release.TagName); err != nil {
			return nil, err
		}
	}
	return &systemUpdateInspection{info: info, asset: asset, checksumAsset: checksum}, nil
}
func (u *systemUpdater) inspectSource(ctx context.Context, source string, fresh bool) (*systemUpdateInspection, error) {
	release, err := u.fetchSourceRelease(ctx, source, fresh)
	if err != nil {
		return nil, err
	}
	return u.inspectSourceRelease(source, release)
}
func (u *systemUpdater) CheckSource(ctx context.Context, source string) (*systemUpdateInfo, error) {
	release, err := u.fetchSourceRelease(ctx, source, false)
	if err != nil {
		return nil, err
	}
	inspection, err := u.inspectSourceRelease(source, release)
	if err != nil {
		return nil, err
	}
	info := inspection.info
	if !info.Supported || info.Status == "latest" {
		return info, nil
	}
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(entropy)
	expires := time.Now().Add(5 * time.Minute)
	u.releaseCacheMu.Lock()
	defer u.releaseCacheMu.Unlock()
	if u.plans == nil {
		u.plans = make(map[string]systemUpdatePlan)
	}
	for key, plan := range u.plans {
		if time.Now().After(plan.expires) {
			delete(u.plans, key)
		}
	}
	// Bound memory even if clients repeatedly request plans without using them.
	if len(u.plans) >= 32 {
		var oldest string
		var deadline time.Time
		for key, plan := range u.plans {
			if oldest == "" || plan.expires.Before(deadline) {
				oldest = key
				deadline = plan.expires
			}
		}
		delete(u.plans, oldest)
	}
	u.plans[token] = systemUpdatePlan{source, release.TagName, systemReleaseFingerprint(release), u.identity(), u.goos + "/" + u.goarch, expires}
	info.PlanToken = token
	info.PlanExpiresAt = expires.UTC().Format(time.RFC3339)
	return info, nil
}
func (u *systemUpdater) PerformPlannedUpdate(ctx context.Context, request systemUpdateRequest) (*systemUpdateResult, error) {
	if _, ok := systemUpdateSources[request.Source]; !ok {
		return nil, errSystemUpdatePlan
	}
	if request.TargetTag == "" || request.PlanToken == "" {
		return nil, errSystemUpdatePlan
	}
	if !systemUpdateGlobal.TryLock() {
		return nil, errSystemUpdateBusy
	}
	defer systemUpdateGlobal.Unlock()
	if systemUpdateGlobal.restartPending {
		return nil, errSystemUpdateBusy
	}
	u.releaseCacheMu.Lock()
	plan, ok := u.plans[request.PlanToken]
	delete(u.plans, request.PlanToken)
	u.releaseCacheMu.Unlock()
	if !ok || time.Now().After(plan.expires) || plan.source != request.Source || plan.tag != request.TargetTag || plan.identity != u.identity() || plan.platform != u.goos+"/"+u.goarch {
		return nil, errSystemUpdatePlan
	}
	info := u.baseSourceInfo(request.Source)
	if info.RequiresOfficialConfirmation && !request.ConfirmOfficial || info.RequiresMigrationConfirmation && !request.ConfirmMigration {
		return nil, fmt.Errorf("%w: explicit confirmation required", errSystemUpdatePlan)
	}
	release, err := u.fetchPlannedRelease(ctx, request.Source, request.TargetTag)
	if err != nil {
		return nil, err
	}
	if release == nil || time.Now().After(plan.expires) || systemReleaseFingerprint(release) != plan.fingerprint {
		return nil, errSystemUpdatePlan
	}
	inspection, err := u.inspectSourceRelease(request.Source, release)
	if err != nil {
		return nil, err
	}
	if !inspection.info.Supported {
		return nil, fmt.Errorf("%w: %s", errSystemUpdateUnsupported, inspection.info.UnsupportedReason)
	}
	if inspection.info.Status == "latest" {
		return nil, errSystemUpdateLatest
	}
	exe, backup, err := u.applyBinaryUpdate(ctx, inspection)
	if err != nil {
		return nil, err
	}
	systemUpdateGlobal.restartPending = true
	u.scheduleRestart(exe)
	return &systemUpdateResult{Message: "更新已应用，服务正在重启", CurrentVersion: u.currentVersion, LatestVersion: request.TargetTag, NeedRestart: true, Restarting: u.restartProcess != nil, Mode: "binary", BackupPath: backup}, nil
}
