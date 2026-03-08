# AngelLab

![GitHub stars](https://img.shields.io/github/stars/NacreousDawn596/angellab)
![GitHub forks](https://img.shields.io/github/forks/NacreousDawn596/angellab)
![GitHub issues](https://img.shields.io/github/issues/NacreousDawn596/angellab)
![License](https://img.shields.io/github/license/NacreousDawn596/angellab)

![Go Version](https://img.shields.io/github/go-mod/go-version/NacreousDawn596/angellab)
![Platform](https://img.shields.io/badge/platform-linux-blue)
![Nix Flake](https://img.shields.io/badge/nix-flake-5277C3)
![Repo size](https://img.shields.io/github/directory-file-count/NacreousDawn596/angellab)


**Autonomous system guardians for Linux.**

AngelLab is a modular host-security and anomaly-detection daemon. It runs a set of specialised monitoring workers — *Angels* — under a central supervisor called the *Lab*. Angels watch for filesystem tampering, unusual network connections, runaway processes, and memory leaks. When they detect something, they emit structured events and (where applicable) take automatic corrective action.

```
[Lab daemon]  ──── Unix socket ────  [Guardian A-01]  watches /etc/passwd, /etc/shadow …
                                     [Sentinel A-02]  monitors outbound connections
                                     [Memory  A-03]  tracks per-process RSS trends
                                     [Process A-04]  detects unexpected executables
```

---

## Table of contents

- [Features](#features)
- [Quick start](#quick-start)
- [Installation](#installation)
- [Configuration](#configuration)
- [CLI reference](#cli-reference)
- [Angel types](#angel-types)
- [Observability](#observability)
- [Architecture overview](#architecture-overview)
- [Failure semantics](#failure-semantics)
- [Building from source](#building-from-source)
- [Testing](#testing)
- [Examples](#examples)
- [Project layout](#project-layout)
- [NixOS](#NixOS)
- [Nix Flakes](#Nix-(flakes))
- [Threat Model & Security Guarantees](#Threat-Model-&-Security-Guarantees)
- [Project Status](#Project-Status)
---

## Features

| Feature | Detail |
|---------|--------|
| **Guardian** | inotify-based file integrity monitor. Detects any modification, restores from snapshot automatically |
| **Sentinel** | Adaptive network monitor. Learns normal outbound traffic during a training window, alerts on deviations |
| **Memory Angel** | RSS sliding-window trend detector. Catches slow leaks (% growth) and fast ones (KB/s rate) |
| **Process Angel** | Monitors process execution. Alerts on processes spawned from suspicious directories or with missing exe links |
| **Structured logging** | Text or JSON Lines output. JSON mode feeds directly into Loki, Splunk, or Datadog |
| **Prometheus metrics** | Zero-dependency exporter on `:9101/metrics`. Per-angel state, RSS, CPU, FD count, restart counters |
| **Live TUI** | `lab tui` — full-screen ANSI dashboard. Refreshes every 2 s, no external dependencies |
| **SIGHUP hot-reload** | Re-reads `angellab.toml`, spawns newly added angels, updates log level. No socket rebind |
| **Fail-fast heartbeats** | Every angel detects Lab crashes within one heartbeat interval (≤10 s) and exits cleanly |
| **Boot recovery** | On Lab restart, stale angel PIDs are verified and killed before fresh angels are spawned |
| **SQLite registry** | Persistent angel state and event history. WAL mode for concurrent reads |
| **Systemd integration** | Socket activation, `sd_notify`, `KillMode=process` for graceful shutdown |
| **`lab doctor`** | Pre-flight checker: socket, binary, inotify limits, cgroup v2, kernel version |

---

## Quick start

```bash
# 0. Initialize Go modules
go mod tidy

# 1. Build
make build

# 2. Install system prerequisites (creates user, directories, config)
sudo ./scripts/install.sh

# 3. Install binaries
sudo cp build/labd  /usr/local/bin/labd
sudo cp build/lab   /usr/local/bin/lab
sudo cp build/angel /usr/local/bin/angel

# 4. Install and start the systemd unit
sudo cp scripts/angellab.service /etc/systemd/system/angellab.service
sudo systemctl daemon-reload
sudo systemctl enable --now angellab

# 5. Verify
lab doctor
lab status
lab tui
```

---

## Installation

### System requirements

| Requirement | Minimum | Notes |
|-------------|---------|-------|
| Linux kernel | 4.18 | For cgroup v2 full support |
| Go | 1.22 | Build only |
| CGO | enabled | Required by `go-sqlite3` |
| `/proc` | mounted | Sentinel, Memory, Process angels |
| `inotify` | available | Guardian angel |
| cgroup v2 | optional | Memory angel OOM detection |

### Directories created by `install.sh`

| Path | Purpose | Owner | Mode |
|------|---------|-------|------|
| `/run/angellab/` | Unix socket | root | 0755 |
| `/var/lib/angellab/` | Registry DB, baselines | angellab | 0750 |
| `/var/lib/angellab/snapshots/` | Guardian file snapshots | angellab | 0700 |
| `/var/log/angellab/` | Rotating log files | angellab | 0750 |
| `/etc/angellab/` | Configuration | root:angellab | 0750 |

### Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LAB_SOCKET` | `/run/angellab/lab.sock` | Override socket path for the `lab` CLI |

---

## Configuration

The configuration file lives at `/etc/angellab/angellab.toml`. A full annotated example:

```toml
[lab]
# Path to the Unix domain socket.
socket        = "/run/angellab/lab.sock"

# SQLite registry database path.
registry      = "/var/lib/angellab/registry.db"

# Rotating log file path.  Leave empty to log to stdout only.
log_path      = "/var/log/angellab/lab.log"

# Minimum log verbosity: debug | info | warn | crit
log_level     = "info"

# Log output format: "text" (default) or "json" (for Loki/Splunk/Datadog)
# log_format  = "json"

# Path to the angel worker binary.
angel_binary  = "/usr/local/bin/angel"

# Prometheus metrics address.  Remove or leave empty to disable.
metrics_addr  = ":9101"

[supervisor]
# How often each angel must send a heartbeat.
heartbeat_interval = "10s"

# How long without a heartbeat before an angel is marked UNSTABLE.
# Must be > heartbeat_interval.  At least 2× recommended.
heartbeat_timeout  = "30s"

# Maximum restart attempts before an angel is CONTAINED (permanently stopped).
max_restarts = 5

# Delay between successive restart attempts (exponential: each attempt doubles).
restart_backoff = "5s"

# ─────────────────────────────────────────────
# Static angels — spawned at daemon startup.
# Additional angels can be created at runtime.
# ─────────────────────────────────────────────

[[angel]]
type = "guardian"
id   = "A-01"
paths = [
  "/etc/passwd",
  "/etc/shadow",
  "/etc/sudoers",
  "/etc/hosts",
  "/etc/ssh/sshd_config",
]
snapshot_dir = "/var/lib/angellab/snapshots"

[[angel]]
type = "sentinel"
id   = "A-02"
# Training window before alerting begins.  Longer = fewer false positives.
baseline_duration = "60s"

[[angel]]
type = "memory"
id   = "A-03"

[[angel]]
type = "process"
id   = "A-04"
```

See [`docs/configuration.md`](docs/configuration.md) for every field with types, defaults, and tuning guidance.

---

## CLI reference

All commands talk to the Lab daemon via the Unix socket. Set `LAB_SOCKET` or use the default `/run/angellab/lab.sock`.

### `lab status`

Print daemon and angel summary.

```
AngelLab v1.2.0   pid 1234   uptime 4h23m   socket /run/angellab/lab.sock

ID        TYPE        STATE       CONN        RESTARTS  LAST SEEN
A-01      guardian    ACTIVE      ACTIVE      0         2s ago
A-02      sentinel    ACTIVE      ACTIVE      0         3s ago
A-03      memory      ACTIVE      ACTIVE      0         1s ago
```

### `lab tui`

Full-screen live dashboard. Refreshes every 2 seconds. Shows angel table, live event feed, and last-polled timestamp. Press `Ctrl-C` to exit.

### `lab angel list`

List all known angels (including terminated ones still in the registry).

### `lab angel create <type> [flags]`

Spawn a new angel at runtime without restarting the daemon.

```bash
lab angel create guardian --paths /etc/crontab,/etc/cron.d
lab angel create sentinel
lab angel create memory
lab angel create process
lab angel create guardian --id A-07 --paths /var/www/html/index.php
```

Flags:

| Flag | Description |
|------|-------------|
| `--id <id>` | Explicit ID, e.g. `A-07`. Auto-generated if omitted |
| `--paths <p1,p2>` | Comma-separated watch paths (Guardian only) |

### `lab angel inspect <id>`

Detailed view of one angel: state, connection state, PID, restart count, config, recent telemetry, last 10 events.

```bash
lab angel inspect A-02
```

### `lab angel diff <id>`

Compare a Guardian angel's watched files against their stored snapshots.

```
AngelLab  Guardian A-01 — 1 file differs from snapshot

  /etc/hosts  (snapshot taken 14m23s ago)
    snapshot sha256: 4a5f8c2d1e9b7f3a…
    current  sha256: 9d2c1f4e8b3a7c6f…
    size delta:  +43 bytes
```

### `lab angel terminate <id>`

Send a graceful shutdown sequence to one angel. Lab will restart it according to its restart policy unless `max_restarts` is 0.

```bash
lab angel terminate A-03
```

### `lab events [flags]`

Stream live events to the terminal. Ctrl-C to stop.

```bash
lab events
lab events --filter "suspicious"
lab events --filter "A-02"
lab events --since 5m
```

Flags:

| Flag | Description |
|------|-------------|
| `--filter <str>` | Only show events whose message contains `str` |
| `--since <duration>` | Only show events newer than duration (e.g. `5m`, `1h`) |

### `lab doctor`

Check system prerequisites and print a `PASS` / `WARN` / `FAIL` report.

```
AngelLab system check
  Check                               Result  Detail
  ──────────────────────────────────────────────────
  Lab socket reachable                PASS    /run/angellab/lab.sock
  Protocol version match              PASS    daemon v1.2.0, client protocol v1
  Angel binary executable             PASS    /usr/local/bin/angel
  /proc/net/tcp readable              PASS    /proc/net/tcp and /proc/net/tcp6
  /proc/<pid>/fd accessible           PASS    own fd dir readable (23 open fds)
  inotify watch limit                 WARN    current 8192 < recommended 65536
  cgroup v2 (unified hierarchy)       PASS    cgroup v2 with memory controller
  State directory writable            PASS    /var/lib/angellab
  Kernel version                      PASS    Linux 5.15.0-76-generic
```

### `lab version`

Print version, commit hash, and build time.

---

## Angel types

### Guardian

Watches a list of files with `inotify(7)`. On any modification:

1. Emits a `CRIT` event: `"Guardian A-01 detected modification: /etc/shadow"`
2. Reads the modified file and computes its SHA-256
3. If the hash differs from the snapshot, restores the file from the snapshot atomically (temp file + rename)
4. Emits a second `CRIT` event confirming the restore

Snapshots are taken at startup and refreshed every 10 minutes. They are stored in `snapshot_dir` as `<basename>.snap` files. If two watched files share the same basename, the longer path is hashed to avoid collisions.

**Config fields:**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `paths` | `[]string` | required | Absolute paths to watch |
| `snapshot_dir` | `string` | `/var/lib/angellab/snapshots` | Where to store snapshots |

### Sentinel

Monitors outbound network connections by polling `/proc/net/tcp` and `/proc/net/tcp6` every 2 seconds.

**Training phase** (default 60 s): the Sentinel observes all outbound connections without alerting. It builds a `Baseline` model: which IPs and ports are seen, and what the typical concurrent connection count is. The baseline is persisted to disk (`<state_dir>/sentinel-<id>-baseline.json`) with a schema version number. On restart, if the saved baseline schema matches the current version, training is skipped and the angel goes directly to `ACTIVE`.

**Active phase**: each new connection is scored against the baseline using an additive rule table:

| Rule | Weight | Rationale |
|------|--------|-----------|
| New remote IP | +3 | Most common indicator of unexpected contact |
| New remote port | +2 | Unusual service |
| Ephemeral port (>49152) | +1 | Common in C2 channels |
| Very high port (>60000) | +1 | Stacks with above |
| Connection burst (>20 new/5 s) | +4 | Scanning or beaconing |
| Concurrent spike (>2× baseline max) | +3 | Mass connection event |
| RFC1918 source | −1 | Usually legitimate internal traffic |
| Loopback remote | −5 | Never alert (exits immediately) |

Default thresholds: **WARN ≥ 3**, **CRIT ≥ 6**.

The scorer also tracks process identity: each connection is resolved to a PID via the inode in `/proc/net/tcp`, then `/proc/<pid>/exe` and `/proc/<pid>/comm` are read. Events include the process name and path when available.

A `Deduplicator` ensures each (IP, port) pair is only scored once per observation cycle. The deduplicator is bounded at 100,000 entries; it prunes itself by half when the limit is reached.

**Config fields:**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `poll_interval` | `duration` | `2s` | How often `/proc/net/tcp` is read |
| `baseline_duration` | `duration` | `60s` | Training window |
| `state_dir` | `string` | `/var/lib/angellab` | Where to save/load the baseline |
| `warn_threshold` | `int` | `3` | Minimum score for a WARN event |
| `crit_threshold` | `int` | `6` | Minimum score for a CRIT event |
| `inode_cache_ttl` | `duration` | `10s` | How often the inode→PID map is rebuilt |

### Memory Angel

Tracks RSS (resident set size) for a set of processes using `/proc/<pid>/status`. Maintains a sliding window of samples per process and detects:

- **Percentage growth**: RSS grew by more than `growth_warn_pct` / `growth_crit_pct` within the window
- **Absolute threshold**: RSS exceeds `abs_warn_mb` / `abs_crit_mb`
- **Rate growth** (KB/s): sustained growth rate within the window exceeds `growth_rate_warn_kbps` / `growth_rate_crit_kbps`
- **cgroup v2 OOM events**: reads `memory.events` from `cgroup_path` and emits events on OOM kills

An `alert_cooldown` (default 5 m) prevents repeated alerts for the same process.

**Config fields:**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `pids` | `[]int` | `[]` | Static PID list. Empty = watch self only |
| `watch_all` | `bool` | `false` | Watch every process in `/proc` |
| `process_names` | `[]string` | `[]` | Watch processes by comm name |
| `poll_interval` | `duration` | `5s` | Sample frequency |
| `window_size` | `int` | `12` | Samples per process (12 × 5 s = 1 min trend) |
| `growth_warn_pct` | `float64` | `50` | Percentage growth WARN threshold |
| `growth_crit_pct` | `float64` | `200` | Percentage growth CRIT threshold |
| `abs_warn_mb` | `uint64` | `512` | Absolute RSS WARN threshold (MB) |
| `abs_crit_mb` | `uint64` | `2048` | Absolute RSS CRIT threshold (MB) |
| `alert_cooldown` | `duration` | `5m` | Minimum time between repeated alerts |
| `growth_rate_warn_kbps` | `float64` | `10240` | Rate WARN threshold (10 MB/s) |
| `growth_rate_crit_kbps` | `float64` | `102400` | Rate CRIT threshold (100 MB/s) |
| `cgroup_path` | `string` | `""` | cgroup v2 dir for OOM detection |

### Process Angel

Monitors running processes by polling `/proc` every 2 seconds and diffing the snapshot against the previous cycle.

**Training phase** (default 30 s): seeds the baseline from the current process table. All processes visible during training are considered "normal".

**Active phase**: new processes are scored using seven rules:

| Rule | Weight | Trigger |
|------|--------|---------|
| Suspicious exe dir | +5 | `/tmp`, `/dev/shm`, `/var/tmp`, `/run/user` |
| Missing exe symlink | +3 | Packed binary or in-memory execution |
| Unknown exe path | +3 | Path not in baseline |
| Unknown comm name | +2 | Comm not in baseline |
| Unknown parent PID | +2 | PPID not in baseline |
| Setuid / setgid | +2 | UID ≠ EUID, or GID ≠ EGID |
| Whitelist match | −10 | Guaranteed below threshold |

Reads `/proc/<pid>/comm`, `/proc/<pid>/exe`, `/proc/<pid>/cmdline`, and `/proc/<pid>/status` (PPID, UID, EUID, GID) for each new process.

Also emits `INFO` events when a previously whitelisted or baseline process exits unexpectedly (configurable with `alert_on_exit`).

**Config fields:**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `poll_interval` | `duration` | `2s` | How often `/proc` is scanned |
| `baseline_duration` | `duration` | `30s` | Training window |
| `whitelist_exes` | `[]string` | `[]` | Exe paths always allowed (exact match) |
| `whitelist_comms` | `[]string` | `[]` | Comm names always allowed (partial match) |
| `suspicious_exe_dirs` | `[]string` | see above | Directory prefixes that score +5 |
| `alert_on_exit` | `bool` | `true` | Alert when baseline processes exit |
| `state_dir` | `string` | `/var/lib/angellab` | Baseline persistence directory |

---

## Observability

### Prometheus metrics

Enable by setting `metrics_addr = ":9101"` in `[lab]`. Scrape at `http://<host>:9101/metrics`.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `angellab_angel_state` | gauge | `angel_id`, `angel_type`, `conn_state` | 1 if angel is in the labelled state, 0 otherwise |
| `angellab_angel_restarts_total` | counter | `angel_id`, `angel_type` | Cumulative restart count |
| `angellab_angel_rss_bytes` | gauge | `angel_id`, `angel_type` | Last reported RSS from heartbeat |
| `angellab_angel_cpu_percent` | gauge | `angel_id`, `angel_type` | Last reported CPU% from heartbeat |
| `angellab_angel_fd_count` | gauge | `angel_id`, `angel_type` | Open file descriptor count |
| `angellab_angel_goroutines` | gauge | `angel_id`, `angel_type` | Goroutine count in the angel process |
| `angellab_angel_uptime_seconds` | gauge | `angel_id`, `angel_type` | Seconds since angel last started |
| `angellab_events_total` | counter | `angel_id`, `angel_type`, `severity` | Cumulative events emitted |
| `angellab_lab_uptime_seconds` | gauge | — | Seconds since Lab last started |
| `angellab_lab_angel_count` | gauge | — | Current number of managed angels |

Also serves `/healthz` (returns `200 OK`).

### Logging

**Text format** (default) — human-readable aligned columns:

```
2025-03-06T14:22:01Z  INFO   [Lab]             Angel A-03 transitioned TRAINING → ACTIVE
2025-03-06T14:22:04Z  CRIT   [Guardian/A-01]   Modification detected: /etc/shadow
2025-03-06T14:22:04Z  CRIT   [Guardian/A-01]   Restored /etc/shadow from snapshot
```

**JSON Lines format** — for log aggregators. Enable with `log_format = "json"` in `[lab]`:

```json
{"ts":"2025-03-06T14:22:01.123456789Z","level":"INFO","component":"Lab","msg":"Angel A-03 transitioned TRAINING → ACTIVE"}
{"ts":"2025-03-06T14:22:04.987654321Z","level":"CRIT","component":"Guardian/A-01","msg":"Modification detected: /etc/shadow"}
```

**Log rotation** is built-in: when the log file exceeds 100 MiB, it is rotated with a timestamp suffix. Up to 5 backups are kept.

### SIGHUP hot-reload

Send `SIGHUP` to reload `angellab.toml` without restarting:

```bash
sudo kill -HUP $(pidof labd)
# or via systemctl:
sudo systemctl reload angellab
```

What reloads:
- `log_level` — takes effect immediately for the Lab daemon
- `log_format` — switching between text and JSON
- New `[[angel]]` blocks — newly added angels are spawned

What does **not** reload:
- `socket`, `registry`, `log_path`, `angel_binary` — require full restart
- Running angels — existing angels are never stopped by a reload

---

## Architecture overview

```
┌─────────────────────────────────────────────────────────────┐
│  labd process                                               │
│                                                             │
│  ┌──────────┐  ┌──────────────┐  ┌───────────────────────┐ │
│  │ Registry │  │  Supervisor  │  │  Broadcaster          │ │
│  │ (SQLite) │  │  (FSM+watch) │  │  (event fan-out)      │ │
│  └──────────┘  └──────────────┘  └───────────────────────┘ │
│                       │                        │            │
│               ┌───────┴───────┐                │            │
│               │   Server      │◄───────────────┘            │
│               │ (socket loop) │                             │
│               └───────┬───────┘                             │
│         Unix socket   │  /run/angellab/lab.sock             │
└───────────────────────┼─────────────────────────────────────┘
                        │
          ┌─────────────┼──────────────┐
          │             │              │
   [angel/A-01]  [angel/A-02]  [lab CLI]
   Guardian       Sentinel      lab status
                                lab events
```

All communication uses a single Unix domain socket. The protocol is length-prefixed msgpack with a mandatory HELLO handshake. Every connection identifies itself as `angel`, `cli`, or `lab`.

Angels are independent OS processes launched with `exec.Cmd`. They are placed in their own process group (`Setpgid=true`) so Lab's signals don't inadvertently reach them, and so each angel's death doesn't propagate to Lab.

See [`docs/architecture.md`](docs/architecture.md) for complete architecture documentation including the angel lifecycle FSM, connection state machine, boot recovery, and IPC protocol detail.

---

## Failure semantics

AngelLab is designed to fail fast and recover predictably.

### When an angel crashes

1. The angel process exits — `cmd.Wait()` in the supervisor returns.
2. The supervisor applies the restart policy: wait `restart_backoff`, then respawn.
3. After `max_restarts` failures, the angel is moved to `CONTAINED` and not restarted.

### When Lab crashes

1. Angels detect the dead control channel via failed heartbeat sends (within ≤ 10 s — one heartbeat interval).
2. Each angel applies a one-retry grace period (filters socket hiccups from genuine Lab death).
3. Angels exit with a non-zero code.
4. Systemd restarts Lab (`Restart=on-failure`).
5. Lab's boot recovery reconciles the registry: stale PIDs are verified via `/proc/<pid>/cmdline` and killed; fresh angels are spawned.

See [`docs/failure-semantics.md`](docs/failure-semantics.md) for the complete failure mode catalogue.

---

## Building from source

```bash
# Prerequisites
apt-get install -y golang gcc libsqlite3-dev

# Clone
git clone https://github.com/NacreousDawn596/angellab
cd angellab

# Initialize Go modules
go mod tidy

# Build all three binaries into build/
make build

# Build with version information
make build VERSION=$(git describe --tags --always)

# Run unit tests
make test

# Run integration tests (no root required)
make test-integration

# Run stress tests (latency benchmark)
make test-stress

# Run all tests with race detector
make test-race

# Cross-compile (CGO must target same arch for sqlite3)
GOOS=linux GOARCH=amd64 make build
```

`make build` produces:

| Binary | Purpose |
|--------|---------|
| `build/labd` | Lab daemon |
| `build/lab` | CLI client |
| `build/angel` | Angel worker dispatcher |

---

## Testing

### Unit tests

```bash
go test ./pkg/... ./internal/...
```

Tests coverage:
- `pkg/linux/procnet_test.go` — `/proc/net/tcp` parser
- `internal/angels/sentinel/sentinel_test.go` — deduplicator, baseline, scorer

### Integration tests

```bash
go test ./test/integration/... -v -count=1 -race
```

Coverage:
- `pipeline_test.go` — 7 end-to-end IPC tests including version mismatch, event pipeline, correlation IDs, multi-angel heartbeats
- `heartbeat_failure_test.go` — 6 failure-semantics tests verifying angels detect Lab crashes within the expected window

### Stress / latency tests

```bash
go test ./test/stress/... -v -run TestSentinelLatency
go test ./test/stress/... -bench=. -benchmem
```

`TestSentinelLatency` asserts that the Sentinel parse+score pipeline runs in < 5 ms P99 for 1,000 connections. This test fails CI if the hot path regresses.

---

## Examples

Runnable examples live in `./example/`. Each is a `package main` program.

| Directory | Description |
|-----------|-------------|
| `example/basic_guardian` | Minimal Lab daemon with one Guardian angel. Run with `sudo go run ./example/basic_guardian` |
| `example/ipc_client` | Dial a running daemon from Go code: fetch status, list angels, stream events |
| `example/sentinel_tuning` | Run the Sentinel scoring pipeline against synthetic data. No daemon needed. Useful for threshold tuning |
| `example/custom_angel` | Template for a new angel type. Implements a `PingAngel` that monitors host reachability |

---

## Project layout

```
angellab/
├── cmd/
│   ├── labd/           # Lab daemon entry point
│   ├── lab/            # CLI client (status, tui, angel, events, doctor)
│   └── angel/          # Angel worker dispatcher (guardian/sentinel/memory/process)
├── internal/
│   ├── lab/            # Daemon core: supervisor, server, broadcaster, reload
│   └── angels/
│       ├── guardian/   # File integrity monitor (inotify + snapshot restore)
│       ├── sentinel/   # Network anomaly detector (baseline + scorer)
│       ├── memory/     # RSS trend detector (sliding window + cgroup v2)
│       └── process/    # Process execution monitor
├── pkg/
│   ├── ipc/            # Wire protocol: message types, transport, client
│   ├── linux/          # Low-level: inotify, /proc/net/tcp, /proc/<pid>/*
│   ├── logging/        # Levelled logger with text/JSON and rotating file sink
│   ├── metrics/        # Prometheus text-format exporter (zero deps)
│   ├── registry/       # SQLite persistence: angels + events tables
│   └── version/        # Build-time version info (-ldflags injection)
├── test/
│   ├── integration/    # End-to-end IPC and failure-semantics tests
│   └── stress/         # Sentinel pipeline latency benchmark
├── example/            # Runnable example programs
├── configs/
│   └── angellab.toml   # Default configuration template
├── scripts/
│   ├── install.sh      # System setup (user, dirs, permissions)
│   └── angellab.service # systemd unit file
└── docs/               # Detailed documentation
    ├── architecture.md
    ├── configuration.md
    ├── angels.md
    ├── ipc-protocol.md
    ├── operations.md
    ├── development.md
    └── failure-semantics.md
```



## NixOS

AngelLab provides a **Nix flake** with a NixOS module, allowing fully declarative installation without running the `install.sh` script.

### Using the flake

Add AngelLab to your system flake inputs:

```nix
{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    angellab.url = "github:NacreousDawn596/angellab";
  };

  outputs = { self, nixpkgs, angellab, ... }:
  {
    nixosConfigurations.myhost = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";

      modules = [
        angellab.nixosModules.default
        {
          services.angellab.enable = true;
        }
      ];
    };
  };
}
```

Enable the module in your NixOS configuration:

```nix
{
  imports = [
    inputs.angellab.nixosModules.default
  ];

  services.angellab.enable = true;
}
```

Apply the configuration:

```bash
sudo nixos-rebuild switch
```

The module automatically:

* builds the `labd`, `lab`, and `angel` binaries
* creates the `angellab` system user
* creates required directories:

| Path                          | Purpose                         |
| ----------------------------- | ------------------------------- |
| `/run/angellab`               | Unix socket                     |
| `/var/lib/angellab`           | registry database and baselines |
| `/var/lib/angellab/snapshots` | Guardian snapshots              |
| `/var/log/angellab`           | log files                       |
| `/etc/angellab`               | configuration                   |

* installs the default configuration at:

```
/etc/angellab/angellab.toml
```

* installs and starts the `angellab` systemd service.

### Custom configuration

You can provide your own configuration file:

```nix
{
  services.angellab = {
    enable = true;
    configFile = ./angellab.toml;
  };
}
```

After rebuilding, the daemon will start automatically.

Check status:

```bash
lab doctor
lab status
lab tui
```

---


## Nix (flakes)

AngelLab can be run directly using Nix without installing it.

Run the CLI:

```bash
nix run github:NacreousDawn596/angellab
```

Run specific binaries:

```bash
nix run github:NacreousDawn596/angellab#lab
nix run github:NacreousDawn596/angellab#labd
nix run github:NacreousDawn596/angellab#angel
```

Enter the development shell:

```bash
nix develop
make build
make test
```

---

## Threat Model & Security Guarantees

AngelLab is designed to detect **host-level anomalies and post-exploitation activity** on Linux systems. It focuses on behavioural monitoring rather than signature-based malware detection.

### Security goals

AngelLab aims to detect and surface events such as:

* Modification of critical system files (`/etc/passwd`, `/etc/shadow`, `/etc/sudoers`)
* Unexpected outbound network connections
* Execution of binaries from suspicious locations
* Long-running processes exhibiting memory leak patterns
* Rapid bursts of connections or process creation
* Changes to system configuration that deviate from a known-good baseline

The system prioritizes:

* **Low false positives** through adaptive baselines
* **Clear event attribution** (process, path, PID, and context)
* **Fast detection latency** (typically within seconds)

AngelLab does **not attempt to prevent attacks proactively**, but instead focuses on **early detection and containment signals**.

### Attacker assumptions

The threat model assumes an attacker may:

* Execute arbitrary processes under a non-root user
* Spawn processes from writable directories (`/tmp`, `/dev/shm`, etc.)
* Establish outbound connections to remote hosts
* Modify monitored files
* Trigger abnormal resource usage patterns

AngelLab is designed to surface these behaviours quickly through the appropriate angel.

### Out-of-scope threats

AngelLab does **not attempt to mitigate** the following classes of attacks:

| Threat                                    | Reason                                                                      |
| ----------------------------------------- | --------------------------------------------------------------------------- |
| Kernel-level rootkits                     | Kernel-space compromise bypasses `/proc`, inotify, and userspace monitoring |
| Attacks executed entirely in kernel space | Outside the visibility of userspace processes                               |
| Physical access attacks                   | Disk tampering or offline modification of system state                      |
| Firmware / bootloader compromise          | Requires secure boot and hardware trust chains                              |
| Encrypted covert channels                 | Sentinel observes connection metadata, not payload contents                 |

AngelLab should therefore be considered **a detection and observability tool**, not a complete host intrusion prevention system.

### Privilege model

AngelLab runs as a **dedicated system user (`angellab`)** and does not require root privileges for most operations.

However, certain angels may require elevated capabilities depending on configuration:

| Angel    | Capability requirement         |
| -------- | ------------------------------ |
| Guardian | Read access to monitored files |
| Sentinel | Read access to `/proc/net/*`   |
| Memory   | Read access to `/proc/<pid>`   |
| Process  | Read access to `/proc`         |

In most deployments the Lab daemon runs under systemd with restricted permissions and sandboxing enabled.

### Tamper resistance

AngelLab provides **limited tamper resistance**:

* Angels are supervised by the Lab daemon and restarted on crash
* Heartbeat monitoring ensures angels detect Lab failure within one interval
* Boot recovery verifies and cleans up stale angel processes
* Snapshots allow Guardian to restore critical files automatically

However, an attacker with **root privileges** can disable or modify AngelLab. In such environments, AngelLab should be combined with:

* immutable infrastructure
* centralized log aggregation
* external monitoring systems

### Recommended deployment

For best security outcomes:

* Forward JSON logs to a **remote log collector**
* Scrape metrics into **Prometheus or another monitoring system**
* Monitor the **health and uptime of the Lab daemon**
* Deploy alongside complementary tools such as network monitoring, audit frameworks, or kernel-level security systems

AngelLab is intended to act as a **lightweight host guardian**, providing fast insight into abnormal system behaviour.




## Project Status

**AngelLab is currently under active development and should be considered unstable.**

The architecture, configuration format, and internal APIs may change between releases.
Features may be incomplete, experimental, or subject to breaking changes.

This project is **not yet recommended for production environments**.

If you choose to run AngelLab:

* expect configuration and behaviour to change
* expect occasional bugs or crashes
* avoid deploying it on critical systems without proper backups

Feedback, bug reports, and contributions are very welcome while the project evolves.


## License

See `LICENSE`.
