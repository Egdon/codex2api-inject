package proxy

import (
	"archive/zip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

const (
	codexMSIXChunkBytes  = 1 << 20
	codexMSIXBudgetBytes = 36 << 20
	codexASARHeaderMax   = 8 << 20
	codexASAROutputMax   = 64 << 20
	codexPackageJSONMax  = 64 << 10
)

type codexRangeReader struct {
	ctx        context.Context
	client     *http.Client
	url        string
	size       int64
	etag       string
	used       int64
	cacheStart int64
	cache      []byte
}

func codexRangeMetadata(resp *http.Response) (start, end, total int64, err error) {
	if resp.StatusCode != http.StatusPartialContent {
		return 0, 0, 0, fmt.Errorf("Range request returned HTTP %d", resp.StatusCode)
	}
	n, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total)
	if err != nil || n != 3 || start < 0 || end < start || total <= end {
		return 0, 0, 0, fmt.Errorf("invalid Content-Range %q", resp.Header.Get("Content-Range"))
	}
	return start, end, total, nil
}

func newCodexRangeReader(ctx context.Context, client *http.Client, url string) (*codexRangeReader, error) {
	r := &codexRangeReader{ctx: ctx, client: client, url: url}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	start, end, size, err := codexRangeMetadata(resp)
	if err != nil {
		return nil, fmt.Errorf("MSIX range probe: %w", err)
	}
	if start != 0 || end != 0 {
		return nil, fmt.Errorf("MSIX range probe returned unexpected interval")
	}
	r.size, r.etag = size, resp.Header.Get("ETag")
	return r, nil
}

func (r *codexRangeReader) fetch(start int64) error {
	end := start + codexMSIXChunkBytes - 1
	if end >= r.size {
		end = r.size - 1
	}
	count := end - start + 1
	if r.used+count > codexMSIXBudgetBytes {
		return fmt.Errorf("MSIX Range budget exceeded")
	}
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	req.Header.Set("Accept-Encoding", "identity")
	if r.etag != "" {
		req.Header.Set("If-Range", r.etag)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	gotStart, gotEnd, total, err := codexRangeMetadata(resp)
	if err != nil {
		return fmt.Errorf("MSIX Range response mismatch: %w", err)
	}
	if gotStart != start || gotEnd != end || total != r.size {
		return fmt.Errorf("MSIX Range response interval changed")
	}
	if r.etag != "" && resp.Header.Get("ETag") != "" && resp.Header.Get("ETag") != r.etag {
		return fmt.Errorf("MSIX archive changed during Range read")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, count+1))
	if err != nil {
		return fmt.Errorf("MSIX Range body read: %w", err)
	}
	if int64(len(data)) != count {
		return fmt.Errorf("MSIX Range body length mismatch")
	}
	r.cacheStart, r.cache = start, data
	r.used += count
	return nil
}

func (r *codexRangeReader) ReadAt(p []byte, offset int64) (int, error) {
	if offset < 0 || offset >= r.size {
		return 0, io.EOF
	}
	n := 0
	for len(p) > 0 && offset < r.size {
		if offset < r.cacheStart || offset >= r.cacheStart+int64(len(r.cache)) {
			if err := r.fetch(offset / codexMSIXChunkBytes * codexMSIXChunkBytes); err != nil {
				return n, err
			}
		}
		copied := copy(p, r.cache[offset-r.cacheStart:])
		p, offset, n = p[copied:], offset+int64(copied), n+copied
	}
	if len(p) > 0 {
		return n, io.EOF
	}
	return n, nil
}

func codexASARPackageLocation(source io.Reader) (int64, int64, int64, error) {
	var prefix [16]byte
	if _, err := io.ReadFull(source, prefix[:]); err != nil {
		return 0, 0, 0, err
	}
	headerSize := int64(binary.LittleEndian.Uint32(prefix[4:8]))
	jsonSize := int64(binary.LittleEndian.Uint32(prefix[12:16]))
	if headerSize < 8 || headerSize > codexASARHeaderMax || jsonSize <= 0 || jsonSize > headerSize-8 {
		return 0, 0, 0, fmt.Errorf("invalid ASAR header size")
	}
	header := make([]byte, jsonSize)
	if _, err := io.ReadFull(source, header); err != nil {
		return 0, 0, 0, err
	}
	var metadata struct {
		Files map[string]struct {
			Offset string `json:"offset"`
			Size   int64  `json:"size"`
		} `json:"files"`
	}
	if err := json.Unmarshal(header, &metadata); err != nil {
		return 0, 0, 0, err
	}
	entry, ok := metadata.Files["package.json"]
	if !ok || entry.Size <= 0 || entry.Size > codexPackageJSONMax {
		return 0, 0, 0, fmt.Errorf("ASAR root package.json missing or oversized")
	}
	offset, err := strconv.ParseInt(entry.Offset, 10, 64)
	if err != nil || offset < 0 {
		return 0, 0, 0, fmt.Errorf("invalid ASAR package.json offset")
	}
	target := 8 + headerSize + offset
	consumed := int64(len(prefix)) + jsonSize
	if target < consumed || target+entry.Size > codexASAROutputMax {
		return 0, 0, 0, fmt.Errorf("ASAR package.json exceeds prefix limit")
	}
	return target, entry.Size, consumed, nil
}

func codexASARPackageVersion(source io.Reader) (string, error) {
	target, size, consumed, err := codexASARPackageLocation(source)
	if err != nil {
		return "", err
	}
	if _, err := io.CopyN(io.Discard, source, target-consumed); err != nil {
		return "", err
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(source, data); err != nil {
		return "", err
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", err
	}
	if _, ok := codexBuildParts(pkg.Version, 3); !ok {
		return "", fmt.Errorf("invalid Desktop build in package.json")
	}
	return pkg.Version, nil
}

func codexWindowsPackageVersion(ctx context.Context, client *http.Client) (string, []int64, error) {
	resp, err := codexBuildRequest(ctx, client, codexBuildRequestSpec{Method: http.MethodGet, URL: codexWindowsUpdateURL})
	if err != nil {
		return "", nil, err
	}
	data, err := codexReadSmallResponse(resp, 64<<10)
	if err != nil {
		return "", nil, err
	}
	var update struct {
		PackageIdentity string `json:"packageIdentity"`
		BuildVersion    string `json:"buildVersion"`
	}
	if err := json.Unmarshal(data, &update); err != nil {
		return "", nil, err
	}
	parts, valid := codexBuildParts(update.BuildVersion, 4)
	if !valid || update.PackageIdentity != "OpenAI.Codex" {
		return "", nil, fmt.Errorf("invalid Windows update manifest")
	}
	return update.BuildVersion, parts, nil
}

// FetchCodexDesktopWindowsBuild 以 Range 分片读取 MSIX 内 ASAR 的压缩前缀。
func FetchCodexDesktopWindowsBuild(ctx context.Context, proxyURL string) (string, error) {
	client := codexBuildHTTPClient(proxyURL)
	packageVersion, parts, err := codexWindowsPackageVersion(ctx, client)
	if err != nil {
		return "", err
	}
	url := "https://persistent.oaistatic.com/codex-app-prod/releases/" + packageVersion + "/ChatGPT-x64.msix"
	ranges, err := newCodexRangeReader(ctx, client, url)
	if err != nil {
		return "", err
	}
	archive, err := zip.NewReader(ranges, ranges.size)
	if err != nil {
		return "", err
	}
	for _, file := range archive.File {
		if file.Name != "app/resources/app.asar" {
			continue
		}
		if file.Method != zip.Deflate {
			return "", fmt.Errorf("unsupported ASAR ZIP compression %d", file.Method)
		}
		stream, err := file.Open()
		if err != nil {
			return "", err
		}
		version, parseErr := codexASARPackageVersion(stream)
		_ = stream.Close()
		if parseErr != nil {
			return "", parseErr
		}
		buildParts, ok := codexBuildParts(version, 3)
		if !ok || buildParts[0] != parts[0] || buildParts[1] != parts[1] {
			return "", fmt.Errorf("Desktop package version mismatch")
		}
		return version, nil
	}
	return "", fmt.Errorf("Desktop app.asar not found in MSIX")
}
