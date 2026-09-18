package admin

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/internal/version"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

const (
	systemUpdateRepo             = "Egdon/codex2api-inject"
	systemUpdateUserAgent        = "Codex2API-Updater"
	systemUpdateMaxDownloadBytes = 200 * 1024 * 1024
	systemUpdateRestartDelay     = 900 * time.Millisecond
	systemUpdateReleaseCacheTTL  = 2 * time.Minute
)

var (
	errSystemUpdateBusy        = errors.New("已有更新任务正在执行")
	errSystemUpdateLatest      = errors.New("当前已是最新版本")
	errSystemUpdateUnsupported = errors.New("当前运行环境不支持在线更新")
)

type systemUpdater struct {
	currentSource      string
	currentVersion     string
	client             systemReleaseClient
	goos               string
	goarch             string
	executablePath     func() (string, error)
	restartProcess     func(string) error
	restartDelay       time.Duration
	runningInContainer func() bool

	sourceCaches          map[string]systemSourceCache
	plans                 map[string]systemUpdatePlan
	mu                    sync.Mutex
	releaseCacheMu        sync.Mutex
	releaseCache          *systemGitHubRelease
	releaseCacheExpiresAt time.Time
}

type systemReleaseClient interface {
	FetchLatestRelease(ctx context.Context) (*systemGitHubRelease, error)
	DownloadFile(ctx context.Context, rawURL, dest string, maxSize int64) error
	FetchText(ctx context.Context, rawURL string, maxSize int64) ([]byte, error)
}

type systemUpdateInfo struct {
	Source                        string `json:"source"`
	CurrentSource                 string `json:"current_source"`
	CurrentLocal                  bool   `json:"current_local"`
	Status                        string `json:"status"`
	TargetTag                     string `json:"target_tag,omitempty"`
	PlanToken                     string `json:"plan_token,omitempty"`
	PlanExpiresAt                 string `json:"plan_expires_at,omitempty"`
	RequiresOfficialConfirmation  bool   `json:"requires_official_confirmation"`
	RequiresMigrationConfirmation bool   `json:"requires_migration_confirmation"`
	CurrentVersion                string `json:"current_version"`
	LatestVersion                 string `json:"latest_version"`
	HasUpdate                     bool   `json:"has_update"`
	Supported                     bool   `json:"supported"`
	UnsupportedReason             string `json:"unsupported_reason,omitempty"`
	RuntimeOS                     string `json:"runtime_os"`
	RuntimeArch                   string `json:"runtime_arch"`
	Mode                          string `json:"mode"`
	ReleaseURL                    string `json:"release_url,omitempty"`
	AssetName                     string `json:"asset_name,omitempty"`
	PublishedAt                   string `json:"published_at,omitempty"`
	Warning                       string `json:"warning,omitempty"`
}

type systemUpdateResult struct {
	Message        string `json:"message"`
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version"`
	NeedRestart    bool   `json:"need_restart"`
	Restarting     bool   `json:"restarting"`
	Mode           string `json:"mode"`
	BackupPath     string `json:"backup_path,omitempty"`
}

type systemUpdateInspection struct {
	info          *systemUpdateInfo
	asset         *systemGitHubAsset
	checksumAsset *systemGitHubAsset
}

type systemGitHubRelease struct {
	ID          int64               `json:"id"`
	Draft       bool                `json:"draft"`
	Prerelease  bool                `json:"prerelease"`
	TagName     string              `json:"tag_name"`
	Name        string              `json:"name"`
	Body        string              `json:"body"`
	PublishedAt string              `json:"published_at"`
	HTMLURL     string              `json:"html_url"`
	Assets      []systemGitHubAsset `json:"assets"`
}

type systemGitHubAsset struct {
	ID                 int64  `json:"id"`
	UpdatedAt          string `json:"updated_at"`
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest"`
}

type defaultSystemReleaseClient struct {
	apiClient      *http.Client
	downloadClient *http.Client
}

func newSystemUpdater() *systemUpdater {
	client := newDefaultSystemReleaseClient()
	return &systemUpdater{
		currentVersion:     version.Current(),
		currentSource:      version.Source,
		client:             client,
		goos:               runtime.GOOS,
		goarch:             runtime.GOARCH,
		executablePath:     os.Executable,
		restartProcess:     defaultRestartProcess,
		restartDelay:       systemUpdateRestartDelay,
		runningInContainer: detectRunningInContainer,
	}
}

// detectRunningInContainer 尽力判断当前进程是否运行在容器内:
// 更新容器内的二进制在容器重建后会被镜像版本覆盖,需要提示用户改用镜像升级。
func detectRunningInContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil { // podman
		return true
	}
	data, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	content := string(data)
	return strings.Contains(content, "docker") || strings.Contains(content, "kubepods") || strings.Contains(content, "containerd")
}

func newDefaultSystemReleaseClient() *defaultSystemReleaseClient {
	redirectPolicy := func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		if err := validateSystemUpdateURL(req.URL.String()); err != nil {
			return err
		}
		if len(via) > 0 && via[0].URL.Hostname() == "api.github.com" {
			return fmt.Errorf("API redirects are not allowed")
		}
		if req.URL.Hostname() != "objects.githubusercontent.com" && req.URL.Hostname() != "release-assets.githubusercontent.com" {
			return fmt.Errorf("unexpected release redirect")
		}
		return nil
	}
	// GitHub 专用代理（issue #522）：配置了则优先，否则维持原行为（环境变量代理）。
	// 每请求动态解析，设置热更新即时生效。
	proxyFunc := func(req *http.Request) (*url.URL, error) {
		if req != nil && req.URL != nil {
			if dedicated := proxy.GithubProxyOrDefault(req.URL.String(), ""); dedicated != "" {
				return url.Parse(dedicated)
			}
		}
		return http.ProxyFromEnvironment(req)
	}
	return &defaultSystemReleaseClient{
		apiClient: &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: redirectPolicy,
			Transport:     &http.Transport{Proxy: proxyFunc},
		},
		downloadClient: &http.Client{
			Timeout:       10 * time.Minute,
			CheckRedirect: redirectPolicy,
			Transport:     &http.Transport{Proxy: proxyFunc},
		},
	}
}

func (h *Handler) GetSystemUpdate(c *gin.Context) {
	source := c.DefaultQuery("source", "patched")
	if _, ok := systemUpdateSources[source]; !ok {
		writeError(c, http.StatusBadRequest, "invalid source")
		return
	}
	c.Header("Cache-Control", "no-store")
	updater := h.systemUpdater()
	info, err := updater.CheckSource(c.Request.Context(), source)
	if err != nil {
		info = updater.unavailableSourceInfo(source)
		log.Printf("检查系统更新失败 (%s): %v", source, err)
	}
	c.JSON(http.StatusOK, info)
}

func (u *systemUpdater) unavailableInfo() *systemUpdateInfo {
	return u.unavailableSourceInfo("patched")
}

func (h *Handler) PerformSystemUpdate(c *gin.Context) {
	var request systemUpdateRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if err := c.ShouldBindJSON(&request); err != nil {
		writeError(c, http.StatusBadRequest, "invalid update request")
		return
	}
	result, err := h.systemUpdater().PerformPlannedUpdate(c.Request.Context(), request)
	if err == nil {
		c.JSON(http.StatusOK, result)
		return
	}
	switch {
	case errors.Is(err, errSystemUpdateBusy), errors.Is(err, errSystemUpdateLatest), errors.Is(err, errSystemUpdatePlan):
		writeError(c, http.StatusConflict, err.Error())
	case errors.Is(err, errSystemUpdateUnsupported):
		writeError(c, http.StatusBadRequest, err.Error())
	default:
		log.Printf("在线更新失败: %v", err)
		writeError(c, http.StatusInternalServerError, "在线更新失败，请重新检查更新或查看服务日志")
	}
}

func (h *Handler) systemUpdater() *systemUpdater {
	h.systemUpdateOnce.Do(func() {
		if h.systemUpdate == nil {
			h.systemUpdate = newSystemUpdater()
		}
	})
	return h.systemUpdate
}

// Legacy method names remain for package test compilation; updates require an explicit plan.
func (u *systemUpdater) Check(ctx context.Context) (*systemUpdateInfo, error) {
	return u.CheckSource(ctx, "patched")
}
func (u *systemUpdater) PerformUpdate(ctx context.Context) (*systemUpdateResult, error) {
	if !u.mu.TryLock() {
		return nil, errSystemUpdateBusy
	}
	u.mu.Unlock()
	return nil, errSystemUpdatePlan
}
func (u *systemUpdater) inspect(ctx context.Context) (*systemUpdateInspection, error) {
	return u.inspectSource(ctx, "patched", false)
}
func (u *systemUpdater) fetchLatestRelease(ctx context.Context) (*systemGitHubRelease, error) {
	return u.fetchSourceRelease(ctx, "patched", false)
}

func cloneSystemGitHubRelease(release *systemGitHubRelease) *systemGitHubRelease {
	if release == nil {
		return nil
	}
	cloned := *release
	cloned.Assets = append([]systemGitHubAsset(nil), release.Assets...)
	return &cloned
}

func (u *systemUpdater) applyBinaryUpdate(ctx context.Context, inspection *systemUpdateInspection) (string, string, error) {
	asset := inspection.asset
	if asset == nil {
		return "", "", fmt.Errorf("更新资产为空")
	}
	if err := validateSystemAssetURL(asset, inspection.info.Source, inspection.info.TargetTag); err != nil {
		return "", "", fmt.Errorf("发布资产 URL 不可信: %w", err)
	}
	if inspection.checksumAsset != nil {
		if err := validateSystemAssetURL(inspection.checksumAsset, inspection.info.Source, inspection.info.TargetTag); err != nil {
			return "", "", fmt.Errorf("校验和 URL 不可信: %w", err)
		}
	}

	exePath, err := u.executablePath()
	if err != nil {
		return "", "", fmt.Errorf("获取当前可执行文件路径失败: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	exeDir := filepath.Dir(exePath)

	tempDir, err := os.MkdirTemp(exeDir, ".codex2api-update-*")
	if err != nil {
		return "", "", fmt.Errorf("创建更新临时目录失败: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	archivePath := filepath.Join(tempDir, filepath.Base(asset.Name))
	if err := u.client.DownloadFile(ctx, asset.BrowserDownloadURL, archivePath, systemUpdateMaxDownloadBytes); err != nil {
		return "", "", fmt.Errorf("下载更新包失败: %w", err)
	}
	if stat, err := os.Stat(archivePath); err != nil || stat.Size() != asset.Size {
		return "", "", fmt.Errorf("downloaded asset size differs from release metadata")
	}
	if err := verifySystemUpdateChecksum(ctx, u.client, archivePath, asset, inspection.checksumAsset); err != nil {
		return "", "", err
	}

	newBinaryPath := filepath.Join(tempDir, systemBinaryName(u.goos))
	if err := extractSystemUpdateBinary(archivePath, newBinaryPath); err != nil {
		return "", "", fmt.Errorf("解压更新包失败: %w", err)
	}
	if err := validateSystemBinaryPlatform(newBinaryPath, u.goos, u.goarch); err != nil {
		return "", "", err
	}
	if err := os.Chmod(newBinaryPath, 0755); err != nil {
		return "", "", fmt.Errorf("设置新程序执行权限失败: %w", err)
	}

	backupPath := exePath + ".backup"
	if err := replaceExecutable(exePath, newBinaryPath, backupPath); err != nil {
		return "", "", err
	}
	return exePath, backupPath, nil
}

func (u *systemUpdater) scheduleRestart(exePath string) {
	restart := u.restartProcess
	if restart == nil {
		return
	}
	delay := u.restartDelay
	if delay < 0 {
		delay = 0
	}
	go func() {
		time.Sleep(delay)
		if err := restart(exePath); err != nil {
			log.Printf("在线更新后重启失败: %v", err)
		}
	}()
}

func (c *defaultSystemReleaseClient) FetchLatestRelease(ctx context.Context) (*systemGitHubRelease, error) {
	return c.FetchLatestReleaseForSource(ctx, "patched")
}

func (c *defaultSystemReleaseClient) FetchLatestReleaseForSource(ctx context.Context, source string) (*systemGitHubRelease, error) {
	repo, ok := systemUpdateSources[source]
	if !ok {
		return nil, fmt.Errorf("invalid source")
	}
	apiURL := "https://api.github.com/repos/" + repo + "/releases/latest"
	return c.fetchReleaseURL(ctx, apiURL)
}

func (c *defaultSystemReleaseClient) FetchReleaseByTag(ctx context.Context, source, tag string) (*systemGitHubRelease, error) {
	repo, ok := systemUpdateSources[source]
	if _, canonical := canonicalSystemTag(source, tag); !ok || !canonical {
		return nil, errSystemUpdatePlan
	}
	return c.fetchReleaseURL(ctx, "https://api.github.com/repos/"+repo+"/releases/tags/"+tag)
}

func (c *defaultSystemReleaseClient) fetchReleaseURL(ctx context.Context, apiURL string) (*systemGitHubRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", systemUpdateUserAgent)
	proxy.ApplyGithubAuth(req)

	resp, err := c.apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API 返回 HTTP %d", resp.StatusCode)
	}

	var release systemGitHubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024*1024)).Decode(&release); err != nil {
		return nil, err
	}
	return &release, nil
}

func (c *defaultSystemReleaseClient) DownloadFile(ctx context.Context, rawURL, dest string, maxSize int64) error {
	if err := validateSystemUpdateURL(rawURL); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", systemUpdateUserAgent)

	resp, err := c.downloadClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载返回 HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxSize {
		return fmt.Errorf("下载文件过大: %d bytes", resp.ContentLength)
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	limited := io.LimitReader(resp.Body, maxSize+1)
	written, copyErr := io.Copy(out, limited)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dest)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(dest)
		return closeErr
	}
	if written > maxSize {
		_ = os.Remove(dest)
		return fmt.Errorf("下载超过大小上限: %d bytes", maxSize)
	}
	return nil
}

func (c *defaultSystemReleaseClient) FetchText(ctx context.Context, rawURL string, maxSize int64) ([]byte, error) {
	if err := validateSystemUpdateURL(rawURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", systemUpdateUserAgent)

	resp, err := c.apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载校验和返回 HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxSize {
		return nil, fmt.Errorf("校验和文件超过大小上限: %d bytes", maxSize)
	}
	return data, nil
}

func validateSystemUpdateURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" || parsed.Fragment != "" || parsed.Host != parsed.Hostname() {
		return fmt.Errorf("仅允许 HTTPS")
	}
	host := strings.ToLower(parsed.Hostname())
	switch host {
	case "github.com", "api.github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com":
		return nil
	default:
		return fmt.Errorf("不允许的下载域名: %s", host)
	}
}

func findSystemUpdateAsset(release *systemGitHubRelease, latestVersion, goos, goarch string) *systemGitHubAsset {
	if release == nil {
		return nil
	}
	prefix := fmt.Sprintf("codex2api_%s_%s_%s", strings.TrimPrefix(latestVersion, "v"), goos, goarch)
	for i := range release.Assets {
		asset := &release.Assets[i]
		name := strings.ToLower(asset.Name)
		if !strings.HasPrefix(name, strings.ToLower(prefix)) {
			continue
		}
		if goos == "windows" {
			if strings.HasSuffix(name, ".zip") {
				return asset
			}
			continue
		}
		if strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") {
			return asset
		}
	}
	return nil
}

func findSystemChecksumAsset(release *systemGitHubRelease) *systemGitHubAsset {
	if release == nil {
		return nil
	}
	for i := range release.Assets {
		asset := &release.Assets[i]
		if strings.EqualFold(asset.Name, "SHA256SUMS.txt") || strings.EqualFold(asset.Name, "sha256sums.txt") {
			return asset
		}
	}
	return nil
}

func verifySystemUpdateChecksum(ctx context.Context, client systemReleaseClient, filePath string, asset *systemGitHubAsset, checksumAsset *systemGitHubAsset) error {
	actual, err := sha256File(filePath)
	if err != nil {
		return fmt.Errorf("计算更新包校验和失败: %w", err)
	}
	if asset != nil && strings.HasPrefix(strings.ToLower(asset.Digest), "sha256:") {
		expected := strings.TrimPrefix(strings.ToLower(asset.Digest), "sha256:")
		if actual != expected {
			return fmt.Errorf("更新包校验和不匹配: expected %s, got %s", expected, actual)
		}
		return nil
	}
	if checksumAsset == nil {
		return fmt.Errorf("release 未提供 SHA256 校验信息")
	}
	data, err := client.FetchText(ctx, checksumAsset.BrowserDownloadURL, 2*1024*1024)
	if err != nil {
		return fmt.Errorf("下载 SHA256SUMS.txt 失败: %w", err)
	}
	expected, ok := checksumForFile(data, filepath.Base(filePath))
	if !ok {
		return fmt.Errorf("SHA256SUMS.txt 中未找到 %s", filepath.Base(filePath))
	}
	if actual != expected {
		return fmt.Errorf("更新包校验和不匹配: expected %s, got %s", expected, actual)
	}
	return nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func checksumForFile(data []byte, name string) (string, bool) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	found := ""
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		digest, err := hex.DecodeString(fields[0])
		if err != nil || len(digest) != 32 || found != "" {
			return "", false
		}
		found = strings.ToLower(fields[0])
	}
	return found, scanner.Err() == nil && found != ""
}

// Read fixed-size headers rather than parsing attacker-controlled section tables.
func validateSystemBinaryPlatform(path, goos, goarch string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var header [64]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return fmt.Errorf("invalid executable header: %w", err)
	}
	valid := false
	if goos == "linux" {
		machine := elf.Machine(binary.LittleEndian.Uint16(header[18:20]))
		kind := elf.Type(binary.LittleEndian.Uint16(header[16:18]))
		valid = string(header[:4]) == "\x7fELF" && header[4] == byte(elf.ELFCLASS64) && header[5] == byte(elf.ELFDATA2LSB) && header[6] == 1 && (kind == elf.ET_EXEC || kind == elf.ET_DYN) && ((goarch == "amd64" && machine == elf.EM_X86_64) || (goarch == "arm64" && machine == elf.EM_AARCH64))
	} else if goos == "darwin" {
		cpu := macho.Cpu(binary.LittleEndian.Uint32(header[4:8]))
		valid = binary.LittleEndian.Uint32(header[:4]) == macho.Magic64 && macho.Type(binary.LittleEndian.Uint32(header[12:16])) == macho.TypeExec && ((goarch == "amd64" && cpu == macho.CpuAmd64) || (goarch == "arm64" && cpu == macho.CpuArm64))
	}
	if !valid {
		return fmt.Errorf("executable does not match %s/%s", goos, goarch)
	}
	return nil
}

func extractSystemUpdateBinary(archivePath, destPath string) (resultErr error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gzr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gzr.Close()
	// Bound all expanded data, including ignored files and trailing gzip data.
	limited := &io.LimitedReader{R: gzr, N: 2*systemUpdateMaxDownloadBytes + 1}
	tr := tar.NewReader(limited)
	found := false
	defer func() {
		if resultErr != nil {
			_ = os.Remove(destPath)
		}
	}()
	for entries := 0; ; entries++ {
		if entries > 10000 {
			return fmt.Errorf("too many archive entries")
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if strings.Contains(hdr.Name, "..") || strings.Contains(hdr.Name, "\\") || filepath.IsAbs(hdr.Name) {
			return fmt.Errorf("unsafe archive path")
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			return fmt.Errorf("unsupported archive entry type")
		}
		if filepath.Base(hdr.Name) != "codex2api" {
			continue
		}
		if hdr.Typeflag != tar.TypeReg || found {
			return fmt.Errorf("duplicate or invalid executable entry")
		}
		if hdr.Size <= 0 || hdr.Size > systemUpdateMaxDownloadBytes {
			return fmt.Errorf("invalid executable size")
		}
		out, err := os.OpenFile(destPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		written, copyErr := io.Copy(out, tr)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if written != hdr.Size {
			return fmt.Errorf("incomplete executable")
		}
		found = true
	}
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return err
	}
	if limited.N == 0 {
		return fmt.Errorf("expanded archive exceeds limit")
	}
	if !found {
		return fmt.Errorf("更新包内未找到 codex2api 程序")
	}
	return nil
}

func replaceExecutable(currentPath, newPath, backupPath string) error {
	_ = os.Remove(backupPath)
	if err := os.Rename(currentPath, backupPath); err != nil {
		return fmt.Errorf("备份当前程序失败: %w", err)
	}
	if err := os.Rename(newPath, currentPath); err != nil {
		if restoreErr := os.Rename(backupPath, currentPath); restoreErr != nil {
			return fmt.Errorf("替换程序失败且恢复备份失败: %w (restore: %v)", err, restoreErr)
		}
		return fmt.Errorf("替换程序失败，已恢复旧版本: %w", err)
	}
	return nil
}

func systemBinaryName(goos string) string {
	if goos == "windows" {
		return "codex2api.exe"
	}
	return "codex2api"
}

func normalizeSystemVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "refs/tags/")
	v = strings.TrimPrefix(v, "V")
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return ""
	}
	return v
}

func compareSystemVersions(a, b string) int {
	av, okA := parseSystemVersion(a)
	bv, okB := parseSystemVersion(b)
	if !okA && !okB {
		return strings.Compare(a, b)
	}
	if !okA {
		return -1
	}
	if !okB {
		return 1
	}
	for i := 0; i < 3; i++ {
		if av[i] < bv[i] {
			return -1
		}
		if av[i] > bv[i] {
			return 1
		}
	}
	return 0
}

func parseSystemVersion(v string) ([3]int, bool) {
	var result [3]int
	v = normalizeSystemVersion(v)
	if idx := strings.IndexAny(v, "+-"); idx >= 0 {
		v = v[:idx]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return result, false
	}
	for i := 0; i < len(parts); i++ {
		if parts[i] == "" {
			return result, false
		}
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return result, false
		}
		result[i] = n
	}
	return result, true
}
