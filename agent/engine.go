package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// defaultLLaMATag is the pinned llama.cpp nightly the agent installs when
// discovery is unavailable. Updated by hand; override with SPARK_LLAMA_TAG.
const defaultLLaMATag = "b11878"

const githubReleasesAPI = "https://api.github.com/repos/ggml-org/llama.cpp/releases?per_page=15"

// EngineManager installs and locates llama.cpp binaries under
// <dataDir>/engines/<tag>/, or uses SPARK_LLAMA_PATH as-is.
type EngineManager struct {
	dataDir string
	cfg     *Config
}

func NewEngineManager(cfg *Config) *EngineManager {
	return &EngineManager{dataDir: cfg.DataDir, cfg: cfg}
}

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// locateBin resolves a binary that llama.cpp has renamed across releases
// (rpc-server became ggml-rpc-server). candidates are bare names.
func locateBin(dir string, candidates ...string) string {
	for _, c := range candidates {
		p := filepath.Join(dir, exeName(c))
		if fileExists(p) {
			return p
		}
	}
	return ""
}

// assetPatternFor maps (goos, goarch) to the llama.cpp release asset suffix.
// Verified against nightly asset naming, e.g. llama-b11878-bin-win-cpu-x64.zip.
func assetPatternFor(goos, goarch string) (string, error) {
	switch {
	case goos == "windows" && goarch == "amd64":
		return "bin-win-cpu-x64.zip", nil
	case goos == "windows" && goarch == "arm64":
		return "bin-win-cpu-arm64.zip", nil
	case goos == "linux" && goarch == "amd64":
		return "bin-ubuntu-x64.tar.gz", nil
	case goos == "linux" && goarch == "arm64":
		return "bin-ubuntu-arm64.tar.gz", nil
	case goos == "darwin" && goarch == "arm64":
		return "bin-macos-arm64.tar.gz", nil
	case goos == "darwin" && goarch == "amd64":
		return "bin-macos-x64.tar.gz", nil
	case goos == "android" && goarch == "arm64":
		return "bin-android-arm64.tar.gz", nil
	default:
		return "", fmt.Errorf("no prebuilt llama.cpp asset for %s/%s; set SPARK_LLAMA_PATH to local binaries", goos, goarch)
	}
}

// findAsset picks the asset URL whose name contains the platform pattern.
func findAsset(pattern string, assets []ghAsset) (ghAsset, bool) {
	for _, a := range assets {
		name := strings.ToLower(a.Name)
		if strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".tar.gz") {
			if strings.Contains(name, strings.ToLower(pattern)) {
				return a, true
			}
		}
	}
	return ghAsset{}, false
}

type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// resolveTag picks the llama.cpp release tag: explicit override, pinned
// default, or the newest nightly that actually ships our platform asset.
func (e *EngineManager) resolveTag(ctx context.Context) string {
	if e.cfg.LLaMATag != "" {
		return e.cfg.LLaMATag
	}
	pattern, err := assetPatternFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return defaultLLaMATag
	}
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubReleasesAPI, nil)
	if err != nil {
		return defaultLLaMATag
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return defaultLLaMATag
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return defaultLLaMATag
	}
	var releases []ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return defaultLLaMATag
	}
	for _, r := range releases {
		if !r.Prerelease {
			continue
		}
		if _, ok := findAsset(pattern, r.Assets); ok {
			return r.TagName
		}
	}
	return defaultLLaMATag
}

// installedAt returns engine paths if tag is fully installed in dataDir.
func installedAt(dataDir, tag string) (EngineInfo, bool) {
	dir := filepath.Join(dataDir, "engines", tag)
	server := locateBin(dir, "llama-server")
	rpc := locateBin(dir, "rpc-server", "ggml-rpc-server")
	if server != "" && rpc != "" && fileExists(filepath.Join(dir, "spark-ok")) {
		return EngineInfo{Tag: tag, Dir: dir, ServerPath: server, RPCPath: rpc}, true
	}
	return EngineInfo{}, false
}

// cachedTag persists the last tag resolved from GitHub so steady-state
// Ensures never touch the network (also survives API rate limits).
func (e *EngineManager) cachedTag() string {
	b, err := os.ReadFile(filepath.Join(e.dataDir, "engines", "resolved-tag"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (e *EngineManager) writeCachedTag(tag string) {
	_ = os.MkdirAll(filepath.Join(e.dataDir, "engines"), 0o755)
	_ = os.WriteFile(filepath.Join(e.dataDir, "engines", "resolved-tag"), []byte(tag), 0o644)
}

// Ensure returns an EngineInfo with absolute binary paths, downloading and
// extracting the llama.cpp release on first use. Local installs are checked
// FIRST (pinned tag, cached tag, default tag); the GitHub API is only hit
// when nothing is installed locally. Safe to call repeatedly.
func (e *EngineManager) Ensure(ctx context.Context) (EngineInfo, error) {
	if e.cfg.LLaMAPath != "" {
		dir := e.cfg.LLaMAPath
		server := locateBin(dir, "llama-server")
		rpc := locateBin(dir, "rpc-server", "ggml-rpc-server")
		if server != "" && rpc != "" {
			return EngineInfo{Tag: "custom", Dir: dir, ServerPath: server, RPCPath: rpc}, nil
		}
		return EngineInfo{}, fmt.Errorf("SPARK_LLAMA_PATH=%q is missing llama-server/rpc-server binaries", dir)
	}

	// fast path: something already installed locally
	for _, tag := range e.localTags() {
		if info, ok := installedAt(e.dataDir, tag); ok {
			return info, nil
		}
	}

	tag := e.cfg.LLaMATag
	if tag == "" {
		tag = e.resolveTag(ctx)
	}
	e.writeCachedTag(tag)

	if info, ok := installedAt(e.dataDir, tag); ok {
		return info, nil
	}

	pattern, err := assetPatternFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return EngineInfo{}, err
	}
	asset, err := e.downloadAsset(ctx, tag, pattern)
	if err != nil {
		return EngineInfo{}, err
	}
	dir := filepath.Join(e.dataDir, "engines", tag)
	if err := extractArchive(asset, dir); err != nil {
		return EngineInfo{}, fmt.Errorf("extract %s: %w", filepath.Base(asset), err)
	}
	_ = os.Remove(asset)
	if info, ok := installedAt(e.dataDir, tag); ok {
		_ = os.WriteFile(filepath.Join(dir, "spark-ok"), []byte(pattern), 0o644)
		return info, nil
	}
	return EngineInfo{}, fmt.Errorf("archive did not contain llama-server/rpc-server (looked in %s)", dir)
}

// localTags lists candidate tags to probe on disk, in priority order.
func (e *EngineManager) localTags() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range []string{e.cfg.LLaMATag, e.cachedTag(), defaultLLaMATag} {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

func (e *EngineManager) downloadAsset(ctx context.Context, tag, pattern string) (string, error) {
	url := fmt.Sprintf("https://github.com/ggml-org/llama.cpp/releases/download/%s/llama-%s-%s", tag, tag, pattern)
	dest := filepath.Join(e.dataDir, "engines", fmt.Sprintf("download-%s-%s", tag, pattern))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if err := downloadTo(ctx, url, dest, nil); err != nil {
		return "", err
	}
	return dest, nil
}

// Current reports the engine state without side effects (for /status).
func (e *EngineManager) Current() EngineInfo {
	if e.cfg.LLaMAPath != "" {
		dir := e.cfg.LLaMAPath
		server := locateBin(dir, "llama-server")
		rpc := locateBin(dir, "rpc-server", "ggml-rpc-server")
		if server != "" && rpc != "" {
			return EngineInfo{Tag: "custom", Dir: dir, ServerPath: server, RPCPath: rpc}
		}
		return EngineInfo{}
	}
	tag := e.cfg.LLaMATag
	if tag == "" {
		tag = defaultLLaMATag
	}
	dir := filepath.Join(e.dataDir, "engines", tag)
	server := locateBin(dir, "llama-server")
	rpc := locateBin(dir, "rpc-server", "ggml-rpc-server")
	if server != "" && rpc != "" {
		return EngineInfo{Tag: tag, Dir: dir, ServerPath: server, RPCPath: rpc}
	}
	return EngineInfo{}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// extractArchive unpacks .zip or .tar.gz llama.cpp release archives into dir.
// Archives are flat (binaries at the root); every regular member is extracted.
func extractArchive(archivePath, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lower := strings.ToLower(archivePath)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(archivePath, dir)
	case strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
		return extractTarGz(archivePath, dir)
	default:
		return fmt.Errorf("unsupported archive type: %s", filepath.Base(archivePath))
	}
}

func safeJoin(dir, name string) (string, error) {
	target := filepath.Clean(filepath.Join(dir, filepath.FromSlash(name)))
	if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive member escapes target dir: %s", name)
	}
	return target, nil
}

func extractZip(archivePath, dir string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		target, err := safeJoin(dir, f.Name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeMode(rc, target, f.Mode())
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func extractTarGz(archivePath, dir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		target, err := safeJoin(dir, hdr.Name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeMode(tr, target, hdr.FileInfo().Mode()); err != nil {
			return err
		}
	}
}

func writeMode(r io.Reader, target string, mode os.FileMode) error {
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, r)
	return err
}
