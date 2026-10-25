package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
)

// collectInventory gathers the hardware facts the placement planner needs.
// Every probe is best-effort: a device with partial visibility is still
// usable, so failures degrade to zero values instead of errors.
func collectInventory(dataDir string) Inventory {
	inv := Inventory{
		Hostname: mustHostname(),
		OS:       goos(),
		Arch:     goarch(),
	}

	if info, err := host.Info(); err == nil {
		inv.Platform = fmt.Sprintf("%s %s", info.Platform, info.PlatformVersion)
	}

	if cores, err := cpu.Counts(true); err == nil {
		inv.CPUCores = cores
	}
	if infos, err := cpu.Info(); err == nil && len(infos) > 0 {
		inv.CPUModel = strings.TrimSpace(infos[0].Model)
	}

	if vm, err := mem.VirtualMemory(); err == nil {
		inv.MemTotal = vm.Total
		inv.MemAvailable = vm.Available
	}

	if du, err := disk.Usage(dataDir); err == nil {
		inv.DiskFree = du.Free
	}

	inv.GPUs = detectGPUs()
	inv.IPs, inv.MACs = interfaceAddrs()
	return inv
}

var vramRe = regexp.MustCompile(`(\d+)\s*MiB`)

// detectGPUs probes nvidia-smi only; v1 keeps this best-effort and CPU-first.
func detectGPUs() []GPUInfo {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path,
		"--query-gpu=name,memory.total", "--format=csv,noheader").Output()
	if err != nil {
		return nil
	}
	var gpus []GPUInfo
	r := csv.NewReader(strings.NewReader(string(out)))
	if rows, err := r.ReadAll(); err == nil {
		for _, row := range rows {
			if len(row) < 2 {
				continue
			}
			g := GPUInfo{Name: strings.TrimSpace(row[0])}
			if m := vramRe.FindStringSubmatch(strings.TrimSpace(row[1])); m != nil {
				var mib int
				if _, err := fmt.Sscanf(m[1], "%d", &mib); err == nil {
					g.VRAMBytes = uint64(mib) * 1024 * 1024
				}
			}
			gpus = append(gpus, g)
		}
	}
	return gpus
}

func interfaceAddrs() (ips, macs []string) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil
	}
	for _, ifc := range ifaces {
		if ifc.HardwareAddr != nil && ifc.HardwareAddr.String() != "" {
			macs = append(macs, ifc.HardwareAddr.String())
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			ips = append(ips, ip4.String())
		}
	}
	return ips, macs
}

// virtualIF reports interface names that carry NAT/VM addresses (WSL,
// Hyper-V, Docker, VPN taps) which must not be advertised as the device's
// cluster address.
func virtualIF(name string) bool {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "vethernet"), strings.Contains(n, "wsl"),
		strings.Contains(n, "docker"), strings.Contains(n, "vmware"),
		strings.Contains(n, "virtualbox"), strings.Contains(n, "loopback"),
		strings.Contains(n, "tap"), strings.HasPrefix(n, "veth"),
		strings.HasPrefix(n, "br-"), strings.HasPrefix(n, "docker"):
		return true
	}
	return false
}

// preferredIP returns the source address the default route would use,
// which is by definition reachable from the rest of the LAN; falls back to
// the first physical-interface IPv4.
func preferredIP() string {
	conn, err := net.DialTimeout("udp", "223.5.5.5:53", time.Second)
	if err == nil {
		defer conn.Close()
		if host, _, err := net.SplitHostPort(conn.LocalAddr().String()); err == nil && host != "" {
			ip := net.ParseIP(host)
			if ip != nil && ip.To4() != nil && !ip.IsLoopback() {
				return host
			}
		}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	for _, ifc := range ifaces {
		if virtualIF(ifc.Name) {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			return ip4.String()
		}
	}
	return "127.0.0.1"
}

// advertiseIP picks the address agents announce to the cluster: explicit
// flag first, then the default-route source address, then any global IPv4.
func advertiseIP(inv Inventory, override string) string {
	if override != "" {
		return override
	}
	if ip := preferredIP(); ip != "127.0.0.1" {
		return ip
	}
	if len(inv.IPs) > 0 {
		return inv.IPs[0]
	}
	return "127.0.0.1"
}
