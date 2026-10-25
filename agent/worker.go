package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WorkerManager supervises llama.cpp child processes (rpc-server and
// llama-server). Workers that exit unexpectedly are restarted with capped
// exponential backoff; Stop is the only sanctioned way to retire one.
type WorkerManager struct {
	mu      sync.Mutex
	workers map[string]*worker
	dataDir string
}

type worker struct {
	status   WorkerStatus
	cmd      *exec.Cmd
	logPath  string
	stopFlag bool
}

const maxRestarts = 3

func NewWorkerManager(dataDir string) *WorkerManager {
	return &WorkerManager{workers: map[string]*worker{}, dataDir: dataDir}
}

// pidsDir holds one file per supervised worker containing its PID, so a
// restarted agent can kill llama.cpp processes orphaned by its previous
// life (otherwise they hold ports and eat RAM outside supervision).
func (m *WorkerManager) pidsDir() string {
	return filepath.Join(m.dataDir, "pids")
}

func (m *WorkerManager) writePid(id string, pid int) {
	_ = os.MkdirAll(m.pidsDir(), 0o755)
	_ = os.WriteFile(filepath.Join(m.pidsDir(), id+".pid"), []byte(strconv.Itoa(pid)), 0o644)
}

func (m *WorkerManager) clearPid(id string) {
	_ = os.Remove(filepath.Join(m.pidsDir(), id+".pid"))
}

// ReapOrphans kills processes recorded by a previous agent instance that
// this one does not supervise, then clears the records. Called once at
// startup before any worker is launched.
func (m *WorkerManager) ReapOrphans() {
	entries, err := os.ReadDir(m.pidsDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".pid") {
			continue
		}
		id := strings.TrimSuffix(name, ".pid")
		if w, ok := m.workers[id]; ok && w.status.State == "running" {
			continue // already supervised by us (not expected at startup)
		}
		data, err := os.ReadFile(filepath.Join(m.pidsDir(), name))
		if err != nil {
			_ = os.Remove(filepath.Join(m.pidsDir(), name))
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 0 {
			_ = os.Remove(filepath.Join(m.pidsDir(), name))
			continue
		}
		if proc, err := os.FindProcess(pid); err == nil {
			if err := proc.Kill(); err == nil {
				log.Printf("reaped orphan worker %s (pid %d) from previous agent run", id, pid)
			}
		}
		_ = os.Remove(filepath.Join(m.pidsDir(), name))
	}
}

// StartRPC launches an rpc-server worker with well-known id "rpc".
func (m *WorkerManager) StartRPC(engine EngineInfo, port int) (WorkerStatus, error) {
	if engine.RPCPath == "" {
		return WorkerStatus{}, fmt.Errorf("engine not ready: rpc-server binary missing")
	}
	args := []string{"--host", "0.0.0.0", "--port", strconv.Itoa(port)}
	st := WorkerStatus{ID: "rpc", Kind: "rpc", Port: port}
	return m.launch(engine.Dir, engine.RPCPath, args, st)
}

// StartServer launches a llama-server worker. Multiple servers may coexist
// on one node (one per model deployment), keyed by port.
func (m *WorkerManager) StartServer(engine EngineInfo, req StartServerRequest) (WorkerStatus, error) {
	if engine.ServerPath == "" {
		return WorkerStatus{}, fmt.Errorf("engine not ready: llama-server binary missing")
	}
	if req.ModelPath == "" {
		return WorkerStatus{}, fmt.Errorf("model_path is required")
	}
	if _, err := os.Stat(req.ModelPath); err != nil {
		return WorkerStatus{}, fmt.Errorf("model file not accessible: %w", err)
	}
	port := req.Port
	if port == 0 {
		port = 8300
	}
	ctxSize := req.Ctx
	if ctxSize == 0 {
		ctxSize = 4096
	}
	args := []string{
		"--host", "0.0.0.0",
		"--port", strconv.Itoa(port),
		"-m", req.ModelPath,
		"-c", strconv.Itoa(ctxSize),
	}
	if req.Threads > 0 {
		args = append(args, "-t", strconv.Itoa(req.Threads))
	}
	if len(req.RPC) > 0 {
		args = append(args, "--rpc", joinRPC(req.RPC))
	}
	args = append(args, req.ExtraArgs...)

	st := WorkerStatus{
		ID:    fmt.Sprintf("server-%d", port),
		Kind:  "server",
		Port:  port,
		RPC:   req.RPC,
		Model: filepath.Base(req.ModelPath),
	}
	return m.launch(engine.Dir, engine.ServerPath, args, st)
}

func joinRPC(addrs []string) string {
	out := ""
	for i, a := range addrs {
		if i > 0 {
			out += ","
		}
		out += a
	}
	return out
}

func (m *WorkerManager) launch(workDir, bin string, args []string, st WorkerStatus) (WorkerStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, ok := m.workers[st.ID]; ok && prev.status.State != "stopped" && prev.status.State != "failed" {
		return WorkerStatus{}, fmt.Errorf("worker %q already %s; stop it first", st.ID, prev.status.State)
	}
	w := &worker{status: st}
	if err := m.spawn(w, workDir, bin, args); err != nil {
		return WorkerStatus{}, err
	}
	m.workers[st.ID] = w
	go m.monitor(w, workDir, bin, args)
	return w.status, nil
}

// spawn starts the process and wires its output to a per-worker log file.
func (m *WorkerManager) spawn(w *worker, workDir, bin string, args []string) error {
	logsDir := filepath.Join(m.dataDir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return err
	}
	w.logPath = filepath.Join(logsDir, fmt.Sprintf("%s-%d.log", w.status.ID, time.Now().Unix()))
	logFile, err := os.OpenFile(w.logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = workDir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = procAttr()
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start %s: %w", filepath.Base(bin), err)
	}
	w.cmd = cmd
	w.status.State = "running"
	w.status.PID = cmd.Process.Pid
	w.status.StartedAt = time.Now().Unix()
	w.status.Note = fmt.Sprintf("log: %s", w.logPath)
	m.writePid(w.status.ID, cmd.Process.Pid)
	log.Printf("worker %s started (pid %d): %s", w.status.ID, cmd.Process.Pid, w.status.Note)
	return nil
}

// monitor waits for process exit; unexpected exits trigger capped restarts.
// The inner decision loop keeps retrying respawns (with backoff, counted
// against the restart budget) without ever calling Wait on an already-reaped
// process.
func (m *WorkerManager) monitor(w *worker, workDir, bin string, args []string) {
	for {
		err := w.cmd.Wait()
		log.Printf("worker %s exited (%v)", w.status.ID, err)

		for {
			m.mu.Lock()
			if w.stopFlag {
				w.status.State = "stopped"
				w.status.PID = 0
				m.mu.Unlock()
				return
			}
			if w.status.Restarts >= maxRestarts {
				w.status.State = "failed"
				w.status.PID = 0
				w.status.Note = fmt.Sprintf("exited (%v) after %d restarts; log: %s", err, w.status.Restarts, w.logPath)
				m.mu.Unlock()
				log.Printf("worker %s failed permanently", w.status.ID)
				return
			}
			w.status.Restarts++
			w.status.State = "restarting"
			backoff := time.Duration(1<<w.status.Restarts) * time.Second
			m.mu.Unlock()

			time.Sleep(backoff)

			m.mu.Lock()
			if w.stopFlag {
				w.status.State = "stopped"
				w.status.PID = 0
				m.mu.Unlock()
				return
			}
			spawnErr := m.spawn(w, workDir, bin, args)
			m.mu.Unlock()
			if spawnErr == nil {
				break // respawned; back to outer Wait
			}
			log.Printf("worker %s respawn failed: %v", w.status.ID, spawnErr)
		}
	}
}

// Stop retires a worker by id.
func (m *WorkerManager) Stop(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopLocked(id)
}

func (m *WorkerManager) stopLocked(id string) error {
	w, ok := m.workers[id]
	if !ok {
		return fmt.Errorf("worker %q not found", id)
	}
	w.stopFlag = true
	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
	}
	w.status.State = "stopped"
	w.status.PID = 0
	m.clearPid(id)
	return nil
}

// StopAll retires every worker (used on agent shutdown and /shutdown).
func (m *WorkerManager) StopAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.workers))
	for id := range m.workers {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.Stop(id)
	}
}

// Status snapshots all workers for announces and /status.
func (m *WorkerManager) Status() []WorkerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]WorkerStatus, 0, len(m.workers))
	for _, w := range m.workers {
		out = append(out, w.status)
	}
	return out
}
