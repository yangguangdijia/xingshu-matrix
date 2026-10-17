// Package proto is the JSON-over-WebSocket wire protocol between
// matrix-agent and matrix-server (hosts module).
//
// Every WebSocket text frame is one Envelope. Connection flow:
//
//	enrol:  GET /api/v1/agent/ws?token=<enrol token>   agent→hello, server→welcome{host_id,secret}, server closes
//	run:    GET /api/v1/agent/ws  (Authorization: Bearer <secret>, X-Matrix-Host-ID: <id>)
//	        agent→hello, server→welcome{host_id}; then heartbeat / exec / file_push frames.
package proto

import "encoding/json"

// Frame types.
const (
	// agent → server
	TypeHello      = "hello"       // Hello
	TypeHeartbeat  = "heartbeat"   // Heartbeat
	TypeExecOutput = "exec_output" // ExecOutput
	TypeExecResult = "exec_result" // ExecResult
	TypeFileResult = "file_result" // FileResult
	// server → agent
	TypeWelcome  = "welcome"   // Welcome
	TypeExec     = "exec"      // Exec
	TypeFilePush = "file_push" // FilePush
	TypeError    = "error"     // Error
)

// Header names used when an enrolled agent reconnects.
const (
	HeaderHostID = "X-Matrix-Host-ID"
	WSPath       = "/api/v1/agent/ws"
	// MaxOutput is the maximum captured output per task (bytes).
	MaxOutput = 1 << 20
	// MaxFile is the maximum decoded file size for file_push (bytes).
	MaxFile = 1 << 20
)

type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

func Encode(typ string, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{Type: typ, Data: b})
}

type Disk struct {
	Mount   string  `json:"mount"`
	FSType  string  `json:"fstype"`
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	Percent float64 `json:"percent"`
}

// Facts are collected by the agent with gopsutil.
type Facts struct {
	Hostname        string   `json:"hostname"`
	OS              string   `json:"os"`
	Platform        string   `json:"platform"`
	PlatformVersion string   `json:"platform_version"`
	Kernel          string   `json:"kernel"`
	Arch            string   `json:"arch"`
	CPUModel        string   `json:"cpu_model"`
	CPUCores        int      `json:"cpu_cores"`
	CPUPercent      float64  `json:"cpu_percent"`
	Load1           float64  `json:"load1"`
	MemTotal        uint64   `json:"mem_total"`
	MemUsed         uint64   `json:"mem_used"`
	MemPercent      float64  `json:"mem_percent"`
	Disks           []Disk   `json:"disks"`
	Uptime          uint64   `json:"uptime"`
	BootTime        uint64   `json:"boot_time"`
	IPs             []string `json:"ips"`
}

type Hello struct {
	AgentVersion string `json:"agent_version"`
	Facts        Facts  `json:"facts"`
}

type Heartbeat struct {
	Facts *Facts `json:"facts,omitempty"`
}

type Welcome struct {
	HostID int64  `json:"host_id"`
	Secret string `json:"secret,omitempty"` // only on enrolment; agent must persist it
}

type Exec struct {
	TaskID     int64  `json:"task_id"`
	Command    string `json:"command"`
	TimeoutSec int    `json:"timeout_sec"`
}

type ExecOutput struct {
	TaskID int64  `json:"task_id"`
	Stream string `json:"stream"` // stdout | stderr
	Data   string `json:"data"`
}

type ExecResult struct {
	TaskID     int64  `json:"task_id"`
	ExitCode   int    `json:"exit_code"`
	TimedOut   bool   `json:"timed_out"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

type FilePush struct {
	TaskID        int64  `json:"task_id"`
	Path          string `json:"path"`
	ContentBase64 string `json:"content_base64"`
	Mode          uint32 `json:"mode"` // e.g. 0644; 0 → 0644
}

type FileResult struct {
	TaskID int64  `json:"task_id"`
	Bytes  int    `json:"bytes"`
	Error  string `json:"error,omitempty"`
}

type Error struct {
	Message string `json:"message"`
}
