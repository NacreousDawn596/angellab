// Package ipc defines the AngelLab wire protocol.
//
// All communication flows over a single Unix domain socket at
// /run/angellab/lab.sock using length-prefixed msgpack frames.
//
// Every new connection begins with a mandatory HELLO exchange:
//
//	Connector sends: KindHello {Version: 1, Role: "angel"|"cli"}
//	Listener replies: KindHello {Version: 1, Role: "lab"}
//
// Frame layout:
//
//	┌──────────────────────────────────────────┐
//	│  4 bytes: uint32 payload length (BE)     │
//	│  N bytes: msgpack-encoded Message        │
//	└──────────────────────────────────────────┘
package ipc

import "time"

const ProtocolVersion uint8 = 1
const MaxFrameSize = 16 << 20

// MessageKind identifies the purpose of a message on the wire.
type MessageKind uint8

const (
	KindHello     MessageKind = iota + 1 // bidirectional handshake, always first frame
	KindRegister                          // angel → lab: initial registration after HELLO
	KindHeartbeat                         // angel → lab: periodic telemetry
	KindEvent                             // angel → lab: monitoring event

	KindCmdPing      // lab → angel: liveness probe (expects KindPong reply)
	KindCmdPong      // angel → lab: reply to KindCmdPing
	KindCmdStatus    // lab → angel: request status snapshot
	KindCmdTerminate // lab → angel: graceful shutdown

	KindCLIRequest  // cli → lab
	KindCLIResponse // lab → cli (single response)
	KindEventStream // lab → cli (streaming push)
)

// Message is the top-level envelope for every IPC frame.
type Message struct {
	Version       uint8       `msgpack:"v"`
	Kind          MessageKind `msgpack:"k"`
	CorrelationID string      `msgpack:"cid,omitempty"`
	Payload       []byte      `msgpack:"p,omitempty"`
}

// ---------------------------------------------------------------------------
// HELLO handshake
// ---------------------------------------------------------------------------

type Role string

const (
	RoleAngel Role = "angel"
	RoleCLI   Role = "cli"
	RoleLab   Role = "lab"
)

// HelloPayload is exchanged as the very first frame on every connection.
type HelloPayload struct {
	Version uint8  `msgpack:"v"`
	Role    Role   `msgpack:"role"`
	Binary  string `msgpack:"binary,omitempty"` // informational only
}

// ---------------------------------------------------------------------------
// Angel → Lab payloads
// ---------------------------------------------------------------------------

type RegisterPayload struct {
	AngelID   string `msgpack:"angel_id"`
	AngelType string `msgpack:"angel_type"`
	PID       int    `msgpack:"pid"`
}

// HeartbeatPayload carries live telemetry on each heartbeat tick.
type HeartbeatPayload struct {
	AngelID    string  `msgpack:"angel_id"`
	State      string  `msgpack:"state"`
	Uptime     int64   `msgpack:"uptime_s"`
	CPUPercent float64 `msgpack:"cpu_pct"`
	RSSBytes   uint64  `msgpack:"rss_bytes"`
	Goroutines int     `msgpack:"goroutines"`
	// FDCount is read from /proc/self/fd — detects file descriptor leaks
	// in long-running angel processes before they become critical.
	FDCount   int               `msgpack:"fd_count"`
	AngelMeta map[string]string `msgpack:"meta,omitempty"`
}

type Severity uint8

const (
	SeverityInfo     Severity = iota + 1
	SeverityWarn
	SeverityCritical
)

func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "INFO"
	case SeverityWarn:
		return "WARN"
	case SeverityCritical:
		return "CRIT"
	default:
		return "UNKN"
	}
}

type EventPayload struct {
	AngelID   string            `msgpack:"angel_id"`
	Severity  Severity          `msgpack:"severity"`
	Message   string            `msgpack:"message"`
	Timestamp time.Time         `msgpack:"ts"`
	Meta      map[string]string `msgpack:"meta,omitempty"`
}

// ---------------------------------------------------------------------------
// CLI ↔ Lab payloads
// ---------------------------------------------------------------------------

type CLICommand string

const (
	CLICmdAngelCreate    CLICommand = "angel.create"
	CLICmdAngelList      CLICommand = "angel.list"
	CLICmdAngelInspect   CLICommand = "angel.inspect"
	CLICmdAngelTerminate CLICommand = "angel.terminate"
	CLICmdLabStatus      CLICommand = "lab.status"
	CLICmdEventSubscribe CLICommand = "event.subscribe"
	CLICmdAngelDiff      CLICommand = "angel.diff"
)

type CLIRequest struct {
	Command CLICommand        `msgpack:"cmd"`
	Args    map[string]string `msgpack:"args,omitempty"`
}

type CLIResponse struct {
	OK    bool   `msgpack:"ok"`
	Data  []byte `msgpack:"data,omitempty"`
	Error string `msgpack:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// CLI response body types
// ---------------------------------------------------------------------------

type AngelSummary struct {
	ID           string    `msgpack:"id"`
	AngelType    string    `msgpack:"type"`
	State        string    `msgpack:"state"`
	ConnState    string    `msgpack:"conn_state"`
	PID          int       `msgpack:"pid"`
	RestartCount int       `msgpack:"restarts"`
	LastSeen     time.Time `msgpack:"last_seen"`
}

type AngelDetail struct {
	AngelSummary
	CreatedAt    time.Time         `msgpack:"created_at"`
	ConfigJSON   string            `msgpack:"config_json"`
	Telemetry    *HeartbeatPayload `msgpack:"telemetry,omitempty"`
	RecentEvents []EventPayload    `msgpack:"recent_events,omitempty"`
}

type LabStatus struct {
	Version   string         `msgpack:"version"`
	PID       int            `msgpack:"pid"`
	Uptime    int64          `msgpack:"uptime_s"`
	StartedAt time.Time      `msgpack:"started_at"`
	Angels    []AngelSummary `msgpack:"angels"`
}
