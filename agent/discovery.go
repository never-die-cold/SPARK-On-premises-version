package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strings"
	"time"
)

// discovery runs the agent side of the UDP discovery protocol:
//   - listen on udpAgentPort for SPARK/1 MASTER beacons,
//   - broadcast SPARK/1 ANNOUNCE packets to the master port on every
//     broadcast-capable interface every few seconds.
func (a *Agent) runDiscovery() {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: udpAgentPort})
	if err != nil {
		log.Printf("discovery: cannot bind UDP %d (%v); relying on static master only", udpAgentPort, err)
		return
	}
	go a.announceLoop(conn)
	a.beaconListen(conn)
}

func (a *Agent) announceLoop(conn *net.UDPConn) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		packet, err := a.buildAnnouncePacket()
		if err != nil {
			continue
		}
		for _, addr := range append(a.broadcastAddrs(), "255.255.255.255") {
			target := &net.UDPAddr{IP: net.ParseIP(addr), Port: udpMasterPort}
			_, _ = conn.WriteToUDP(packet, target)
		}
	}
}

func (a *Agent) beaconListen(conn *net.UDPConn) {
	buf := make([]byte, 65536)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return // socket closed on shutdown
		}
		msg := string(buf[:n])
		if !strings.HasPrefix(msg, packetMaster) {
			continue
		}
		var beacon MasterBeacon
		if err := decodePacket([]byte(msg[len(packetMaster):]), &beacon); err != nil {
			continue
		}
		if beacon.Protocol != protoVersion || beacon.MasterAddr == "" {
			continue
		}
		a.setMaster(beacon.MasterAddr)
	}
}

func (a *Agent) buildAnnouncePacket() ([]byte, error) {
	ann := a.buildAnnounce()
	payload, err := encodePacket(ann)
	if err != nil {
		return nil, err
	}
	return []byte(packetAnnounce + string(payload)), nil
}

// broadcastAddrs computes subnet-directed broadcast addresses for every
// interface with a global IPv4 address; these get through APs that drop
// 255.255.255.255.
func (a *Agent) broadcastAddrs() []string {
	ifaceAddrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, ia := range ifaceAddrs {
		ipnet, ok := ia.(*net.IPNet)
		if !ok || ipnet.IP.To4() == nil || ipnet.IP.IsLoopback() {
			continue
		}
		mask := ipnet.Mask
		ip := ipnet.IP.To4()
		bc := make(net.IP, 4)
		for i := 0; i < 4; i++ {
			bc[i] = ip[i] | ^mask[i]
		}
		out = append(out, bc.String())
	}
	return out
}

func encodePacket(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal packet: %w", err)
	}
	return b, nil
}

func decodePacket(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
