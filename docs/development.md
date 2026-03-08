# Development Guide

This document covers building from source, running tests, the project structure, coding conventions, and how to add new angel types.

---

## Prerequisites

| Tool | Version | Purpose |
|------|---------|---------|
| Go | ≥ 1.22 | Build and test |
| GCC / Clang | any | CGO (required by go-sqlite3) |
| `libsqlite3-dev` | any | go-sqlite3 header |
| `make` | any | Build system |
| `git` | any | Version injection |

```bash
# Ubuntu / Debian
sudo apt-get install golang gcc libsqlite3-dev make git

# Fedora / RHEL
sudo dnf install golang gcc sqlite-devel make git

# Arch
sudo pacman -S go gcc sqlite make git
```

---

## Building

```bash
git clone https://github.com/angellab/angellab
cd angellab

# Build all three binaries into build/
make build

# Build with version info from git tags
make build VERSION=$(git describe --tags --always)

# Build a specific binary
go build -o build/labd  ./cmd/labd
go build -o build/lab   ./cmd/lab
go build -o build/angel ./cmd/angel
```

### Build output

| Binary | Entry point | Description |
|--------|-------------|-------------|
| `build/labd` | `cmd/labd/main.go` | Lab daemon |
| `build/lab` | `cmd/lab/main.go` | CLI client |
| `build/angel` | `cmd/angel/main.go` | Angel worker dispatcher |

### Version injection

The `Makefile` injects version metadata via `-ldflags`:

```makefile
LDFLAGS = -X github.com/nacreousdawn596/angellab/pkg/version.Version=$(VERSION) \
          -X github.com/nacreousdawn596/angellab/pkg/version.BuildTime=$(shell date -u +%Y-%m-%dT%H:%M:%SZ) \
          -X github.com/nacreousdawn596/angellab/pkg/version.Commit=$(shell git rev-parse --short HEAD)
```

When built outside the Makefile (e.g. `go run`), all three fields default to `"dev"` / `"unknown"`.

---

## Testing

### Unit tests

```bash
go test ./...
# or
make test
```

Test files:
- `pkg/linux/procnet_test.go` — `/proc/net/tcp` parser: IPv4, IPv6, state transitions, edge cases
- `internal/angels/sentinel/sentinel_test.go` — baseline learning, deduplicator bounds, scorer rules

### Integration tests

No root required. No actual angel processes are spawned. Tests use an in-process `miniLab` stand-in with real Unix sockets.

```bash
go test ./test/integration/... -v -count=1 -race
# or
make test-integration
```

Tests in `pipeline_test.go`:
- `TestHelloHandshake` — verify HELLO exchange completes
- `TestVersionMismatch` — verify version mismatch closes connection
- `TestRegisterAndHeartbeat` — full register + heartbeat round-trip
- `TestEventPipeline` — angel emits event, CLI receives it via event stream
- `TestCLIAngelList` — angel registers, CLI lists it
- `TestMultipleAngelsHeartbeats` — 5 concurrent angels, race detector
- `TestCorrelationID` — correlation IDs round-trip correctly

Tests in `heartbeat_failure_test.go`:
- `TestHeartbeatDetectsLabCrash` — closes Lab socket, measures detection latency
- `TestHeartbeatTransientGlitch` — one-retry absorbs a transient failure
- `TestPingPong` — Ping/Pong round-trip
- `TestPingDetectsDeadLab` — Ping fails after Lab closes socket
- `TestMultipleAngelsFailFast` — 4 angels all detect crash
- `TestHeartbeatLoopExitsOnContextCancel` — clean cancel returns nil error

### Stress / latency tests

```bash
go test ./test/stress/... -v -run TestSentinelLatency
go test ./test/stress/... -bench=. -benchmem -benchtime=5s
# or
make test-stress
```

Tests:
- `TestSentinelLatency` — asserts P99 < 5 ms for 1,000 connections (fails CI on regression)
- `TestDeduplicatorHighChurn` — verifies dedup map stays bounded at high churn
- `TestScorerDetectionRate` — verifies >90% alert rate on all-novel connections

Benchmarks:
- `BenchmarkSentinelPipeline` — end-to-end parse+score at 100/500/1000/2000 connections
- `BenchmarkParseTCPFile` — parser in isolation (floor latency)

### Running with the race detector

```bash
go test -race ./...
# or
make test-race
```

The heartbeat failure integration tests use `atomic.Int32` where shared state is mutated from goroutines to satisfy the race detector.

---

## Project structure

```
angellab/
├── cmd/
│   ├── labd/main.go        53 lines — parse flags, open config, create logger, run Daemon
│   ├── lab/
│   │   ├── main.go         522 lines — CLI dispatch, all subcommands
│   │   ├── tui.go          507 lines — live ANSI TUI dashboard
│   │   └── doctor.go       334 lines — system prerequisite checker
│   └── angel/main.go       68 lines — dispatch to guardian/sentinel/memory/process
│
├── internal/
│   ├── lab/
│   │   ├── daemon.go       260 lines — Config, Daemon, Run()
│   │   ├── supervisor.go   629 lines — AngelEntry FSM, boot recovery, restart policy
│   │   ├── server.go       535 lines — socket accept loop, angel + CLI handlers
│   │   ├── connection.go   192 lines — ConnState FSM, ConnTracker
│   │   ├── broadcaster.go  80 lines  — event fan-out to CLI subscribers
│   │   ├── reload.go       82 lines  — SIGHUP hot-reload
│   │   └── helpers.go      9 lines   — shared utilities
│   └── angels/
│       ├── guardian/
│       │   ├── guardian.go 518 lines — inotify + atomic restore
│       │   ├── config.go   38 lines  — JSON config type
│       │   └── diff.go     223 lines — DiffSnapshot for lab angel diff
│       ├── sentinel/
│       │   ├── sentinel.go  432 lines — poll loop, TRAINING→ACTIVE
│       │   ├── baseline.go  173 lines — Baseline model
│       │   ├── scorer.go    204 lines — additive rule scorer
│       │   ├── dedup.go     203 lines — Deduplicator + RateTracker
│       │   ├── inodecache.go 79 lines — inode→PID map with TTL
│       │   ├── persist.go   147 lines — baseline JSON persistence + versioning
│       │   ├── config.go    64 lines  — JSON config type
│       │   └── sentinel_test.go 192 lines
│       ├── memory/
│       │   └── memory.go   672 lines — RSS sliding window + cgroup v2
│       └── process/
│           ├── process.go  639 lines — /proc poll + 7-rule scorer
│           └── config.go   78 lines  — JSON config type
│
├── pkg/
│   ├── ipc/
│   │   ├── message.go      178 lines — all wire types and constants
│   │   ├── transport.go    298 lines — Conn, Listener, Ping(), HELLO
│   │   ├── client.go       168 lines — high-level CLI client
│   │   ├── socket_linux.go 13 lines  — Linux socket helpers
│   │   └── systemd.go      49 lines  — socket activation
│   ├── linux/
│   │   ├── inotify.go      301 lines — inotify(7) wrapper
│   │   ├── proc.go         344 lines — /proc/<pid>/status, /proc/<pid>/fd
│   │   ├── procnet.go      430 lines — /proc/net/tcp parser + ParseTCPFile
│   │   └── procnet_test.go 143 lines
│   ├── logging/
│   │   └── logger.go       318 lines — levelled logger, text+JSON, rotating file
│   ├── metrics/
│   │   └── exporter.go     338 lines — Prometheus text format (zero deps)
│   ├── registry/
│   │   └── registry.go     354 lines — SQLite: angels + events tables
│   └── version/
│       └── version.go      23 lines  — build-time metadata
│
├── test/
│   ├── integration/
│   │   ├── pipeline_test.go         634 lines — 7 IPC tests
│   │   └── heartbeat_failure_test.go 516 lines — 6 failure tests
│   └── stress/
│       └── sentinel_stress_test.go  380 lines — latency benchmark + 4 stress tests
│
├── example/
│   ├── basic_guardian/main.go   — minimal daemon with one Guardian
│   ├── ipc_client/main.go       — Go IPC client example
│   ├── sentinel_tuning/main.go  — Sentinel scoring demo (no daemon needed)
│   └── custom_angel/main.go     — PingAngel template
│
├── configs/angellab.toml    — default configuration template
├── scripts/
│   ├── install.sh           — system setup
│   └── angellab.service     — systemd unit
├── docs/
│   ├── architecture.md
│   ├── configuration.md
│   ├── ipc-protocol.md
│   ├── operations.md
│   ├── failure-semantics.md
│   └── development.md       ← this file
└── README.md
```

---

## Adding a new angel type

Follow these six steps to add a `ping` angel that monitors host reachability.

### 1. Create the package

```
internal/angels/ping/
  ping.go      — angel implementation
  config.go    — config type
```

### 2. Implement the config type (`config.go`)

```go
package ping

import (
    "encoding/json"
    "fmt"
    "io"
    "time"
)

type Config struct {
    Type      string        `json:"type"`
    ID        string        `json:"id"`
    LabSocket string        `json:"lab_socket"`
    Hosts     []string      `json:"hosts"`
    Interval  time.Duration `json:"interval"`
}

func readConfig(r io.Reader) (*Config, error) {
    data, err := io.ReadAll(r)
    if err != nil {
        return nil, fmt.Errorf("read config: %w", err)
    }
    cfg := &Config{
        Interval: 30 * time.Second,
    }
    if len(data) == 0 {
        return cfg, nil
    }
    if err := json.Unmarshal(data, cfg); err != nil {
        return nil, fmt.Errorf("decode config: %w", err)
    }
    return cfg, nil
}
```

### 3. Implement the angel (`ping.go`)

Every angel must:
- Call `ipc.Dial` to connect and `conn.Send(KindRegister)` to register
- Call `sendHeartbeat()` (returns `error`) every `HeartbeatInterval`
- Apply the one-retry policy on heartbeat failure
- Call `conn.Send(KindEvent)` on detections

See `example/custom_angel/main.go` for a complete working template.

### 4. Wire into the dispatcher (`cmd/angel/main.go`)

```go
switch cfg.Type {
case "guardian":
    runGuardian()
case "sentinel":
    runSentinel()
case "memory":
    runMemory()
case "process":
    runProcess()
case "ping":          // ← add this
    runPing()
default:
    fmt.Fprintf(os.Stderr, "angel: unknown type %q\n", cfg.Type)
    os.Exit(1)
}
```

### 5. Wire the CLI create command

In `cmd/lab/main.go`, the `cmdAngelCreate` function's type validation accepts any string, so `lab angel create ping` works without changes. If your angel has specific CLI flags, add them to the `--paths`-style parsing block.

### 6. Add a `[[angel]]` block to the config

```toml
[[angel]]
type = "ping"
id   = "A-05"
[angel.extra]
hosts    = "8.8.8.8,1.1.1.1,github.com"
interval = "30s"
```

---

## Coding conventions

### Error handling

- Return errors from all functions that can fail; do not `log.Fatal` in library code.
- Wrap errors with context: `fmt.Errorf("dial lab: %w", err)`.
- Angel `run()` functions return an error that is logged and causes `os.Exit(1)` in `main()`.
- In production code, never use `panic` for error handling — only for programmer errors (e.g. unreachable switch branches or `mustParseCIDR`).

### Concurrency

- `AngelEntry` fields are protected by `entry.mu` (a `sync.Mutex`). Always lock before reading/writing.
- `Supervisor.angels` map is protected by `s.mu` (a `sync.RWMutex`). Lock for writes; RLock for reads.
- Use `atomic.Int32` / `atomic.Int64` for single-integer counters shared between goroutines.
- Channel sends in the Broadcaster are non-blocking: use `select { case ch <- ev: default: }` to avoid blocking Lab's hot path.

### Imports

- Standard library first, then external, then internal — separated by blank lines.
- Never import `internal/` packages from `pkg/` packages (dependency direction: `cmd → internal → pkg`).
- `pkg/ipc` and `pkg/logging` have no dependencies on `internal/`; keep it that way.

### Testing

- Integration tests must not require root.
- Tests that start real OS processes are expensive — use the `miniLab` / `simAngel` pattern (in-process stand-ins with real sockets).
- Always run integration tests with `-race`.
- Latency tests that have a hard wall-clock assertion (like `TestSentinelLatency`) should be generous: use 10× the expected value as the assertion threshold to accommodate slow CI environments.

### Logging

- Use the `log.Info`, `log.Warn`, `log.Crit` methods — never `fmt.Println` in production code.
- The `[Angel Lab]` prefix on event log lines is a protocol: `log.AngelEvent(type, id, message)` emits it. Operators grep for this prefix.
- Debug logs are gated by `log_level = "debug"` and should be terse.

---

## Dependency policy

External dependencies are kept minimal:

| Package | Purpose | Alternatives considered |
|---------|---------|------------------------|
| `BurntSushi/toml` | Config parsing | encoding/json (less readable for ops), viper (too heavy) |
| `google/uuid` | Angel ID generation | Custom counter (collision risk across restarts) |
| `mattn/go-sqlite3` | Registry persistence | Pure-Go SQLite (not mature enough), PostgreSQL (external dep) |
| `vmihailsa/msgpack` | IPC wire format | encoding/json (too slow), protobuf (code gen required) |
| `golang.org/x/sys` | inotify, Linux syscalls | stdlib syscall (lower-level, less maintained) |

New dependencies require a team discussion. All pkg/ packages except `registry` must have zero external dependencies.
