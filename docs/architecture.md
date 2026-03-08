# Architecture

This document describes every major subsystem in AngelLab: how they are structured, how they communicate, and why specific design decisions were made.

---

## High-level overview

AngelLab is structured as a **supervisor/worker** system:

- **Lab** (`labd`) is the single supervisor process. It owns the Unix socket, the SQLite registry, and all lifecycle decisions.
- **Angels** are stateless worker processes. Each angel type knows only how to monitor one thing. Angels are launched by Lab, monitored via heartbeats, and restarted on failure.
- **`lab`** is the CLI client. It dials the same Unix socket that angels use, identifies itself with a different role, and issues structured commands.

```
                        /run/angellab/lab.sock
                               │
          ┌────────────────────┼────────────────────────────┐
          │    labd                                         │
          │                                                 │
          │  ┌─────────────┐  ┌────────────────────────┐   │
          │  │  Supervisor  │  │  Server                │   │
          │  │  ┌─────────┐ │  │  ┌──────────────────┐  │   │
          │  │  │  FSM    │ │  │  │  angel handler   │  │   │
          │  │  │  +watch │ │  │  │  CLI handler     │  │   │
          │  │  └─────────┘ │  │  └──────────────────┘  │   │
          │  └─────────────┘  └────────────────────────┘   │
          │         │                     │                 │
          │  ┌──────┴───────┐  ┌──────────┴──────────┐     │
          │  │  Registry    │  │  Broadcaster         │     │
          │  │  (SQLite)    │  │  (event fan-out)     │     │
          │  └──────────────┘  └─────────────────────┘     │
          └─────────────────────────────────────────────────┘
                    ↑                    ↑
               angel processes       lab CLI
```

---

## Processes and binaries

Three binaries are produced by `make build`:

### `labd` — the Lab daemon

Single process. Runs as the `angellab` system user (except during tests). Owns:

- The Unix socket at `/run/angellab/lab.sock`
- The SQLite registry at `/var/lib/angellab/registry.db`
- The rotating log file at `/var/log/angellab/lab.log`
- The Prometheus HTTP server at `:9101`
- The set of angel `exec.Cmd` processes

### `angel` — the angel dispatcher

A single binary that dispatches to the correct angel type based on a `type` field in the config passed via stdin. This avoids installing four separate binaries.

```
/usr/local/bin/angel
  stdin: {"type":"guardian","id":"A-01","paths":[...]}
```

### `lab` — the CLI client

Talks to `labd` exclusively via the Unix socket. Never contacts angel processes directly. Has no persistent state of its own.

---

## The Unix socket and IPC protocol

All communication uses a **single Unix domain socket** at `/run/angellab/lab.sock`.

Three roles connect to it:
- `angel` — long-lived persistent connection, registers and sends heartbeats
- `cli` — short-lived request/response, or held open for event streaming
- `lab` — used only internally during the HELLO handshake

### Frame format

Every message is a length-prefixed msgpack frame:

```
┌────────────────────────────────────────┐
│  4 bytes: uint32 payload length (BE)   │
│  N bytes: msgpack-encoded Message      │
└────────────────────────────────────────┘
```

Maximum frame size: 16 MiB (configurable via `MaxFrameSize`).

### HELLO handshake

Every new connection must exchange a HELLO frame before any other message. Version mismatch closes the connection immediately.

```
Connector → Lab:  KindHello { Version: 1, Role: "angel" | "cli" }
Lab → Connector:  KindHello { Version: 1, Role: "lab" }
```

### Angel connection lifecycle

After HELLO, an angel sends `KindRegister` with its ID, type, and PID. Lab then loops receiving `KindHeartbeat` and `KindEvent` frames. Lab sets a read deadline of `2 × HeartbeatTimeout` — if no frame arrives within that window, the connection is declared dead.

```
angel → lab:  KindRegister { angel_id, angel_type, pid }
angel → lab:  KindHeartbeat { angel_id, state, uptime, cpu_pct, rss_bytes, goroutines, fd_count, meta }
angel → lab:  KindEvent     { angel_id, severity, message, timestamp, meta }
lab   → angel: KindCmdPing  { }   ← liveness probe (angel replies KindCmdPong)
lab   → angel: KindCmdTerminate   ← graceful shutdown
```

### CLI connection lifecycle

CLI connections send a `KindCLIRequest` and receive a `KindCLIResponse`. For event streaming, the CLI sends `event.subscribe` and Lab pushes `KindEventStream` frames until the connection closes.

```
cli → lab:  KindCLIRequest  { cmd: "angel.list" | "lab.status" | ... }
lab → cli:  KindCLIResponse { ok: true, data: <msgpack bytes> }

cli → lab:  KindCLIRequest  { cmd: "event.subscribe" }
lab → cli:  KindCLIResponse { ok: true }          ← subscription confirmed
lab → cli:  KindEventStream { ... }               ← pushed on each event
lab → cli:  KindEventStream { ... }
```

---

## Supervisor and angel lifecycle

### Angel lifecycle FSM

Every angel has an independent lifecycle state maintained by the Supervisor:

```
 CREATED ──── process spawned ──── TRAINING
                                       │
                                  first heartbeat
                                       │
                                     ACTIVE
                                       │
                           ┌───────────┴───────────┐
                     heartbeat missed          terminate cmd
                           │                       │
                        UNSTABLE               TERMINATED
                           │
                  recovery window expired
                           │
                  restart policy applied
                    ┌──────┴──────┐
                  within        exhausted
                  max_restarts  max_restarts
                    │                │
                 CREATED          CONTAINED
                (restart)        (permanent stop)
```

`TRAINING` → `ACTIVE` transition is triggered by the angel itself: the first heartbeat after `BaselineDuration` carries a state field of `"ACTIVE"`.

`ACTIVE` → `UNSTABLE` is triggered by the Supervisor's heartbeat watcher: if `time.Since(entry.LastHeartbeat) > HeartbeatTimeout`, the angel is marked UNSTABLE.

### Angel connection FSM

Separate from the lifecycle state, each angel has a connection state:

```
DISCONNECTED ─── spawn ──► CONNECTING
                                │
                           KindRegister received
                                │
                           REGISTERED ──── first heartbeat ──► ACTIVE
                                │                                  │
                           conn drop                          conn drop
                                │                                  │
                                └──────────────┬───────────────────┘
                                               ▼
                                              LOST
                                               │
                               ┌──── within recovery window?
                               │                   │
                             yes                   no
                               │                   │
                               ▼                   ▼
                          RECOVERING          restart policy
                               │
                        KindRegister received
                               │
                          REGISTERED → ACTIVE
```

The 10-second recovery window allows for transient reconnects. After the window expires, the Supervisor SIGTERMs the process (it may still be alive but disconnected) and applies the restart policy.

### Boot recovery

On Lab startup, the Supervisor reads all non-TERMINATED angels from the registry and checks whether their recorded PIDs are still alive:

1. Read `PID` from the registry row.
2. Read `/proc/<pid>/cmdline` and verify it contains `"angel"`.
3. If the process is running and is an angel: send `SIGTERM`, wait for `cmd.Wait()` to return.
4. Re-spawn the angel as a fresh process.

This ensures no ghost angel processes remain from a previous Lab run.

### Restart backoff

Restarts are delayed by `restart_backoff` (default 5 s). Each successive restart doubles the delay up to a maximum of `restart_backoff × 2^4` (80 s with the default). After `max_restarts` (default 5) consecutive failures, the angel is moved to `CONTAINED` and not restarted until the operator intervenes.

---

## Supervisor → angel communication (exec)

Angels are spawned via `exec.Cmd` with:

- `Setpgid: true` — angel is in its own process group; Lab signals don't reach it
- `Stdin`: closed pipe carrying the JSON config blob
- `Stdout` / `Stderr`: piped to the Lab logger
- Environment: inherits Lab's environment

The JSON config is written to stdin before `cmd.Start()` and the pipe is closed. Angels read from stdin at startup and then close it; they never read from stdin again.

---

## Registry (SQLite)

Two tables with WAL mode enabled:

```sql
CREATE TABLE angels (
    id            TEXT PRIMARY KEY,
    angel_type    TEXT    NOT NULL,
    state         TEXT    NOT NULL DEFAULT 'CREATED',
    pid           INTEGER,
    created_at    DATETIME NOT NULL,
    updated_at    DATETIME NOT NULL,
    restart_count INTEGER  NOT NULL DEFAULT 0,
    last_seen     DATETIME,
    config_json   TEXT     -- JSON serialisation of AngelConfig
);

CREATE TABLE events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    angel_id    TEXT     NOT NULL,
    severity    TEXT     NOT NULL,
    message     TEXT     NOT NULL,
    meta_json   TEXT,
    occurred_at DATETIME NOT NULL,
    FOREIGN KEY (angel_id) REFERENCES angels(id)
);
```

All registry mutations go through the single `*sql.DB` connection. Read queries (from `lab angel list`, `lab angel inspect`, CLI handlers) run concurrently thanks to WAL mode.

---

## Broadcaster (event fan-out)

The `Broadcaster` holds a set of subscriber channels — one per active `event.subscribe` CLI connection. When an event arrives from an angel:

1. The Supervisor calls `bcast.Publish(ev)`.
2. The Broadcaster iterates all subscribers and sends to their channels non-blockingly.
3. If a subscriber's channel is full (buffer size 32), the message is dropped for that subscriber only. The subscriber is not removed — it will catch the next event.
4. When a CLI connection closes, it unregisters its channel from the Broadcaster.

This design ensures that a slow CLI client never blocks the angel-handling path.

---

## Prometheus metrics (zero deps)

The `pkg/metrics` package hand-writes Prometheus text format (version 0.0.4) without importing `prometheus/client_golang`. This keeps the binary lean.

Metrics are updated via a `metricsHook` struct that the Supervisor calls after each heartbeat. The hook avoids a circular import between `internal/lab` and `pkg/metrics`.

The metrics server runs on a separate goroutine and HTTP listener from the socket server. It reads from an in-memory `MetricsSnapshot` (protected by a sync.RWMutex) and never touches the SQLite registry.

---

## Logging subsystem

`pkg/logging` provides a levelled, multi-writer logger with:

- **Two output formats**: `text` (human-readable aligned columns) and `json` (one JSON object per line, RFC3339Nano timestamp)
- **Rotating file sink**: rotates when the file exceeds 100 MiB, keeps up to 5 backups
- **SetLevel()**: allows hot-reload to change verbosity without restarting
- **SetFormat()**: switches between text and JSON on SIGHUP

Log levels: `DEBUG` < `INFO` < `WARN` < `CRIT`.

In production, the Lab daemon writes to both `os.Stdout` (captured by journald) and the rotating file. Angels write only to stdout.

---

## Sentinel internals

The Sentinel is the most algorithmically complex angel. Its major components:

### Baseline

A `Baseline` holds the learned "normal" state:
- `knownIPs`: `map[string]bool` (IPv4 string → true)
- `knownPorts`: `map[uint16]bool`
- `maxConcurrent`: peak concurrent established connection count seen during training
- `samples`: list of connection observations for persistence

`Observe(conns)` feeds one poll cycle's connections into the model. `Freeze()` locks the baseline for scoring. `Freeze` must be called before `Score`.

Baseline persistence uses versioned JSON (`baselineSchemaVersion = 1`). A version mismatch causes retraining rather than a load error.

### Deduplicator

A `Deduplicator` tracks recently-seen `(IP, port)` pairs so each connection is only scored once per observation cycle (not once per poll). This prevents the same long-lived connection from triggering repeated alerts.

The dedup map is bounded at 100,000 entries. When the limit is reached, `pruneHalf()` removes the oldest 50,000 entries. This prevents unbounded memory growth on machines with many long-lived connections.

### RateTracker

A `RateTracker` counts new connections in a sliding 5-second window. This feeds the burst rule (+4 points when >20 new connections appear in 5 seconds).

### Inode cache

The `InodeCache` maps `(socket inode → PID → exe path)` for process attribution. Rebuilding requires iterating `/proc/*/fd/*` which is expensive; the cache is rebuilt at most every `InodeCacheTTL` (default 10 s) using a time-based check with double-checked locking.

### Scorer

The `Scorer` evaluates one `OutboundConn` against the frozen `Baseline`. Scoring is additive: each rule contributes a named point value, and the full list of reasons is included in emitted events so operators can understand exactly why an alert fired.

---

## Guardian internals

Guardian uses `inotify(7)` via the `pkg/linux/inotify` wrapper. Watch mask: `IN_MODIFY | IN_CREATE | IN_DELETE | IN_MOVE`.

On a filesystem event:
1. The event is processed in the main select loop.
2. If it's a `IN_DELETE_SELF` (atomic write by editors like vim): the inotify watch is re-added for the new inode.
3. Otherwise: hash the current file, compare to snapshot. If different: write the snapshot content to a temp file in the same directory, then `os.Rename` to the target path (atomic on Linux).

Snapshots are refreshed every 10 minutes regardless of modifications. This keeps them current with legitimate operator changes.

---

## Memory Angel internals

For each monitored process, the Memory Angel maintains a `rssWindow`: a circular buffer of `(timestamp, rss_bytes)` samples. On each poll:

1. Read `/proc/<pid>/status` for the current RSS.
2. Append to the window.
3. Compute percentage growth: `(current − oldest) / oldest × 100`.
4. Compute rate: `(newest_rss − oldest_rss) / elapsed_seconds / 1024` (KB/s).
5. Compare against configured thresholds.
6. For cgroup monitoring: read `/sys/fs/cgroup/<path>/memory.events` and check the `oom_kill` counter.

An `alertCooldown` map prevents the same process from generating repeated alerts within `AlertCooldown` (default 5 min).

---

## Process Angel internals

Process Angel maintains a `snapshot` map of `PID → processInfo` from the previous poll cycle. On each new cycle:

1. Scan `/proc/*/` for all numeric directories.
2. For each PID: read `comm`, `exe`, `cmdline`, and `status` (PPID, UID, EUID, GID).
3. Diff the new snapshot against the previous one.
4. New PIDs → score → emit if above threshold.
5. Missing baseline PIDs → emit INFO if `alert_on_exit`.

The training baseline is persisted to `<state_dir>/process-<id>-baseline.json` at the end of the training window.

---

## Design decisions and tradeoffs

**Why a single socket for both angels and CLI?**
Simplifies deployment (one port, one permission surface), and the HELLO handshake with role field makes it trivial to route frames correctly. The alternative (separate sockets) would require angels to know two paths.

**Why msgpack instead of JSON or protobuf?**
Msgpack is compact, has a mature Go library (`vmihailsa/msgpack`), requires no code generation, and is significantly faster than JSON for the heartbeat-heavy workload. Protobuf was considered but adds a build step that complicates the zero-CGO philosophy of the non-registry packages.

**Why SQLite instead of a flat file or etcd?**
SQLite gives us ACID transactions, concurrent reads via WAL, and SQL queries for the CLI without any network dependency. The registry is local to the machine being monitored — distributed storage would be over-engineering.

**Why does angel-side baseline persistence use JSON instead of msgpack?**
Baseline files are operator-readable and occasionally hand-edited for whitelisting. JSON is the right format for human-visible state files even if it is slightly larger than msgpack.

**Why no angel-side reconnect loop?**
Clear responsibility split: Lab owns lifecycle and supervision; angels are workers that either function or terminate. Angel-side reconnect would introduce a second recovery mechanism with its own failure modes. Since baselines are persisted to disk, the cost of a restart (retraining) is now small for all angel types.
