# IPC Protocol

AngelLab uses a custom binary protocol over Unix domain sockets. This document describes every message type, the connection lifecycle, and the wire encoding in detail.

---

## Transport

All communication goes over the single Unix socket at `/run/angellab/lab.sock` (configurable via `[lab] socket`). The socket is a stream socket (`SOCK_STREAM`). Every new connection begins with a HELLO handshake; frames after that carry the application-level messages.

### Frame encoding

Every message is a **length-prefixed msgpack frame**:

```
Offset  Size  Description
──────  ────  ───────────────────────────────────────────────
0       4     uint32, big-endian — payload length in bytes
4       N     msgpack-encoded Message struct
```

Maximum frame size: **16 MiB** (`MaxFrameSize = 16 << 20`). Frames larger than this are rejected and the connection is closed.

The `Message` struct:

```go
type Message struct {
    Version       uint8       `msgpack:"v"`
    Kind          MessageKind `msgpack:"k"`
    CorrelationID string      `msgpack:"cid,omitempty"`
    Payload       []byte      `msgpack:"p,omitempty"`
}
```

`CorrelationID` is used to match CLI request/response pairs. Angels do not use it. The `Payload` field contains a nested msgpack-encoded type specific to the message kind (see below).

### Read/write deadlines

- `helloTimeout` (5 s): applied to the HELLO exchange. If no HELLO is received within 5 s, the connection is closed.
- `DefaultRWTimeout` (30 s): applied to each individual `Send` and `Recv` call.
- Angel connection deadline: set to `2 × HeartbeatTimeout` on the Lab side. If no frame arrives within this window, the connection is declared dead.
- CLI connections: deadline is set per-request; event stream connections have no deadline.

---

## Protocol version

`ProtocolVersion = 1`

The version is embedded in every `Message.Version` field and in the `HelloPayload.Version` field. If the versions do not match, the connection is closed immediately after the HELLO.

---

## Connection roles

Three roles connect to the socket:

| Role | Constant | Description |
|------|----------|-------------|
| `angel` | `RoleAngel` | Long-lived worker process connection |
| `cli` | `RoleCLI` | Short-lived CLI request or event stream |
| `lab` | `RoleLab` | Used only in the HELLO response from Lab |

---

## HELLO handshake

Every connection must begin with a HELLO exchange before any other message. This is enforced by the `Dial` and `Listen` functions in `pkg/ipc/transport.go`.

```
Connector → Lab:  HELLO { Version: 1, Role: "angel" | "cli", Binary: "/usr/local/bin/angel" }
Lab → Connector:  HELLO { Version: 1, Role: "lab" }
```

`Binary` is informational only (used in log messages). Version mismatch closes the connection with no further messages.

`HelloPayload`:
```go
type HelloPayload struct {
    Version uint8  `msgpack:"v"`
    Role    Role   `msgpack:"role"`
    Binary  string `msgpack:"binary,omitempty"`
}
```

---

## Message kinds

```go
const (
    KindHello        MessageKind = iota + 1  // 1
    KindRegister                              // 2
    KindHeartbeat                             // 3
    KindEvent                                 // 4

    KindCmdPing                               // 5
    KindCmdPong                               // 6
    KindCmdStatus                             // 7
    KindCmdTerminate                          // 8

    KindCLIRequest                            // 9
    KindCLIResponse                           // 10
    KindEventStream                           // 11
)
```

---

## Angel → Lab messages

### KindRegister

Sent immediately after HELLO, before any other message. Lab rejects the connection if the first post-HELLO frame is not `KindRegister`.

```go
type RegisterPayload struct {
    AngelID   string `msgpack:"angel_id"`
    AngelType string `msgpack:"angel_type"` // "guardian" | "sentinel" | "memory" | "process"
    PID       int    `msgpack:"pid"`
}
```

### KindHeartbeat

Sent every `HeartbeatInterval` (default 10 s). Also sent immediately after REGISTER (initial heartbeat).

```go
type HeartbeatPayload struct {
    AngelID    string  `msgpack:"angel_id"`
    State      string  `msgpack:"state"`      // "TRAINING" | "ACTIVE"
    Uptime     int64   `msgpack:"uptime_s"`   // seconds since angel process started
    CPUPercent float64 `msgpack:"cpu_pct"`
    RSSBytes   uint64  `msgpack:"rss_bytes"`
    Goroutines int     `msgpack:"goroutines"`
    FDCount    int     `msgpack:"fd_count"`   // /proc/self/fd entry count
    AngelMeta  map[string]string `msgpack:"meta,omitempty"`
}
```

`AngelMeta` carries angel-type-specific diagnostics. Examples:
- Guardian: `{"watched_files": "5", "snapshots": "5"}`
- Sentinel: `{"phase": "ACTIVE", "connections": "23", "known_ips": "8"}`
- Memory: `{"watched_pids": "3"}`
- Process: `{"baseline_procs": "147"}`

### KindEvent

Emitted whenever the angel detects something noteworthy.

```go
type EventPayload struct {
    AngelID   string            `msgpack:"angel_id"`
    Severity  Severity          `msgpack:"severity"` // 1=INFO, 2=WARN, 3=CRIT
    Message   string            `msgpack:"message"`
    Timestamp time.Time         `msgpack:"ts"`
    Meta      map[string]string `msgpack:"meta,omitempty"`
}
```

Severity values:

| Value | Constant | Meaning |
|-------|----------|---------|
| 1 | `SeverityInfo` | Informational — state change, baseline frozen, process exited |
| 2 | `SeverityWarn` | Anomaly detected below critical threshold |
| 3 | `SeverityCritical` | High-confidence threat or automatic corrective action taken |

Event `Meta` keys by angel type:

**Guardian:**
```json
{"path": "/etc/shadow", "action": "restored", "old_hash": "4a5f...", "new_hash": "9d2c..."}
```

**Sentinel:**
```json
{"remote_ip": "45.33.32.156", "remote_port": "4444", "proto": "tcp",
 "process": "bash", "pid": "8823", "exe": "/tmp/.x", "score": "8"}
```

**Memory:**
```json
{"pid": "1234", "comm": "nginx", "rss_bytes": "536870912", "growth_pct": "210",
 "window_samples": "12"}
```

**Process:**
```json
{"pid": "8823", "ppid": "4211", "comm": "bash", "exe": "/tmp/.x",
 "uid": "0", "euid": "0", "score": "8"}
```

---

## Lab → Angel messages

### KindCmdPing

Sent by Lab to probe angel liveness (full round-trip, detects half-open sockets). The angel must reply with `KindCmdPong` carrying the same `CorrelationID`.

No payload. The `CorrelationID` field in the `Message` envelope is used to match the response.

### KindCmdPong

Reply to `KindCmdPing`. No payload. Must echo the `CorrelationID` from the ping.

### KindCmdStatus

Requests an immediate status snapshot from the angel. The angel replies with a `KindHeartbeat` frame containing current telemetry. No additional payload.

### KindCmdTerminate

Graceful shutdown signal. The angel should complete any in-progress work (e.g. finish a file restore) and then exit cleanly. No payload.

---

## CLI ↔ Lab messages

### KindCLIRequest

```go
type CLIRequest struct {
    Command CLICommand        `msgpack:"cmd"`
    Args    map[string]string `msgpack:"args,omitempty"`
}
```

Available commands:

| Command | Args | Description |
|---------|------|-------------|
| `angel.create` | `type`, `id` (opt), `paths` (opt) | Spawn a new angel |
| `angel.list` | — | List all angels |
| `angel.inspect` | `id` | Detailed view of one angel |
| `angel.terminate` | `id` | Send graceful shutdown |
| `angel.diff` | `id` | Guardian snapshot diff |
| `lab.status` | — | Daemon and angel summary |
| `event.subscribe` | — | Subscribe to live events |

### KindCLIResponse

```go
type CLIResponse struct {
    OK    bool   `msgpack:"ok"`
    Data  []byte `msgpack:"data,omitempty"`   // msgpack-encoded response body
    Error string `msgpack:"error,omitempty"`
}
```

`Data` is a nested msgpack-encoded value whose type depends on the command:

| Command | Response type |
|---------|---------------|
| `angel.list` | `[]AngelSummary` |
| `angel.inspect` | `AngelDetail` |
| `angel.create` | `map[string]string` (id, status) |
| `angel.terminate` | `map[string]string` (id, status) |
| `angel.diff` | `{angel_id, diffs: []FileDiff}` |
| `lab.status` | `LabStatus` |
| `event.subscribe` | `map[string]string` (status) |

### KindEventStream

Pushed by Lab to event-stream subscribers after each event:

```go
type EventPayload struct { ... } // same as KindEvent above
```

The subscription connection stays open until the CLI disconnects or Lab shuts down.

---

## Response body types

### AngelSummary

```go
type AngelSummary struct {
    ID           string    `msgpack:"id"`
    AngelType    string    `msgpack:"type"`
    State        string    `msgpack:"state"`
    ConnState    string    `msgpack:"conn_state"`
    PID          int       `msgpack:"pid"`
    RestartCount int       `msgpack:"restarts"`
    LastSeen     time.Time `msgpack:"last_seen"`
}
```

### AngelDetail

```go
type AngelDetail struct {
    AngelSummary
    CreatedAt    time.Time         `msgpack:"created_at"`
    ConfigJSON   string            `msgpack:"config_json"`
    Telemetry    *HeartbeatPayload `msgpack:"telemetry,omitempty"`
    RecentEvents []EventPayload    `msgpack:"recent_events,omitempty"`
}
```

### LabStatus

```go
type LabStatus struct {
    Version   string         `msgpack:"version"`
    PID       int            `msgpack:"pid"`
    Uptime    int64          `msgpack:"uptime_s"`
    StartedAt time.Time      `msgpack:"started_at"`
    Angels    []AngelSummary `msgpack:"angels"`
}
```

---

## Ping/Pong round-trip

`Conn.Ping(timeout time.Duration)` is a helper that sends `KindCmdPing` and waits for `KindCmdPong`:

```go
func (c *Conn) Ping(timeout time.Duration) error
```

Returns `nil` if the pong arrives within `timeout`. Returns an error on timeout, connection reset, or unexpected reply kind.

This is stricter than checking heartbeat send success alone: `Send()` can succeed even on a half-open socket (the OS queues the write). `Ping()` forces an acknowledgment from the remote side, confirming the full round-trip is alive.

---

## Error handling conventions

- Connection errors (read/write failures, EOF) close the connection. The caller is responsible for reconnecting or exiting.
- Protocol errors (unknown kind, decode failure) are logged and the offending frame is skipped. The connection is not closed unless the error prevents further communication.
- Version mismatch during HELLO closes the connection immediately.
- CLI errors are returned as `CLIResponse{OK: false, Error: "..."}`. The connection is not closed; the CLI can issue another request.

---

## Adding a new message kind

1. Add a constant to the `MessageKind` iota in `pkg/ipc/message.go`.
2. Define the payload type (if any).
3. Handle the new kind in `internal/lab/server.go`'s receive loop.
4. Add the corresponding response path if needed.
5. Bump `ProtocolVersion` if the change is not backward-compatible.
6. Update this document.
