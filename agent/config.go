package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Config carries every agent setting; every field has a flag and (where useful)
// an environment fallback so platform installers can use either.
type Config struct {
	Name        string
	DataDir     string
	ControlPort int
	MasterHTTP  string // static master address, optional; beacons also set it
	AdvertiseIP string // override the auto-picked announcement IP
	DeviceID    string
	LLaMAPath   string // directory with llama-server / rpc-server binaries
	LLaMATag    string // llama.cpp release tag, e.g. b11878
	LLaMAFlavor string // cpu | cuda | vulkan | auto (default: auto -> cuda when a GPU is present)
	HFEndpoint  string // huggingface mirror, e.g. https://hf-mirror.com
	Token       string // shared cluster token for the control channel
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".spark-agent"
	}
	return filepath.Join(home, ".spark-agent")
}

func loadConfig() *Config {
	cfg := &Config{}
	flag.StringVar(&cfg.Name, "name", envOr("SPARK_AGENT_NAME", ""), "device display name (default: hostname)")
	flag.StringVar(&cfg.DataDir, "data-dir", envOr("SPARK_DATA_DIR", defaultDataDir()), "data directory (identity, engines, models, logs)")
	flag.IntVar(&cfg.ControlPort, "control-port", 3971, "HTTP control port the master talks to")
	flag.StringVar(&cfg.MasterHTTP, "master", envOr("SPARK_MASTER", ""), "static master address host:port (optional, beacons work too)")
	flag.StringVar(&cfg.AdvertiseIP, "advertise", envOr("SPARK_ADVERTISE_IP", ""), "IP to advertise to the cluster (default: auto-detect)")
	flag.StringVar(&cfg.LLaMAPath, "llama-path", envOr("SPARK_LLAMA_PATH", ""), "use local llama.cpp binaries in this directory instead of downloading")
	flag.StringVar(&cfg.LLaMATag, "llama-tag", envOr("SPARK_LLAMA_TAG", ""), "llama.cpp release tag to use (default: pinned, or latest discovered)")
	flag.StringVar(&cfg.LLaMAFlavor, "llama-flavor", envOr("SPARK_LLAMA_FLAVOR", "auto"), "engine build: auto|cpu|cuda|vulkan (auto picks cuda when a GPU is detected)")
	flag.StringVar(&cfg.HFEndpoint, "hf-endpoint", envOr("SPARK_HF_ENDPOINT", ""), "HuggingFace endpoint rewrite for model downloads")
	flag.StringVar(&cfg.Token, "token", envOr("SPARK_TOKEN", ""), "shared cluster token guarding the control channel")
	flag.Parse()

	if cfg.Name == "" {
		h, err := os.Hostname()
		if err != nil || h == "" {
			cfg.Name = "device"
		} else {
			cfg.Name = h
		}
	}
	cfg.DeviceID = loadOrCreateIdentity(cfg.DataDir)
	return cfg
}

// loadOrCreateIdentity gives each install a stable device id (hostname + 4 hex
// chars) so the master registry survives agent restarts.
func loadOrCreateIdentity(dataDir string) string {
	path := filepath.Join(dataDir, "identity")
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id
		}
	}
	var b [2]byte
	_, _ = rand.Read(b[:])
	id := fmt.Sprintf("%s-%s-%s", runtime.GOOS, mustHostname(), hex.EncodeToString(b[:]))
	_ = os.MkdirAll(dataDir, 0o755)
	_ = os.WriteFile(path, []byte(id), 0o644)
	return id
}

func mustHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "device"
	}
	// keep ids filesystem/registry friendly
	h = strings.ToLower(h)
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, h)
}
