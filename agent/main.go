// spark-agent is the device-side daemon of SPARK On-premises. It announces
// the device over UDP, heartbeats to the master, and supervises llama.cpp
// workers (rpc-server / llama-server) on the master's behalf.
package main

import (
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
)

func goos() string   { return runtime.GOOS }
func goarch() string { return runtime.GOARCH }

// Agent bundles every long-lived component behind one lock for master state
// and per-component managers that synchronize internally.
type Agent struct {
	cfg      *Config
	mu       sync.RWMutex
	masterAddr     string
	masterFromBeacon bool
	lastBeacon     time.Time

	invMu     sync.RWMutex
	inventory Inventory

	workers   *WorkerManager
	engine    *EngineManager
	downloads *DownloadManager
}

func NewAgent(cfg *Config) *Agent {
	return &Agent{
		cfg:       cfg,
		workers:   NewWorkerManager(cfg.DataDir),
		engine:    NewEngineManager(cfg),
		downloads: NewDownloadManager(),
	}
}

// currentInventory returns a snapshot, refreshing memory/disk figures so the
// planner sees live free memory rather than boot-time values.
func (a *Agent) currentInventory() Inventory {
	inv := collectInventory(a.cfg.DataDir)
	a.invMu.Lock()
	a.inventory = inv
	a.invMu.Unlock()
	return inv
}

func (a *Agent) cachedInventory() Inventory {
	a.invMu.RLock()
	defer a.invMu.RUnlock()
	return a.inventory
}

// buildAnnounce assembles the payload shared by UDP announces and heartbeats.
func (a *Agent) buildAnnounce() Announce {
	return Announce{
		Protocol:    protoVersion,
		DeviceID:    a.cfg.DeviceID,
		Name:        a.cfg.Name,
		ControlAddr: a.controlAddr(),
		Inventory:   a.currentInventory(),
		Workers:     a.workers.Status(),
		TS:          time.Now().Unix(),
	}
}

func (a *Agent) status() AgentStatus {
	return AgentStatus{
		DeviceID:    a.cfg.DeviceID,
		Name:        a.cfg.Name,
		ControlAddr: a.controlAddr(),
		MasterAddr:  a.getMaster(),
		Inventory:   a.currentInventory(),
		Workers:     a.workers.Status(),
		Engine:      a.engine.Current(),
		Downloads:   a.downloads.Status(),
	}
}

func (a *Agent) shutdown() {
	log.Printf("shutting down: stopping workers")
	a.workers.StopAll()
	os.Exit(0)
}

func (a *Agent) sleepHost() { sleepHost() }

func main() {
	cfg := loadConfig()
	log.SetPrefix("spark-agent ")
	log.SetFlags(log.LstdFlags)

	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "logs"), 0o755); err != nil {
		log.Fatalf("cannot create data dir %s: %v", cfg.DataDir, err)
	}
	log.Printf("starting: device_id=%s name=%q data_dir=%s control_port=%d",
		cfg.DeviceID, cfg.Name, cfg.DataDir, cfg.ControlPort)

	a := NewAgent(cfg)
	// kill llama.cpp processes orphaned by a previous agent instance so
	// they cannot hold ports/memory outside supervision (observed live:
	// auto-heal re-deploy landed on an orphaned llama-server)
	a.workers.ReapOrphans()
	if cfg.MasterHTTP != "" {
		a.setMaster(cfg.MasterHTTP)
	}

	// control server (master -> agent HTTP)
	ln, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.ControlPort)))
	if err != nil {
		log.Fatalf("cannot bind control port %d: %v", cfg.ControlPort, err)
	}
	go func() {
		if err := a.runControlServer(ln); err != nil {
			log.Fatalf("control server: %v", err)
		}
	}()

	go a.runDiscovery()
	go a.runHeartbeat()

	// periodic inventory warm-up so cachedInventory has data early
	a.currentInventory()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	a.shutdown()
}
