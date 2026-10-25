package main

// Wire protocol structs shared between agent, master and the UDP packets.

const (
	protoVersion = 1
	// udpAgentPort is the UDP port agents listen on for master beacons.
	udpAgentPort = 3970
	// udpMasterPort is the UDP port the master listens on for agent announces.
	udpMasterPort = 3969

	packetAnnounce = "SPARK/1 ANNOUNCE "
	packetMaster   = "SPARK/1 MASTER "
)

// Inventory describes device hardware as reported by the agent.
type Inventory struct {
	Hostname     string     `json:"hostname"`
	OS           string     `json:"os"`       // runtime.GOOS
	Arch         string     `json:"arch"`     // runtime.GOARCH
	Platform     string     `json:"platform"` // human-friendly OS string
	CPUModel     string     `json:"cpu_model"`
	CPUCores     int        `json:"cpu_cores"`
	MemTotal     uint64     `json:"mem_total"`
	MemAvailable uint64     `json:"mem_available"`
	DiskFree     uint64     `json:"disk_free"`
	GPUs         []GPUInfo  `json:"gpus,omitempty"`
	IPs          []string   `json:"ips"`
	MACs         []string   `json:"macs"`
}

type GPUInfo struct {
	Name      string `json:"name"`
	VRAMBytes uint64 `json:"vram_bytes"`
}

// Announce is the payload agents broadcast over UDP and POST to the master
// heartbeat endpoint; the two channels carry the same document.
type Announce struct {
	Protocol    int            `json:"protocol"`
	DeviceID    string         `json:"device_id"`
	Name        string         `json:"name"`
	ControlAddr string         `json:"control_addr"`
	Inventory   Inventory      `json:"inventory"`
	Workers     []WorkerStatus `json:"workers"`
	TS          int64          `json:"ts"`
}

// MasterBeacon is broadcast by the master so agents learn its HTTP address.
type MasterBeacon struct {
	Protocol   int    `json:"protocol"`
	MasterAddr string `json:"master_addr"`
	TS         int64  `json:"ts"`
}

type WorkerStatus struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`  // "rpc" | "server"
	State     string   `json:"state"` // running | restarting | stopped | failed
	Port      int      `json:"port,omitempty"`
	RPC       []string `json:"rpc,omitempty"`
	Model     string   `json:"model,omitempty"`
	PID       int      `json:"pid,omitempty"`
	Restarts  int      `json:"restarts"`
	StartedAt int64    `json:"started_at,omitempty"`
	Note      string   `json:"note,omitempty"`
}

type EngineInfo struct {
	Tag        string `json:"tag,omitempty"`
	Dir        string `json:"dir,omitempty"`
	ServerPath string `json:"server_path,omitempty"`
	RPCPath    string `json:"rpc_path,omitempty"`
}

type AgentStatus struct {
	DeviceID    string           `json:"device_id"`
	Name        string           `json:"name"`
	ControlAddr string           `json:"control_addr"`
	MasterAddr  string           `json:"master_addr,omitempty"`
	Inventory   Inventory        `json:"inventory"`
	Workers     []WorkerStatus   `json:"workers"`
	Engine      EngineInfo       `json:"engine"`
	Downloads   []DownloadStatus `json:"downloads"`
}

type DownloadStatus struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Filename string `json:"filename"`
	State    string `json:"state"` // running | done | error
	Received int64  `json:"received"`
	Total    int64  `json:"total"`
	Error    string `json:"error,omitempty"`
}

// Control API request payloads (master -> agent HTTP).

type StartRPCRequest struct {
	Port int `json:"port"`
}

type StartServerRequest struct {
	ModelPath string   `json:"model_path"`
	Port      int      `json:"port"`
	RPC       []string `json:"rpc,omitempty"`
	Ctx       int      `json:"ctx,omitempty"`
	Threads   int      `json:"threads,omitempty"`
	ExtraArgs []string `json:"extra_args,omitempty"`
}

type StopWorkerRequest struct {
	ID string `json:"id"`
}

type DownloadRequest struct {
	URL        string `json:"url"`
	Filename   string `json:"filename"`
	HFEndpoint string `json:"hf_endpoint,omitempty"`
}
