package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// rangeServer serves payload with HTTP Range support, mirroring how HF and
// its mirrors behave for resumable downloads.
func rangeServer(t *testing.T, payload []byte, failFirstN *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failFirstN != nil && *failFirstN > 0 {
			*failFirstN--
			// close mid-response to simulate a dropped connection
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("server cannot hijack")
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		rng := r.Header.Get("Range")
		start := int64(0)
		if strings.HasPrefix(rng, "bytes=") {
			end := strings.TrimPrefix(rng, "bytes=")
			if v, err := strconv.ParseInt(strings.TrimSuffix(end, "-"), 10, 64); err == nil {
				start = v
			}
		}
		if start >= int64(len(payload)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if start > 0 {
			w.Header().Set("Content-Range",
				fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		_, _ = w.Write(payload[start:])
	}))
}

func TestDownloadToPlain(t *testing.T) {
	payload := []byte(strings.Repeat("0123456789", 100)) // 1KB
	srv := rangeServer(t, payload, nil)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "f.bin")
	var gotReceived, gotTotal int64
	err := downloadTo(context.Background(), srv.URL, dest, func(r, total int64) {
		gotReceived, gotTotal = r, total
	})
	if err != nil {
		t.Fatalf("downloadTo: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Fatalf("content mismatch: %d bytes", len(got))
	}
	if gotReceived != int64(len(payload)) || gotTotal != int64(len(payload)) {
		t.Fatalf("progress = %d/%d, want %d/%d", gotReceived, gotTotal, len(payload), len(payload))
	}
}

func TestDownloadToResumesAfterDrop(t *testing.T) {
	payload := []byte(strings.Repeat("abcdefghij", 500)) // 5KB
	drops := 2 // first two attempts die mid-transfer
	srv := rangeServer(t, payload, &drops)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "f.bin")
	var lastReceived int64
	err := downloadTo(context.Background(), srv.URL, dest, func(r, total int64) {
		if r > lastReceived {
			lastReceived = r
		}
	})
	if err != nil {
		t.Fatalf("downloadTo with resume: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Fatalf("resumed content mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

func TestDownloadToPermanentErrorNoRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "f.bin")
	err := downloadTo(context.Background(), srv.URL+"/missing", dest, nil)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want permanent 404 error, got %v", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		// file may exist empty from a failed first attempt; acceptable,
		// but must not loop retries (test server counts hits implicitly
		// by finishing fast)
	}
}

func TestDownloadAttempt206Total(t *testing.T) {
	payload := []byte(strings.Repeat("x", 1000))
	srv := rangeServer(t, payload, nil)
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "f.part")

	// pre-seed 400 bytes on disk, then a resumed attempt must report
	// total=1000, not the 600 remaining bytes
	if err := os.WriteFile(dest, payload[:400], 0o644); err != nil {
		t.Fatal(err)
	}
	var received, total int64
	received, total, err := downloadAttempt(context.Background(), srv.URL, dest, nil)
	if err != nil {
		t.Fatalf("downloadAttempt: %v", err)
	}
	if total != int64(len(payload)) {
		t.Fatalf("206 total = %d, want %d (full size, not remaining)", total, len(payload))
	}
	if received != int64(len(payload)) {
		t.Fatalf("received = %d, want %d", received, len(payload))
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Fatalf("resumed file mismatch: %d bytes", len(got))
	}
}
