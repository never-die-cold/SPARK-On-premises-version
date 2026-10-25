package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const userAgent = "spark-agent/0.1"

// DownloadManager tracks model download jobs so the master can poll progress
// while gigabyte-scale GGUF files stream in.
type DownloadManager struct {
	mu    sync.Mutex
	jobs  map[string]*DownloadStatus
	order []string
}

func NewDownloadManager() *DownloadManager {
	return &DownloadManager{jobs: map[string]*DownloadStatus{}}
}

// Start launches a background download of url into <dataDir>/models/<filename>
// and returns the job id. An existing complete file short-circuits to done.
func (m *DownloadManager) Start(dataDir string, req DownloadRequest) (string, error) {
	if req.URL == "" || req.Filename == "" {
		return "", fmt.Errorf("url and filename are required")
	}
	if strings.ContainsRune(req.Filename, '/') || strings.ContainsRune(req.Filename, '\\') {
		return "", fmt.Errorf("filename must be a bare file name")
	}
	id := newJobID()
	job := &DownloadStatus{
		ID:       id,
		URL:      req.URL,
		Filename: req.Filename,
		State:    "running",
	}
	m.mu.Lock()
	m.jobs[id] = job
	m.order = append(m.order, id)
	m.mu.Unlock()

	go m.run(job, dataDir, req)
	return id, nil
}

func (m *DownloadManager) run(job *DownloadStatus, dataDir string, req DownloadRequest) {
	modelsDir := filepath.Join(dataDir, "models")
	if err := os.MkdirAll(modelsDir, 0o755); err != nil {
		m.fail(job, err)
		return
	}
	final := filepath.Join(modelsDir, req.Filename)
	tmp := final + ".part"

	realURL := RewriteHFEndpoint(req.URL, firstNonEmpty(req.HFEndpoint, os.Getenv("SPARK_HF_ENDPOINT")))

	// short-circuit if the file is already there with the expected size
	if st, err := os.Stat(final); err == nil {
		m.mu.Lock()
		job.State = "done"
		job.Received = st.Size()
		job.Total = st.Size()
		job.Filename = req.Filename
		m.mu.Unlock()
		return
	}

	progress := func(received, total int64) {
		m.mu.Lock()
		job.Received, job.Total = received, total
		m.mu.Unlock()
	}
	// seed the job with bytes already on disk so UIs show real progress
	// the moment a resume starts
	if st, err := os.Stat(tmp); err == nil {
		m.mu.Lock()
		job.Received = st.Size()
		m.mu.Unlock()
	}
	if err := downloadTo(context.Background(), realURL, tmp, progress); err != nil {
		m.fail(job, err)
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		m.fail(job, err)
		return
	}
	m.mu.Lock()
	job.State = "done"
	if job.Total == 0 {
		if st, err := os.Stat(final); err == nil {
			job.Received, job.Total = st.Size(), st.Size()
		}
	}
	m.mu.Unlock()
}

func (m *DownloadManager) fail(job *DownloadStatus, err error) {
	m.mu.Lock()
	job.State = "error"
	job.Error = err.Error()
	m.mu.Unlock()
}

// Status snapshots all jobs (newest last).
func (m *DownloadManager) Status() []DownloadStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]DownloadStatus, 0, len(m.order))
	for _, id := range m.order {
		if j, ok := m.jobs[id]; ok {
			out = append(out, *j)
		}
	}
	return out
}

// PathFor returns the local path of a downloaded model file.
func (m *DownloadManager) PathFor(dataDir, filename string) string {
	return filepath.Join(dataDir, "models", filename)
}

// RewriteHFEndpoint swaps the huggingface.co host of url for endpoint (e.g.
// https://hf-mirror.com). Empty endpoint or non-HF urls pass through.
func RewriteHFEndpoint(rawURL, endpoint string) string {
	if endpoint == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	host := strings.ToLower(u.Host)
	if host != "huggingface.co" && !strings.HasSuffix(host, ".huggingface.co") {
		return rawURL
	}
	ep, err := url.Parse(endpoint)
	if err != nil || ep.Host == "" {
		return rawURL
	}
	u.Scheme = ep.Scheme
	u.Host = ep.Host
	return u.String()
}

// downloadTo streams url to dest with progress callbacks and automatic
// resume: on transient failures it retries with HTTP Range from the bytes
// already on disk, so multi-GB model downloads survive flaky links.
// total is 0 when the server does not report a size.
func downloadTo(ctx context.Context, rawURL, dest string, progress func(received, total int64)) error {
	const maxAttempts = 6
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		received, total, err := downloadAttempt(ctx, rawURL, dest, progress)
		if err == nil {
			if progress != nil {
				progress(received, total)
			}
			return nil
		}
		lastErr = err
		if !isRetryable(err) {
			return err
		}
		log.Printf("download attempt %d/%d failed (%s bytes on disk): %v",
			attempt+1, maxAttempts, dest, err)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return fmt.Errorf("download %s failed after %d attempts: %w", rawURL, maxAttempts, lastErr)
}

// downloadAttempt performs one streaming pass, appending to an existing
// partial file via Range when the server supports it. It is not retried
// on permanent errors (4xx other than 416, bad URL).
func downloadAttempt(ctx context.Context, rawURL, dest string, progress func(received, total int64)) (int64, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("User-Agent", userAgent)

	var received int64
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		received = st.Size()
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", received))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return received, 0, retryable(fmt.Errorf("connect: %w", err))
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusPartialContent: // resumed
	case resp.StatusCode == http.StatusOK: // server ignored Range -> restart
		received = 0
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && received > 0:
		// already complete on disk; caller verifies via progress
		return received, received, nil
	default:
		return received, 0, permanent(fmt.Errorf("HTTP %d", resp.StatusCode))
	}

	total := resp.ContentLength // 200: full size; 206: remaining bytes
	if total < 0 {
		total = 0
	}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		// Content-Range: bytes start-end/total
		if slash := strings.LastIndex(cr, "/"); slash >= 0 {
			if t, err := strconv.ParseInt(cr[slash+1:], 10, 64); err == nil && t > 0 {
				total = t
			}
		}
	}
	if resp.StatusCode == http.StatusPartialContent && total <= received && resp.ContentLength >= 0 {
		// server answered 206 without a usable Content-Range
		total = received + resp.ContentLength
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return received, total, permanent(err)
	}
	openFlags := os.O_CREATE | os.O_WRONLY
	if received > 0 {
		openFlags |= os.O_APPEND
	} else {
		openFlags |= os.O_TRUNC
	}
	out, err := os.OpenFile(dest, openFlags, 0o644)
	if err != nil {
		return received, total, permanent(err)
	}
	defer out.Close()

	buf := make([]byte, 1024*512)
	var lastTick time.Time
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return received, total, permanent(werr)
			}
			received += int64(n)
			if progress != nil && time.Since(lastTick) > 500*time.Millisecond {
				progress(received, total)
				lastTick = time.Now()
			}
		}
		if rerr == io.EOF {
			return received, total, nil
		}
		if rerr != nil {
			return received, total, retryable(rerr)
		}
	}
}

type retryableError struct{ err error }

func (e retryableError) Error() string { return e.err.Error() }
func (e retryableError) Unwrap() error { return e.err }

func retryable(err error) error   { return retryableError{err} }
func permanent(err error) error   { return err }
func isRetryable(err error) bool {
	var re retryableError
	return errors.As(err, &re)
}

var _ = isRetryable // used by tests

func newJobID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:]) + fmt.Sprintf("-%d", time.Now().Unix())
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
