package main

import (
	"testing"
)

func TestRewriteHFEndpoint(t *testing.T) {
	const raw = "https://huggingface.co/Qwen/Qwen3-4B-GGUF/resolve/main/Qwen3-4B-Q4_K_M.gguf"

	cases := []struct {
		name     string
		url      string
		endpoint string
		want     string
	}{
		{"no endpoint passes through", raw, "", raw},
		{"mirror rewrite", raw, "https://hf-mirror.com",
			"https://hf-mirror.com/Qwen/Qwen3-4B-GGUF/resolve/main/Qwen3-4B-Q4_K_M.gguf"},
		{"already mirrored stays", "https://hf-mirror.com/x/y.gguf", "https://hf-mirror.com",
			"https://hf-mirror.com/x/y.gguf"},
		{"non-hf url untouched", "https://example.com/a.gguf", "https://hf-mirror.com",
			"https://example.com/a.gguf"},
		{"bad endpoint passes through", raw, "::::not-a-url", raw},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RewriteHFEndpoint(tc.url, tc.endpoint); got != tc.want {
				t.Fatalf("RewriteHFEndpoint(%q, %q) = %q, want %q", tc.url, tc.endpoint, got, tc.want)
			}
		})
	}
}

func TestAssetPatternFor(t *testing.T) {
	cases := []struct{ goos, goarch, want string }{
		{"windows", "amd64", "bin-win-cpu-x64.zip"},
		{"windows", "arm64", "bin-win-cpu-arm64.zip"},
		{"linux", "amd64", "bin-ubuntu-x64.tar.gz"},
		{"linux", "arm64", "bin-ubuntu-arm64.tar.gz"},
		{"darwin", "arm64", "bin-macos-arm64.tar.gz"},
		{"darwin", "amd64", "bin-macos-x64.tar.gz"},
		{"android", "arm64", "bin-android-arm64.tar.gz"},
	}
	for _, tc := range cases {
		got, err := assetPatternFor(tc.goos, tc.goarch)
		if err != nil {
			t.Fatalf("assetPatternFor(%s,%s): %v", tc.goos, tc.goarch, err)
		}
		if got != tc.want {
			t.Fatalf("assetPatternFor(%s,%s) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
		}
	}
	if _, err := assetPatternFor("plan9", "386"); err == nil {
		t.Fatal("expected error for unsupported platform")
	}
}

func TestFindAsset(t *testing.T) {
	assets := []ghAsset{
		{Name: "llama-b11878-bin-ubuntu-cuda-12.8-x64.tar.gz"},
		{Name: "llama-b11878-bin-ubuntu-x64.tar.gz"},
		{Name: "llama-b11878-bin-win-cpu-x64.zip"},
		{Name: "llama-b11878-bin-win-cuda-cu12.0-x64.zip"},
	}
	got, ok := findAsset("bin-win-cpu-x64.zip", assets)
	if !ok || got.Name != "llama-b11878-bin-win-cpu-x64.zip" {
		t.Fatalf("findAsset picked %+v (ok=%v)", got, ok)
	}
	if _, ok := findAsset("bin-macos-arm64.tar.gz", assets); ok {
		t.Fatal("findAsset should not match a missing asset")
	}
}

func TestPacketRoundtrip(t *testing.T) {
	ann := Announce{
		Protocol:    protoVersion,
		DeviceID:    "windows-my-pc-ab12",
		Name:        "my-pc",
		ControlAddr: "192.168.1.5:3971",
		Inventory: Inventory{
			Hostname:     "my-pc",
			OS:           "windows",
			Arch:         "amd64",
			MemTotal:     16 << 30,
			MemAvailable: 8 << 30,
		},
		Workers: []WorkerStatus{{ID: "server", Kind: "server", State: "running", Port: 8300}},
		TS:      1764400000,
	}
	payload, err := encodePacket(ann)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back Announce
	if err := decodePacket(payload, &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.DeviceID != ann.DeviceID || back.ControlAddr != ann.ControlAddr ||
		back.Inventory.MemAvailable != ann.Inventory.MemAvailable || len(back.Workers) != 1 {
		t.Fatalf("roundtrip mismatch: %+v", back)
	}
}

func TestJoinRPC(t *testing.T) {
	if got := joinRPC([]string{"a:1", "b:2"}); got != "a:1,b:2" {
		t.Fatalf("joinRPC = %q", got)
	}
}
