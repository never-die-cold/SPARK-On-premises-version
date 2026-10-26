package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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

// assetPlan describes the archives needed for a platform+flavor combo.
// Windows CUDA needs a second archive with the CUDA runtime DLLs.
type assetPlan struct {
	patterns []string // first match wins across these patterns
	extra    []string // additional archives merged into the same dir
}

// assetPlanFor maps (goos, goarch, flavor) to llama.cpp release assets.
// Verified against nightly naming, e.g.
//   llama-b11878-bin-win-cpu-x64.zip
//   llama-b11878-bin-win-cuda-13.4-x64.zip + cudart-llama-bin-win-cuda-13.4-x64.zip
//   llama-b11878-bin-ubuntu-vulkan-x64.tar.gz
func assetPlanFor(goos, goarch, flavor string) (assetPlan, error) {
	cpuPatterns := func() ([]string, error) {
		switch {
		case goos == "windows" && goarch == "amd64":
			return []string{"bin-win-cpu-x64.zip"}, nil
		case goos == "windows" && goarch == "arm64":
			return []string{"bin-win-cpu-arm64.zip"}, nil
		case goos == "linux" && goarch == "amd64":
			return []string{"bin-ubuntu-x64.tar.gz"}, nil
		case goos == "linux" && goarch == "arm64":
			return []string{"bin-ubuntu-arm64.tar.gz"}, nil
		case goos == "darwin" && goarch == "arm64":
			return []string{"bin-macos-arm64.tar.gz"}, nil
		case goos == "darwin" && goarch == "amd64":
			return []string{"bin-macos-x64.tar.gz"}, nil
		case goos == "android" && goarch == "arm64":
			return []string{"bin-android-arm64.tar.gz"}, nil
		default:
			return nil, fmt.Errorf("no prebuilt llama.cpp asset for %s/%s; set SPARK_LLAMA_PATH to local binaries", goos, goarch)
		}
	}

	switch flavor {
	case "", "cpu":
		p, err := cpuPatterns()
		return assetPlan{patterns: p}, err
	case "cuda":
		switch {
		case goos == "windows" && goarch == "amd64":
			return assetPlan{
				patterns: []string{"bin-win-cuda-13.4-x64.zip", "bin-win-cuda-12.4-x64.zip"},
				extra:    []string{"cudart-llama-bin-win-cuda-13.4-x64.zip", "cudart-llama-bin-win-cuda-12.4-x64.zip"},
			}, nil
		case goos == "linux" && goarch == "amd64":
			return assetPlan{patterns: []string{"bin-ubuntu-cuda-13.4-x64.tar.gz", "bin-ubuntu-cuda-12.8-x64.tar.gz"}}, nil
		default:
			return assetPlan{}, fmt.Errorf("no CUDA build for %s/%s; use cpu or vulkan", goos, goarch)
		}
	case "vulkan":
		switch {
		case goos == "windows" && goarch == "amd64":
			return assetPlan{patterns: []string{"bin-win-vulkan-x64.zip"}}, nil
		case goos == "linux" && goarch == "amd64":
			return assetPlan{patterns: []string{"bin-ubuntu-vulkan-x64.tar.gz"}}, nil
		case goos == "linux" && goarch == "arm64":
			return assetPlan{patterns: []string{"bin-ubuntu-vulkan-arm64.tar.gz"}}, nil
		default:
			return assetPlan{}, fmt.Errorf("no Vulkan build for %s/%s; use cpu", goos, goarch)
		}
	default:
		return assetPlan{}, fmt.Errorf("unknown engine flavor %q (cpu|cuda|vulkan)", flavor)
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
	flavor := e.cfg.LLaMAFlavor
	if flavor == "" {
		flavor = "cpu"
	}
	plan, err := assetPlanFor(runtime.GOOS, runtime.GOARCH, flavor)
	if err != nil || len(plan.patterns) == 0 {
		return defaultLLaMATag
	}
	pattern := plan.patterns[0]
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

// installedAt returns engine paths if tag+flavor is fully installed.
// Marker format: "<flavor>|<asset-pattern>"; pre-flavor markers count as cpu.
func installedAt(dataDir, tag, flavor string) (EngineInfo, bool) {
	dir := filepath.Join(dataDir, "engines", tag)
	server := locateBin(dir, "llama-server")
	rpc := locateBin(dir, "rpc-server", "ggml-rpc-server")
	marker := filepath.Join(dir, "spark-ok")
	if server == "" || rpc == "" || !fileExists(marker) {
		return EngineInfo{}, false
	}
	raw, _ := os.ReadFile(marker)
	installedFlavor := "cpu"
	if parts := strings.SplitN(strings.TrimSpace(string(raw)), "|", 2); len(parts) == 2 {
		installedFlavor = parts[0]
	}
	if installedFlavor != flavor {
		return EngineInfo{}, false
	}
	return EngineInfo{Tag: tag, Dir: dir, ServerPath: server, RPCPath: rpc, Flavor: flavor}, true
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
// extracting the llama.cpp release (cpu/cuda/vulkan flavor) on first use.
// Local installs are checked FIRST; the GitHub API is only hit when nothing
// matching is installed. Safe to call repeatedly.
func (e *EngineManager) Ensure(ctx context.Context) (EngineInfo, error) {
	flavor := e.cfg.LLaMAFlavor
	if flavor == "" {
		flavor = "cpu"
	}
	if e.cfg.LLaMAPath != "" {
		dir := e.cfg.LLaMAPath
		server := locateBin(dir, "llama-server")
		rpc := locateBin(dir, "rpc-server", "ggml-rpc-server")
		if server != "" && rpc != "" {
			return EngineInfo{Tag: "custom", Dir: dir, ServerPath: server, RPCPath: rpc, Flavor: flavor}, nil
		}
		return EngineInfo{}, fmt.Errorf("SPARK_LLAMA_PATH=%q is missing llama-server/rpc-server binaries", dir)
	}

	// fast path: something matching the flavor already installed locally
	for _, tag := range e.localTags() {
		if info, ok := installedAt(e.dataDir, tag, flavor); ok {
			return info, nil
		}
	}

	tag := e.cfg.LLaMATag
	if tag == "" {
		tag = e.resolveTag(ctx)
	}
	e.writeCachedTag(tag)

	if info, ok := installedAt(e.dataDir, tag, flavor); ok {
		return info, nil
	}

	plan, err := assetPlanFor(runtime.GOOS, runtime.GOARCH, flavor)
	if err != nil {
		return EngineInfo{}, err
	}
	dir := filepath.Join(e.dataDir, "engines", tag)

	chosen, pattern, err := e.downloadFirst(ctx, tag, plan.patterns)
	if err != nil {
		return EngineInfo{}, err
	}
	if err := extractArchive(chosen, dir); err != nil {
		return EngineInfo{}, fmt.Errorf("extract %s: %w", filepath.Base(chosen), err)
	}
	_ = os.Remove(chosen)

	// runtime packs (e.g. cudart DLLs) merge into the same directory
	for _, extraPattern := range plan.extra {
		extraAsset, err := e.downloadAsset(ctx, tag, extraPattern)
		if err != nil {
			log.Printf("engine: optional runtime pack %s unavailable: %v", extraPattern, err)
			continue
		}
		if err := extractArchive(extraAsset, dir); err != nil {
			log.Printf("engine: extracting runtime pack %s failed: %v", extraPattern, err)
		}
		_ = os.Remove(extraAsset)
	}

	server := locateBin(dir, "llama-server")
	rpc := locateBin(dir, "rpc-server", "ggml-rpc-server")
	if server == "" || rpc == "" {
		return EngineInfo{}, fmt.Errorf("archive did not contain llama-server/rpc-server (looked in %s)", dir)
	}
	_ = os.WriteFile(filepath.Join(dir, "spark-ok"), []byte(flavor+"|"+pattern), 0o644)
	return EngineInfo{Tag: tag, Dir: dir, ServerPath: server, RPCPath: rpc, Flavor: flavor}, nil
}

// downloadFirst tries each pattern's release asset until one exists,
// returning the local path and the pattern that matched.
func (e *EngineManager) downloadFirst(ctx context.Context, tag string, patterns []string) (string, string, error) {
	var lastErr error
	for _, p := range patterns {
		asset, err := e.downloadAsset(ctx, tag, p)
		if err == nil {
			return asset, p, nil
		}
		lastErr = err
		log.Printf("engine: asset %s unavailable (%v); trying next", p, err)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no asset patterns")
	}
	return "", "", lastErr
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

// assetFileName maps a pattern to the full release asset file name. Main
// platform builds are named llama-<tag>-<pattern>; runtime packs such as
// cudart-llama-bin-win-cuda-13.4-x64.zip have their own full name.
func assetFileName(tag, pattern string) string {
	if strings.HasPrefix(pattern, "llama-") || strings.HasPrefix(pattern, "cudart-") {
		return pattern
	}
	return fmt.Sprintf("llama-%s-%s", tag, pattern)
}

func (e *EngineManager) downloadAsset(ctx context.Context, tag, pattern string) (string, error) {
	name := assetFileName(tag, pattern)
	url := fmt.Sprintf("https://github.com/ggml-org/llama.cpp/releases/download/%s/%s", tag, name)
	dest := filepath.Join(e.dataDir, "engines", fmt.Sprintf("download-%s", name))
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
	flavor := e.cfg.LLaMAFlavor
	if flavor == "" {
		flavor = "cpu"
	}
	if e.cfg.LLaMAPath != "" {
		dir := e.cfg.LLaMAPath
		server := locateBin(dir, "llama-server")
		rpc := locateBin(dir, "rpc-server", "ggml-rpc-server")
		if server != "" && rpc != "" {
			return EngineInfo{Tag: "custom", Dir: dir, ServerPath: server, RPCPath: rpc, Flavor: "custom"}
		}
		return EngineInfo{}
	}
	tag := e.cfg.LLaMATag
	if tag == "" {
		tag = e.cachedTag()
	}
	if tag == "" {
		tag = defaultLLaMATag
	}
	if info, ok := installedAt(e.dataDir, tag, flavor); ok {
		return info
	}
	return EngineInfo{}
}

// resolveEngineFlavor turns "auto" into a concrete flavor using hardware
// probes. CUDA builds cover NVIDIA on windows/linux amd64; everything else
// (including Apple/ARM/Android) uses cpu. Vulkan is opt-in via SPARK_LLAMA_FLAVOR.
func resolveEngineFlavor(requested string) string {
	switch requested {
	case "cpu", "cuda", "vulkan":
		return requested
	}
	if (runtime.GOOS == "windows" || runtime.GOOS == "linux") &&
		runtime.GOARCH == "amd64" && len(detectGPUs()) > 0 {
		return "cuda"
	}
	return "cpu"
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
