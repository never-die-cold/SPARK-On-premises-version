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

func TestAssetPlanFor(t *testing.T) {
	cases := []struct {
		goos, goarch, flavor string
		wantPattern          string
		wantExtra            bool
	}{
		{"windows", "amd64", "cpu", "bin-win-cpu-x64.zip", false},
		{"windows", "amd64", "cuda", "bin-win-cuda-13.4-x64.zip", true},
		{"windows", "arm64", "cpu", "bin-win-cpu-arm64.zip", false},
		{"linux", "amd64", "cpu", "bin-ubuntu-x64.tar.gz", false},
		{"linux", "amd64", "cuda", "bin-ubuntu-cuda-13.4-x64.tar.gz", false},
		{"linux", "arm64", "cpu", "bin-ubuntu-arm64.tar.gz", false},
		{"linux", "amd64", "vulkan", "bin-ubuntu-vulkan-x64.tar.gz", false},
		{"darwin", "arm64", "cpu", "bin-macos-arm64.tar.gz", false},
		{"darwin", "amd64", "cpu", "bin-macos-x64.tar.gz", false},
		{"android", "arm64", "cpu", "bin-android-arm64.tar.gz", false},
	}
	for _, tc := range cases {
		plan, err := assetPlanFor(tc.goos, tc.goarch, tc.flavor)
		if err != nil {
			t.Fatalf("assetPlanFor(%s,%s,%s): %v", tc.goos, tc.goarch, tc.flavor, err)
		}
		if plan.patterns[0] != tc.wantPattern {
			t.Fatalf("assetPlanFor(%s,%s,%s) first pattern = %q, want %q",
				tc.goos, tc.goarch, tc.flavor, plan.patterns[0], tc.wantPattern)
		}
		if tc.wantExtra && len(plan.extra) == 0 {
			t.Fatalf("assetPlanFor(%s,%s,%s) missing runtime pack", tc.goos, tc.goarch, tc.flavor)
		}
	}
	if _, err := assetPlanFor("plan9", "386", "cpu"); err == nil {
		t.Fatal("expected error for unsupported platform")
	}
	if _, err := assetPlanFor("linux", "arm64", "cuda"); err == nil {
		t.Fatal("expected error: cuda has no linux arm64 build")
	}
	if _, err := assetPlanFor("linux", "amd64", "metal"); err == nil {
		t.Fatal("expected error for unknown flavor")
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

func TestAssetFileName(t *testing.T) {
	cases := []struct{ tag, pattern, want string }{
		{"b11878", "bin-win-cpu-x64.zip", "llama-b11878-bin-win-cpu-x64.zip"},
		{"b11878", "cudart-llama-bin-win-cuda-13.4-x64.zip", "cudart-llama-bin-win-cuda-13.4-x64.zip"},
		{"b11878", "llama-b11878-something.zip", "llama-b11878-something.zip"},
	}
	for _, tc := range cases {
		if got := assetFileName(tc.tag, tc.pattern); got != tc.want {
			t.Fatalf("assetFileName(%q,%q) = %q, want %q", tc.tag, tc.pattern, got, tc.want)
		}
	}
}

func TestJoinRPC(t *testing.T) {
	if got := joinRPC([]string{"a:1", "b:2"}); got != "a:1,b:2" {
		t.Fatalf("joinRPC = %q", got)
	}
}
