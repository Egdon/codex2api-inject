package proxy

import (
	"archive/zip"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
)

type codexWindowsUpdate struct {
	PackageIdentity string `json:"packageIdentity"`
	BuildVersion    string `json:"buildVersion"`
}

type codexWindowsMappingArch struct {
	Architecture           string `json:"architecture"`
	Version                string `json:"version"`
	AppVersion             string `json:"appVersion"`
	BackendVersion         string `json:"backendVersion"`
	Status                 string `json:"status"`
	CurrentForCodexVersion bool   `json:"currentForCodexVersion"`
	ETag                   string `json:"etag"`
}

type codexWindowsMapping struct {
	SchemaVersion int `json:"schemaVersion"`
	Sources       struct {
		Windows struct {
			UpdateManifest codexWindowsUpdate                 `json:"updateManifest"`
			Architectures  map[string]codexWindowsMappingArch `json:"architectures"`
		} `json:"windows"`
	} `json:"sources"`
}

func fetchCodexWindowsCandidates(ctx context.Context, client *http.Client) []codexClientCandidate {
	var update codexWindowsUpdate
	data, updateErr := fetchCodexSmallJSON(ctx, client, codexClientSources.WindowsUpdate)
	if updateErr == nil {
		updateErr = json.Unmarshal(data, &update)
	}
	if _, ok := codexBuildParts(update.BuildVersion, 4); updateErr == nil && (!ok || update.PackageIdentity != "OpenAI.Codex") {
		updateErr = fmt.Errorf("invalid official Windows update manifest")
	}
	var mapping codexWindowsMapping
	var mappingErr error
	if updateErr == nil {
		data, mappingErr = fetchCodexSmallJSON(ctx, client, codexClientSources.WindowsMapping)
		if mappingErr == nil {
			mappingErr = json.Unmarshal(data, &mapping)
		}
	}
	result := make([]codexClientCandidate, 0, 2)
	for _, arch := range []string{"x64", "arm64"} {
		candidate := codexWindowsCandidate(update, arch)
		candidate.Err = updateErr
		if updateErr == nil {
			candidate = applyCodexWindowsMapping(candidate, mapping, mappingErr)
		}
		result = append(result, candidate)
	}
	return result
}

func codexWindowsCandidate(update codexWindowsUpdate, arch string) codexClientCandidate {
	url := "https://persistent.oaistatic.com/codex-app-prod/releases/" + update.BuildVersion + "/ChatGPT-" + arch + ".msix"
	return codexClientCandidate{Kind: string(CodexClientKindDesktop), Target: "win32-" + arch,
		Pair: CodexClientVersionPair{PackageVersion: update.BuildVersion, Source: "official_msix", ArtifactURL: url, ArtifactID: codexArtifactIdentity(url, update.BuildVersion)}}
}

func applyCodexWindowsMapping(candidate codexClientCandidate, mapping codexWindowsMapping, fetchErr error) codexClientCandidate {
	if fetchErr != nil {
		candidate.FallbackReason = fetchErr.Error()
		return candidate
	}
	arch := strings.TrimPrefix(candidate.Target, "win32-")
	entry, ok := mapping.Sources.Windows.Architectures[arch]
	if err := validateCodexWindowsMapping(mapping, entry, candidate); !ok || err != nil {
		candidate.FallbackReason = "missing, stale or invalid architecture mapping"
		return candidate
	}
	candidate.Pair.AppVersion, candidate.Pair.CLIVersion = entry.AppVersion, entry.BackendVersion
	candidate.Pair.Source = "third_party_windows_mapping"
	candidate.Pair.ArtifactID = codexArtifactIdentity(candidate.Target, entry.Version, entry.AppVersion, entry.BackendVersion, entry.ETag)
	return candidate
}

func validateCodexWindowsMapping(mapping codexWindowsMapping, entry codexWindowsMappingArch, candidate codexClientCandidate) error {
	update := mapping.Sources.Windows.UpdateManifest
	if mapping.SchemaVersion <= 0 || update.PackageIdentity != "OpenAI.Codex" || update.BuildVersion != candidate.Pair.PackageVersion {
		return fmt.Errorf("stale Windows mapping")
	}
	if entry.Architecture != strings.TrimPrefix(candidate.Target, "win32-") || entry.Version != update.BuildVersion || entry.Status != "downloadable" || !entry.CurrentForCodexVersion {
		return fmt.Errorf("invalid Windows architecture mapping")
	}
	app, validApp := codexBuildParts(entry.AppVersion, 3)
	pkg, validPackage := codexBuildParts(entry.Version, 4)
	if !validApp || !validPackage || app[0] != pkg[0] || app[1] != pkg[1] || !validCodexClientVersionString(entry.BackendVersion) {
		return fmt.Errorf("invalid Windows application/backend versions")
	}
	return nil
}

func validateCodexMSIXIdentity(archive *zip.Reader, candidate codexClientCandidate) error {
	data, err := codexArchiveFile(archive, "AppxManifest.xml")
	if err != nil {
		return err
	}
	var manifest struct {
		Identity struct {
			Name    string `xml:"Name,attr"`
			Version string `xml:"Version,attr"`
			Arch    string `xml:"ProcessorArchitecture,attr"`
		} `xml:"Identity"`
	}
	if err := xml.Unmarshal(data, &manifest); err != nil {
		return err
	}
	identity := manifest.Identity
	if identity.Name != "OpenAI.Codex" || identity.Version != candidate.Pair.PackageVersion || identity.Arch != strings.TrimPrefix(candidate.Target, "win32-") {
		return fmt.Errorf("MSIX package identity/version/architecture mismatch")
	}
	return nil
}

func codexMSIXAppVersion(archive *zip.Reader) (string, error) {
	for _, file := range archive.File {
		if file.Name != "app/resources/app.asar" {
			continue
		}
		stream, err := file.Open()
		if err != nil {
			return "", err
		}
		defer stream.Close()
		return codexASARPackageVersion(stream)
	}
	return "", fmt.Errorf("Desktop app.asar missing in MSIX")
}

func resolveCodexMSIXPair(archive *zip.Reader, candidate codexClientCandidate) (CodexClientVersionPair, error) {
	pair := candidate.Pair
	if err := validateCodexMSIXIdentity(archive, candidate); err != nil {
		return pair, err
	}
	cli, err := codexArchiveCLI(archive, candidate.Target, "app/")
	if err != nil {
		return pair, err
	}
	app, err := codexMSIXAppVersion(archive)
	if err != nil {
		return pair, err
	}
	appParts, validApp := codexBuildParts(app, 3)
	pkgParts, validPackage := codexBuildParts(pair.PackageVersion, 4)
	if !validApp || !validPackage || appParts[0] != pkgParts[0] || appParts[1] != pkgParts[1] {
		return pair, fmt.Errorf("MSIX application version mismatch")
	}
	pair.AppVersion, pair.CLIVersion = app, cli
	return pair, nil
}
