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

// advertiseIP picks the address agents announce to the cluster: explicit
// flag first, then the first global IPv4 (interface order is a reasonable
// proxy for the default route on stock home setups).
func advertiseIP(inv Inventory, override string) string {
	if override != "" {
		return override
	}
	if len(inv.IPs) > 0 {
		return inv.IPs[0]
	}
	return "127.0.0.1"
}
