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

// Ensure returns an EngineInfo with absolute binary paths, downloading and
// extracting the llama.cpp release on first use. Safe to call repeatedly.
func (e *EngineManager) Ensure(ctx context.Context) (EngineInfo, error) {
	if e.cfg.LLaMAPath != "" {
		dir := e.cfg.LLaMAPath
		server := filepath.Join(dir, exeName("llama-server"))
		rpc := filepath.Join(dir, exeName("rpc-server"))
		if fileExists(server) && fileExists(rpc) {
			return EngineInfo{Tag: "custom", Dir: dir, ServerPath: server, RPCPath: rpc}, nil
		}
		return EngineInfo{}, fmt.Errorf("SPARK_LLAMA_PATH=%q is missing llama-server/rpc-server binaries", dir)
	}

	tag := e.resolveTag(ctx)
	dir := filepath.Join(e.dataDir, "engines", tag)
	server := filepath.Join(dir, exeName("llama-server"))
	rpc := filepath.Join(dir, exeName("rpc-server"))
	marker := filepath.Join(dir, "spark-ok")
	if fileExists(server) && fileExists(rpc) && fileExists(marker) {
		return EngineInfo{Tag: tag, Dir: dir, ServerPath: server, RPCPath: rpc}, nil
	}

	pattern, err := assetPatternFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return EngineInfo{}, err
	}
	asset, err := e.downloadAsset(ctx, tag, pattern)
	if err != nil {
		return EngineInfo{}, err
	}
	if err := extractArchive(asset, dir); err != nil {
		return EngineInfo{}, fmt.Errorf("extract %s: %w", filepath.Base(asset), err)
	}
	_ = os.Remove(asset)
	if !fileExists(server) || !fileExists(rpc) {
		return EngineInfo{}, fmt.Errorf("archive did not contain llama-server/rpc-server (looked in %s)", dir)
	}
	_ = os.WriteFile(marker, []byte(pattern), 0o644)
	return EngineInfo{Tag: tag, Dir: dir, ServerPath: server, RPCPath: rpc}, nil
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
		server := filepath.Join(dir, exeName("llama-server"))
		rpc := filepath.Join(dir, exeName("rpc-server"))
		if fileExists(server) && fileExists(rpc) {
			return EngineInfo{Tag: "custom", Dir: dir, ServerPath: server, RPCPath: rpc}
		}
		return EngineInfo{}
	}
	tag := e.cfg.LLaMATag
	if tag == "" {
		tag = defaultLLaMATag
	}
	dir := filepath.Join(e.dataDir, "engines", tag)
	server := filepath.Join(dir, exeName("llama-server"))
	rpc := filepath.Join(dir, exeName("rpc-server"))
	if fileExists(server) && fileExists(rpc) {
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
