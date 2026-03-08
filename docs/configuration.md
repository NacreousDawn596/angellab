# Configuration Reference

AngelLab is configured via a single TOML file, conventionally at `/etc/angellab/angellab.toml`. The file is divided into four sections: `[lab]`, `[supervisor]`, and any number of `[[angel]]` blocks.

If the file does not exist, all defaults are used and no static angels are spawned.

---

## `[lab]` — daemon settings

```toml
[lab]
socket        = "/run/angellab/lab.sock"
registry      = "/var/lib/angellab/registry.db"
log_path      = "/var/log/angellab/lab.log"
log_level     = "info"
log_format    = "text"
angel_binary  = "/usr/local/bin/angel"
metrics_addr  = ":9101"
```

### `socket`

**Type:** `string`  
**Default:** `/run/angellab/lab.sock`

Path to the Unix domain socket Lab binds. The directory must exist and be writable by the `angellab` user. The socket file is created with mode `0660`. Angel processes and CLI users must be in the `angellab` group (or root) to connect.

The `LAB_SOCKET` environment variable overrides this for the `lab` CLI only — the daemon always uses the config value.

### `registry`

**Type:** `string`  
**Default:** `/var/lib/angellab/registry.db`

Path to the SQLite database file. The directory must be writable by the `angellab` user. Created automatically if it does not exist.

The database uses WAL mode for safe concurrent reads while Lab writes. Do not place this on NFS or other network filesystems.

### `log_path`

**Type:** `string`  
**Default:** `/var/log/angellab/lab.log`

Destination for the rotating log file. Leave empty (`log_path = ""`) to write to stdout only (suitable when running under systemd with `StandardOutput=journal`).

The file is rotated when it exceeds 100 MiB. Up to 5 backups are kept with timestamp suffixes (e.g. `lab.log.2025-03-06T14-00-00Z`).

### `log_level`

**Type:** `string`  
**Default:** `"info"`  
**Values:** `debug`, `info`, `warn`, `crit`

Minimum log verbosity. `debug` produces a high volume of output; suitable only for development. `crit` suppresses all informational messages.

Can be changed at runtime via `SIGHUP` without restarting.

### `log_format`

**Type:** `string`  
**Default:** `"text"`  
**Values:** `text`, `json`

Log output format.

**`text`** (default): human-readable aligned columns.
```
2025-03-06T14:22:01Z  INFO   [Lab]            Angel A-02 transitioned TRAINING → ACTIVE
2025-03-06T14:22:04Z  CRIT   [Guardian/A-01]  Modification detected: /etc/shadow
```

**`json`**: one JSON object per line (JSON Lines / NDJSON). Compatible with Loki, Splunk, Datadog, Elastic, and any aggregator that speaks JSON Lines.
```json
{"ts":"2025-03-06T14:22:01.123Z","level":"INFO","component":"Lab","msg":"Angel A-02 transitioned TRAINING → ACTIVE"}
```

Can be changed at runtime via `SIGHUP`.

### `angel_binary`

**Type:** `string`  
**Default:** `/usr/local/bin/angel`

Absolute path to the `angel` worker binary. Lab uses `exec.Cmd` with this path to spawn all angel types. The binary must be executable by the `angellab` user.

Cannot be changed at runtime — requires a daemon restart.

### `metrics_addr`

**Type:** `string`  
**Default:** `""` (disabled)

TCP address for the Prometheus metrics HTTP endpoint. Format: `"HOST:PORT"` or just `":PORT"` to listen on all interfaces.

Examples:
```toml
metrics_addr = ":9101"           # all interfaces, port 9101
metrics_addr = "127.0.0.1:9101" # localhost only
```

Leave empty or omit to disable the metrics endpoint entirely. When enabled, also serves `/healthz` (returns `200 OK`).

---

## `[supervisor]` — lifecycle settings

```toml
[supervisor]
heartbeat_interval = "10s"
heartbeat_timeout  = "30s"
max_restarts       = 5
restart_backoff    = "5s"
```

Duration values accept Go duration strings: `"5s"`, `"1m30s"`, `"2h"`, etc.

### `heartbeat_interval`

**Type:** `duration`  
**Default:** `"10s"`

How often each angel sends a heartbeat. This is enforced on the angel side — Lab does not send a tick; angels have their own internal ticker.

Shorter intervals mean faster failure detection but more IPC traffic. For most deployments, `10s` is appropriate. Go below `2s` only if you have a specific operational need.

### `heartbeat_timeout`

**Type:** `duration`  
**Default:** `"30s"`  
**Must be:** > `heartbeat_interval`

How long Lab waits after the last heartbeat before declaring an angel `UNSTABLE`. This is the **Lab-side** safety net; the primary failure detection path is angel-side (heartbeat send failure within one interval).

Recommended: at least `3 × heartbeat_interval`. The default `30s` gives three missed heartbeats before action.

### `max_restarts`

**Type:** `int`  
**Default:** `5`

Maximum number of consecutive restart attempts before an angel is moved to `CONTAINED`. A `CONTAINED` angel is permanently stopped until manually restarted with `lab angel create`.

Set to `0` to disable automatic restarts entirely (angels are never restarted on failure).

### `restart_backoff`

**Type:** `duration`  
**Default:** `"5s"`

Base delay between restart attempts. The delay doubles on each subsequent attempt up to a maximum of `restart_backoff × 16` (80 s with the default).

| Attempt | Delay (default) |
|---------|-----------------|
| 1 | 5 s |
| 2 | 10 s |
| 3 | 20 s |
| 4 | 40 s |
| 5 | 80 s (capped) |

---

## `[[angel]]` — static angel declarations

Static angels are spawned when the daemon starts. Zero or more `[[angel]]` blocks can be declared. Additional angels can be created at runtime with `lab angel create`.

Every `[[angel]]` block supports these common fields:

### `type` (required)

**Type:** `string`  
**Values:** `guardian`, `sentinel`, `memory`, `process`

The angel type to spawn.

### `id`

**Type:** `string`  
**Default:** auto-generated (e.g. `A-05`)

The angel's unique identifier. Must be unique across all currently managed angels. If an angel with this ID already exists in the registry (from a previous run), Lab will adopt it rather than creating a duplicate.

Convention: `A-01`, `A-02`, etc. Any string is valid.

### `paths` (Guardian only)

**Type:** `[]string`  
**Default:** `[]`

Absolute file paths for the Guardian to watch. Each path must exist at startup (Guardian exits if a path is not found). Directories are not supported — Guardian watches individual files only.

```toml
[[angel]]
type = "guardian"
id   = "A-01"
paths = [
  "/etc/passwd",
  "/etc/shadow",
  "/etc/sudoers",
  "/etc/hosts",
  "/etc/crontab",
  "/etc/ssh/sshd_config",
  "/etc/ssl/certs/ca-certificates.crt",
]
```

### `snapshot_dir` (Guardian only)

**Type:** `string`  
**Default:** `/var/lib/angellab/snapshots`

Directory where Guardian stores file snapshots. Must be writable by the `angellab` user. The directory is created automatically if it does not exist.

Snapshots are named `<basename>.snap` or `<sha256-of-path[:16]>.snap` when two watched files share the same basename.

### `baseline_duration` (Sentinel only)

**Type:** `duration`  
**Default:** `"60s"`

How long the Sentinel observes traffic before activating alerts. Longer training windows learn more traffic patterns and produce fewer false positives on first use.

Recommended values:
- Development / test: `"30s"`
- Production server: `"300s"` (5 min)
- First deployment on a busy machine: `"600s"` (10 min)

### `extra` — angel-specific overrides

**Type:** `map[string]string`

An escape hatch for angel config fields not covered by the above. Passed through to the angel process as part of the JSON config.

```toml
[[angel]]
type = "sentinel"
id   = "A-02"
[angel.extra]
poll_interval = "1s"
crit_threshold = "8"
```

---

## Complete annotated example

```toml
# /etc/angellab/angellab.toml
# Full example with all fields documented.

[lab]
# Unix socket path. Angels and CLI dial this socket.
socket        = "/run/angellab/lab.sock"

# SQLite database for persistent state.
registry      = "/var/lib/angellab/registry.db"

# Rotating log file. Remove to log stdout only (journald captures that).
log_path      = "/var/log/angellab/lab.log"

# debug | info | warn | crit  (changeable via SIGHUP)
log_level     = "info"

# text | json  (changeable via SIGHUP)
# Set to "json" if you are shipping logs to Loki, Splunk, or Datadog.
log_format    = "text"

# Angel worker binary. Must be executable by angellab user.
angel_binary  = "/usr/local/bin/angel"

# Prometheus endpoint. Comment out to disable.
metrics_addr  = ":9101"

[supervisor]
# Angel heartbeat frequency. Angels send at this interval.
heartbeat_interval = "10s"

# Declare angel UNSTABLE after this long without a heartbeat.
heartbeat_timeout  = "30s"

# Restart attempts before CONTAINED (permanent stop).
max_restarts = 5

# Base delay between restarts (doubles each attempt, capped at 16×).
restart_backoff = "5s"

# ─────────────────────────────────────────
# File integrity: watch critical system files
# ─────────────────────────────────────────
[[angel]]
type = "guardian"
id   = "A-01"
paths = [
  "/etc/passwd",
  "/etc/shadow",
  "/etc/group",
  "/etc/sudoers",
  "/etc/hosts",
  "/etc/hostname",
  "/etc/resolv.conf",
  "/etc/ssh/sshd_config",
  "/etc/crontab",
]
snapshot_dir = "/var/lib/angellab/snapshots"

# ─────────────────────────────────────────
# Network anomaly detection
# ─────────────────────────────────────────
[[angel]]
type = "sentinel"
id   = "A-02"
# Allow 5 minutes of learning on a production machine.
baseline_duration = "300s"

# ─────────────────────────────────────────
# Memory leak and OOM detection
# ─────────────────────────────────────────
[[angel]]
type = "memory"
id   = "A-03"

# ─────────────────────────────────────────
# Process execution monitoring
# ─────────────────────────────────────────
[[angel]]
type = "process"
id   = "A-04"
```

---

## Runtime changes via SIGHUP

```bash
# Send SIGHUP to reload config
sudo kill -HUP $(pidof labd)
# or
sudo systemctl reload angellab
```

Fields that reload:

| Field | Notes |
|-------|-------|
| `log_level` | Takes effect immediately for the daemon |
| `log_format` | Switching between text and JSON |
| New `[[angel]]` blocks | Newly added angels are spawned immediately |

Fields that require a full restart:

| Field | Why |
|-------|-----|
| `socket` | Socket is bound at startup and cannot be rebound without a restart |
| `registry` | Database handle is opened once and held open |
| `log_path` | File handle is opened at startup |
| `angel_binary` | Used only when spawning; existing angels keep running |
| `metrics_addr` | HTTP server is bound at startup |

---

## Validation

Run `lab doctor` to check that all prerequisites are met before (or after) changing the config:

```bash
lab doctor
```

This checks: socket reachability, angel binary, `/proc` readability, inotify limits, cgroup v2 availability, state dir writability, and kernel version.
