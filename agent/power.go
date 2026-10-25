package main

import (
	"log"
	"os/exec"
	"runtime"
)

// sleepHost puts the machine to sleep (S3) so idle devices stop burning
// power -- the cost side of the lifecycle mechanism. Wake-on-LAN brings
// them back; the agent resumes heartbeats automatically after wake.
func sleepHost() {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32.exe", "powrprof.dll,SetSuspendState", "0,1,0")
	case "linux":
		cmd = exec.Command("systemctl", "suspend")
	case "darwin":
		cmd = exec.Command("pmset", "sleepnow")
	default:
		log.Printf("power: sleep not supported on %s", runtime.GOOS)
		return
	}
	log.Printf("power: suspending host (%s)", runtime.GOOS)
	if err := cmd.Start(); err != nil {
		log.Printf("power: sleep failed: %v", err)
	}
}
