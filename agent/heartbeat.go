package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"time"
)

// heartbeat POSTs the announce document to the master every few seconds once
// a master address is known (static config or UDP beacon). If the master was
// learned from beacons and beacons stop arriving, the agent forgets it and
// falls back to announce-only discovery.
const (
	heartbeatInterval = 5 * time.Second
	masterStaleAfter  = 90 * time.Second
)

func (a *Agent) runHeartbeat() {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for range ticker.C {
		master := a.getMaster()
		if master == "" {
			continue
		}
		if a.masterFromBeacon && time.Since(a.lastBeacon) > masterStaleAfter {
			a.setMaster("")
			continue
		}
		body, err := encodePacket(a.buildAnnounce())
		if err != nil {
			continue
		}
		url := fmt.Sprintf("http://%s/api/heartbeat", master)
		client := &http.Client{Timeout: 4 * time.Second}
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", userAgent)
		if a.cfg.Token != "" {
			req.Header.Set("X-SPARK-Token", a.cfg.Token)
		}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("heartbeat: %v", err)
			continue
		}
		_ = resp.Body.Close()
	}
}

func (a *Agent) getMaster() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.masterAddr
}

// setMaster records the current master address. An empty value clears it.
// A static --master config is authoritative and ignores foreign beacons.
func (a *Agent) setMaster(addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if addr == "" {
		a.masterAddr = ""
		a.masterFromBeacon = false
		return
	}
	if a.cfg.MasterHTTP != "" && addr != a.cfg.MasterHTTP {
		return
	}
	if a.masterAddr == addr {
		if a.masterFromBeacon {
			a.lastBeacon = time.Now()
		}
		return
	}
	a.masterAddr = addr
	a.masterFromBeacon = a.cfg.MasterHTTP == ""
	a.lastBeacon = time.Now()
	log.Printf("master discovered at %s", addr)
}
