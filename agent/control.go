package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// controlServer is the HTTP surface the master drives: engine install,
// model downloads, worker lifecycle. Guarded by the shared cluster token
// when one is configured (v1 trust model: trusted LAN).
func (a *Agent) runControlServer(ln net.Listener) error {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.status())
	})

	mux.HandleFunc("POST /engine/ensure", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
		defer cancel()
		info, err := a.engine.Ensure(ctx)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, info)
	})

	mux.HandleFunc("POST /models/download", func(w http.ResponseWriter, r *http.Request) {
		var req DownloadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		id, err := a.downloads.Start(a.cfg.DataDir, req)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"job_id": id})
	})

	mux.HandleFunc("GET /models/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.downloads.Status())
	})

	mux.HandleFunc("GET /models/path", func(w http.ResponseWriter, r *http.Request) {
		filename := r.URL.Query().Get("filename")
		p := a.downloads.PathFor(a.cfg.DataDir, filename)
		st, err := os.Stat(p)
		exists := err == nil && !st.IsDir()
		writeJSON(w, http.StatusOK, map[string]any{"path": p, "exists": exists})
	})

	mux.HandleFunc("POST /workers/rpc", func(w http.ResponseWriter, r *http.Request) {
		var req StartRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if req.Port == 0 {
			req.Port = 50052
		}
		engine, err := a.engine.Ensure(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		st, err := a.workers.StartRPC(engine, req.Port)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, st)
	})

	mux.HandleFunc("POST /workers/server", func(w http.ResponseWriter, r *http.Request) {
		var req StartServerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		req.ModelPath = a.downloads.PathFor(a.cfg.DataDir, filepath.Base(req.ModelPath))
		engine, err := a.engine.Ensure(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		st, err := a.workers.StartServer(engine, req)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, st)
	})

	mux.HandleFunc("POST /workers/stop", func(w http.ResponseWriter, r *http.Request) {
		var req StopWorkerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := a.workers.Stop(req.ID); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
	})

	mux.HandleFunc("POST /power/sleep", func(w http.ResponseWriter, r *http.Request) {
		// refuse to sleep a node that is serving deployments
		for _, wk := range a.workers.Status() {
			if wk.State == "running" || wk.State == "restarting" {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": fmt.Sprintf("worker %s is %s; stop deployments first", wk.ID, wk.State),
				})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "sleeping soon"})
		go func() {
			time.Sleep(1 * time.Second)
			a.sleepHost()
		}()
	})

	mux.HandleFunc("POST /shutdown", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "shutting down"})
		go func() {
			time.Sleep(300 * time.Millisecond)
			a.shutdown()
		}()
	})

	srv := &http.Server{Handler: a.withToken(mux)}
	return srv.Serve(ln)
}

// withToken enforces the shared cluster token when configured.
func (a *Agent) withToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.Token != "" && r.Header.Get("X-SPARK-Token") != a.cfg.Token {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bad or missing X-SPARK-Token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *Agent) controlAddr() string {
	return net.JoinHostPort(advertiseIP(a.currentInventory(), a.cfg.AdvertiseIP), strconv.Itoa(a.cfg.ControlPort))
}
