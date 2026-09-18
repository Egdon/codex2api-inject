package admin

import (
	"context"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests deliberately stop before binary replacement or restarting.
type dualSourceFake struct {
	byTag    map[string]*systemGitHubRelease
	tagCalls int
	releases map[string]*systemGitHubRelease
	calls    map[string]int
	fail     map[string]bool
}

func (f *dualSourceFake) FetchLatestRelease(ctx context.Context) (*systemGitHubRelease, error) {
	return f.FetchLatestReleaseForSource(ctx, "patched")
}
func (f *dualSourceFake) FetchLatestReleaseForSource(_ context.Context, source string) (*systemGitHubRelease, error) {
	f.calls[source]++
	if f.fail[source] {
		return nil, fmt.Errorf("offline")
	}
	return cloneSystemGitHubRelease(f.releases[source]), nil
}
func (f *dualSourceFake) FetchReleaseByTag(_ context.Context, source, tag string) (*systemGitHubRelease, error) {
	f.tagCalls++
	if f.byTag != nil {
		return cloneSystemGitHubRelease(f.byTag[source+"/"+tag]), nil
	}
	return cloneSystemGitHubRelease(f.releases[source]), nil
}
func (*dualSourceFake) DownloadFile(context.Context, string, string, int64) error {
	return errors.New("unexpected download")
}
func (*dualSourceFake) FetchText(context.Context, string, int64) ([]byte, error) {
	return nil, errors.New("unexpected fetch")
}
func dualRelease(source, tag string) *systemGitHubRelease {
	v := tag
	if source == "official" {
		v = strings.TrimPrefix(v, "v")
	}
	name := "codex2api_" + v + "_linux_amd64.tar.gz"
	return &systemGitHubRelease{TagName: tag, Assets: []systemGitHubAsset{{Name: name, Size: 100, Digest: "sha256:" + strings.Repeat("a", 64), BrowserDownloadURL: "https://github.com/" + systemUpdateSources[source] + "/releases/download/" + tag + "/" + name}}}
}
func dualUpdater() (*systemUpdater, *dualSourceFake) {
	f := &dualSourceFake{releases: map[string]*systemGitHubRelease{"patched": dualRelease("patched", "patched-v1.0.1"), "official": dualRelease("official", "v9.0.0")}, calls: map[string]int{}, fail: map[string]bool{}}
	return &systemUpdater{currentVersion: "patched-v1.0.0", currentSource: "patched", goos: "linux", goarch: "amd64", client: f}, f
}
func TestDualCanonicalTags(t *testing.T) {
	for _, tc := range []struct {
		source, tag string
		valid       bool
	}{{"patched", "patched-v1.0.0", true}, {"official", "v1.0.0", true}, {"patched", "v1.0.0", false}, {"official", "patched-v1.0.0", false}, {"patched", "patched-v01.0.0", false}, {"official", "v1.0", false}, {"official", "v1.0.0-rc1", false}, {"official", "v1.0.0+local", false}, {"official", " v1.0.0", false}} {
		if _, ok := canonicalSystemTag(tc.source, tc.tag); ok != tc.valid {
			t.Errorf("%s %s: %v", tc.source, tc.tag, ok)
		}
	}
}
func TestDualSourceCacheAndMigration(t *testing.T) {
	u, f := dualUpdater()
	ctx := context.Background()
	patched, err := u.CheckSource(ctx, "patched")
	if err != nil || patched.Status != "available" || !patched.HasUpdate {
		t.Fatalf("patched: %+v %v", patched, err)
	}
	official, err := u.CheckSource(ctx, "official")
	if err != nil || official.Status != "migration" || official.HasUpdate || !official.RequiresOfficialConfirmation || !official.RequiresMigrationConfirmation {
		t.Fatalf("official: %+v %v", official, err)
	}
	_, _ = u.CheckSource(ctx, "patched")
	if f.calls["patched"] != 1 || f.calls["official"] != 1 {
		t.Fatalf("caches not isolated: %v", f.calls)
	}
	u.currentVersion = "dev"
	info, err := u.CheckSource(ctx, "patched")
	if err != nil || !info.CurrentLocal || info.Status != "migration" || info.HasUpdate {
		t.Fatalf("local: %+v %v", info, err)
	}
}
func TestDualUnavailableNotLatest(t *testing.T) {
	u, f := dualUpdater()
	f.fail["official"] = true
	if _, err := u.CheckSource(context.Background(), "official"); err == nil {
		t.Fatal("expected failure")
	}
	info := u.unavailableSourceInfo("official")
	if info.Status != "unavailable" || info.LatestVersion != "" || info.PlanToken != "" || info.Supported {
		t.Fatalf("misleading fallback: %+v", info)
	}
	if _, err := u.CheckSource(context.Background(), "patched"); err != nil {
		t.Fatal(err)
	}
}
func TestDualAssetRepositoryBinding(t *testing.T) {
	asset := dualRelease("patched", "patched-v1.0.1").Assets[0]
	if err := validateSystemAssetURL(&asset, "patched", "patched-v1.0.1"); err != nil {
		t.Fatal(err)
	}
	original := asset.BrowserDownloadURL
	for _, bad := range []string{strings.Replace(original, "Egdon/codex2api-inject", "evil/repo", 1), strings.Replace(original, "github.com/", "github.com:443/", 1), strings.Replace(original, "https://", "https://user@", 1), original + "?x=1", strings.Replace(original, "patched-v1.0.1/", "patched-v1.0.2/", 1), strings.Replace(original, "github.com", "evil.githubusercontent.com", 1)} {
		asset.BrowserDownloadURL = bad
		if validateSystemAssetURL(&asset, "patched", "patched-v1.0.1") == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
func TestDualPlansBoundAndRejectChangedRelease(t *testing.T) {
	u, f := dualUpdater()
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		if _, err := u.CheckSource(ctx, "patched"); err != nil {
			t.Fatal(err)
		}
	}
	if len(u.plans) > 32 {
		t.Fatal("unbounded plans")
	}
	info, _ := u.CheckSource(ctx, "patched")
	f.releases["patched"].Assets[0].ID++
	_, err := u.PerformPlannedUpdate(ctx, systemUpdateRequest{Source: "patched", TargetTag: info.TargetTag, PlanToken: info.PlanToken})
	if !errors.Is(err, errSystemUpdatePlan) {
		t.Fatalf("changed release accepted: %v", err)
	}
	if _, ok := u.plans[info.PlanToken]; ok {
		t.Fatal("token not consumed")
	}
}
func TestDualPlanExpiryIdentityAndConfirmations(t *testing.T) {
	for _, kind := range []string{"expired", "identity", "source", "tag", "official-confirm", "migration-confirm"} {
		t.Run(kind, func(t *testing.T) {
			u, _ := dualUpdater()
			source := "patched"
			if strings.Contains(kind, "confirm") {
				source = "official"
			}
			info, err := u.CheckSource(context.Background(), source)
			if err != nil {
				t.Fatal(err)
			}
			request := systemUpdateRequest{Source: source, TargetTag: info.TargetTag, PlanToken: info.PlanToken, ConfirmOfficial: true, ConfirmMigration: true}
			switch kind {
			case "expired":
				p := u.plans[info.PlanToken]
				p.expires = time.Now().Add(-time.Second)
				u.plans[info.PlanToken] = p
			case "identity":
				u.currentVersion = "dev"
			case "source":
				request.Source = "official"
			case "tag":
				request.TargetTag = "patched-v1.0.2"
			case "official-confirm":
				request.ConfirmOfficial = false
			case "migration-confirm":
				request.ConfirmMigration = false
			}
			if _, err := u.PerformPlannedUpdate(context.Background(), request); !errors.Is(err, errSystemUpdatePlan) {
				t.Fatalf("unsafe request: %v", err)
			}
		})
	}
}

func TestDualPinnedTagSurvivesNewLatest(t *testing.T) {
	u, f := dualUpdater()
	ctx := context.Background()
	info, err := u.CheckSource(ctx, "patched")
	if err != nil {
		t.Fatal(err)
	}
	f.byTag = map[string]*systemGitHubRelease{"patched/" + info.TargetTag: cloneSystemGitHubRelease(f.releases["patched"])}
	f.releases["patched"] = dualRelease("patched", "patched-v2.0.0")
	release, err := u.fetchPlannedRelease(ctx, "patched", info.TargetTag)
	if err != nil || release.TagName != info.TargetTag || f.tagCalls != 1 {
		t.Fatalf("not pinned: %+v %v", release, err)
	}
}
func TestDualOfficialCurrentAndUnsupportedPlatform(t *testing.T) {
	u, _ := dualUpdater()
	u.currentSource = "official"
	u.currentVersion = "2.9.8"
	if u.baseSourceInfo("official").CurrentLocal {
		t.Fatal("official release misclassified as local")
	}
	if _, ok := canonicalSystemTag("official", "2.9.8"); ok {
		t.Fatal("noncanonical target accepted")
	}
	for _, platform := range [][2]string{{"windows", "amd64"}, {"freebsd", "amd64"}, {"linux", "386"}} {
		u.goos = platform[0]
		u.goarch = platform[1]
		if u.baseSourceInfo("official").Supported {
			t.Fatal("unsupported platform accepted")
		}
	}
}
func TestDualBinaryHeaders(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		for _, arch := range []string{"amd64", "arm64"} {
			t.Run(goos+"/"+arch, func(t *testing.T) {
				h := make([]byte, 64)
				if goos == "linux" {
					copy(h, "\x7fELF")
					h[4] = byte(elf.ELFCLASS64)
					h[5] = byte(elf.ELFDATA2LSB)
					h[6] = 1
					binary.LittleEndian.PutUint16(h[16:], uint16(elf.ET_EXEC))
					machine := elf.EM_X86_64
					if arch == "arm64" {
						machine = elf.EM_AARCH64
					}
					binary.LittleEndian.PutUint16(h[18:], uint16(machine))
				} else {
					binary.LittleEndian.PutUint32(h, macho.Magic64)
					cpu := macho.CpuAmd64
					if arch == "arm64" {
						cpu = macho.CpuArm64
					}
					binary.LittleEndian.PutUint32(h[4:], uint32(cpu))
					binary.LittleEndian.PutUint32(h[12:], uint32(macho.TypeExec))
				}
				path := filepath.Join(t.TempDir(), "binary")
				if err := os.WriteFile(path, h, 0600); err != nil {
					t.Fatal(err)
				}
				if err := validateSystemBinaryPlatform(path, goos, arch); err != nil {
					t.Fatal(err)
				}
				other := "amd64"
				if arch == "amd64" {
					other = "arm64"
				}
				if validateSystemBinaryPlatform(path, goos, other) == nil {
					t.Fatal("wrong architecture accepted")
				}
				h[0] = 0
				if err := os.WriteFile(path, h, 0600); err != nil {
					t.Fatal(err)
				}
				if validateSystemBinaryPlatform(path, goos, arch) == nil {
					t.Fatal("invalid magic accepted")
				}
			})
		}
	}
}
