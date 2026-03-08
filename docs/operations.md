# Operations Guide

This document covers day-to-day operations: deployment, monitoring, log management, Prometheus integration, hot-reload, and common failure scenarios.

---

## Deployment

### Recommended layout

```
/usr/local/bin/labd          daemon binary
/usr/local/bin/lab           CLI binary
/usr/local/bin/angel         angel worker binary

/etc/angellab/angellab.toml  configuration
/etc/systemd/system/angellab.service

/run/angellab/lab.sock       Unix socket (tmpfs, recreated on boot)
/var/lib/angellab/           state directory
  registry.db                SQLite registry
  snapshots/                 Guardian file snapshots
  sentinel-A-02-baseline.json  Sentinel trained baseline
  process-A-04-baseline.json   Process Angel trained baseline
/var/log/angellab/           log directory
  lab.log                    current log
  lab.log.2025-03-06T14-00-00Z  rotated backups (up to 5)
```

### Systemd unit

The included `scripts/angellab.service` configures:

- `Type=notify` — Lab sends `sd_notify(READY=1)` once the socket is bound
- `KillMode=process` — systemd only kills `labd`; angels are killed by Lab during graceful shutdown
- `Restart=on-failure` — Lab is restarted automatically after a crash
- `ProtectSystem=strict` + `ReadWritePaths=...` — filesystem hardening

```bash
sudo cp scripts/angellab.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now angellab
```

### Socket activation (optional)

Lab supports systemd socket activation. Create a companion `.socket` unit:

```ini
# /etc/systemd/system/angellab.socket
[Unit]
Description=AngelLab socket

[Socket]
ListenStream=/run/angellab/lab.sock
SocketMode=0660
SocketUser=angellab
SocketGroup=angellab

[Install]
WantedBy=sockets.target
```

With socket activation, `systemctl start angellab.socket` creates the socket and starts `labd` on first connection. The daemon binary calls `ipc.InheritSystemdListener()` when `$LISTEN_FDS` is set.

---

## Day-to-day commands

### Check overall status

```bash
lab status
# or the live dashboard:
lab tui
```

### Watch live events

```bash
lab events                       # all events
lab events --filter "shadow"     # only events mentioning "shadow"
lab events --filter "CRIT"       # only CRIT events
lab events --since 30m           # events from the last 30 minutes
```

### Inspect an angel

```bash
lab angel list
lab angel inspect A-02
```

### Check a Guardian angel's file integrity

```bash
lab angel diff A-01
```

Output if files are clean:
```
Guardian A-01 — all watched files match snapshots
```

Output if a file changed:
```
Guardian A-01 — 1 file differs from snapshot

  /etc/hosts  (snapshot taken 14m23s ago)
    snapshot sha256: 4a5f8c2d1e9b7f3a…
    current  sha256: 9d2c1f4e8b3a7c6f…
    size delta:  +43 bytes
```

### Add a new angel at runtime

```bash
# Guardian watching a new set of paths
lab angel create guardian --paths /var/www/html/index.php,/var/www/html/.htaccess

# Sentinel with a custom ID
lab angel create sentinel --id A-05

# Terminate an angel (Lab will restart it per restart policy)
lab angel terminate A-03
```

### Pre-flight check on a new machine

```bash
lab doctor
```

---

## Log management

### Viewing logs

```bash
# Systemd journal (real-time)
journalctl -u angellab -f

# Rotating log file
tail -f /var/log/angellab/lab.log

# Last 100 lines
tail -100 /var/log/angellab/lab.log
```

### Log rotation

Built-in rotation activates when the log file exceeds **100 MiB**. The current file is renamed to `lab.log.<timestamp>` and a new file is opened. Up to **5** backups are kept; older ones are deleted automatically.

If you use logrotate instead, add:

```
/var/log/angellab/lab.log {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    postrotate
        kill -HUP $(pidof labd) 2>/dev/null || true
    endscript
}
```

The SIGHUP will cause Lab to close and reopen the log file handle.

### Shipping logs to a SIEM

Enable JSON logging in `angellab.toml`:

```toml
[lab]
log_format = "json"
```

Then ship with your preferred agent:

**Filebeat** (`filebeat.yml`):
```yaml
filebeat.inputs:
  - type: log
    paths: ["/var/log/angellab/lab.log"]
    json.keys_under_root: true
    json.add_error_key: true

output.logstash:
  hosts: ["logstash:5044"]
```

**Promtail / Loki** (`promtail-config.yml`):
```yaml
scrape_configs:
  - job_name: angellab
    static_configs:
      - targets: [localhost]
        labels:
          job: angellab
          __path__: /var/log/angellab/lab.log
    pipeline_stages:
      - json:
          expressions:
            level: level
            component: component
            msg: msg
      - labels:
          level:
          component:
```

---

## Prometheus / Grafana

### Scrape configuration

Add to your `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: angellab
    static_configs:
      - targets: ["<host>:9101"]
    scrape_interval: 15s
```

### Key metrics to alert on

```yaml
# Alert when any angel is UNSTABLE for more than 2 minutes
- alert: AngelUnstable
  expr: angellab_angel_state{state="UNSTABLE"} == 1
  for: 2m
  labels:
    severity: warning
  annotations:
    summary: "Angel {{ $labels.angel_id }} is UNSTABLE"

# Alert when an angel has restarted more than 3 times
- alert: AngelRestartLoop
  expr: angellab_angel_restarts_total > 3
  labels:
    severity: critical
  annotations:
    summary: "Angel {{ $labels.angel_id }} restart loop detected"

# Alert when a CRIT event rate exceeds 1/min over 5 minutes
- alert: AngelCritEventRate
  expr: rate(angellab_events_total{severity="CRIT"}[5m]) * 60 > 1
  labels:
    severity: critical
  annotations:
    summary: "High CRIT event rate from {{ $labels.angel_id }}"

# Alert when RSS exceeds 500 MiB for any angel
- alert: AngelHighMemory
  expr: angellab_angel_rss_bytes > 500 * 1024 * 1024
  labels:
    severity: warning
  annotations:
    summary: "Angel {{ $labels.angel_id }} RSS is {{ $value | humanize1024 }}B"
```

### Example Grafana dashboard panels

| Panel | Query |
|-------|-------|
| Angel states | `angellab_angel_state` |
| Restart rate | `rate(angellab_angel_restarts_total[5m])` |
| Event rate by severity | `rate(angellab_events_total[5m])` |
| Memory by angel | `angellab_angel_rss_bytes` |
| CPU by angel | `angellab_angel_cpu_percent` |
| File descriptors | `angellab_angel_fd_count` |
| Lab uptime | `angellab_lab_uptime_seconds` |

---

## Hot-reload (SIGHUP)

```bash
# Via systemctl (preferred)
sudo systemctl reload angellab

# Via kill
sudo kill -HUP $(pidof labd)
```

What changes take effect:

| Setting | Notes |
|---------|-------|
| `log_level` | Immediate |
| `log_format` | Immediate (text ↔ json) |
| New `[[angel]]` blocks | Newly added angels are spawned |

What requires a full restart:

| Setting | Command |
|---------|---------|
| `socket` | `sudo systemctl restart angellab` |
| `registry` | `sudo systemctl restart angellab` |
| `log_path` | `sudo systemctl restart angellab` |
| `angel_binary` | `sudo systemctl restart angellab` |
| `metrics_addr` | `sudo systemctl restart angellab` |
| `[supervisor]` fields | `sudo systemctl restart angellab` |

---

## Upgrading

```bash
# Build new binaries
make build

# Install
sudo cp build/labd  /usr/local/bin/labd
sudo cp build/lab   /usr/local/bin/lab
sudo cp build/angel /usr/local/bin/angel

# Restart (angels are gracefully terminated and respawned with the new binary)
sudo systemctl restart angellab
```

The registry schema is forward-compatible: new columns are added with defaults, old rows are not modified. If a schema migration is ever required, it will be documented explicitly.

---

## Sentinel baseline management

### Resetting a baseline

If the Sentinel is producing too many false positives after a major traffic change (e.g. new CDN, migrated DNS server), delete the saved baseline and let it retrain:

```bash
# Find the baseline file
ls /var/lib/angellab/sentinel-A-02-baseline.json

# Delete it
sudo rm /var/lib/angellab/sentinel-A-02-baseline.json

# Restart the sentinel angel (it will retrain)
lab angel terminate A-02
# Lab will restart it automatically and retrain from scratch
```

### Extending the training window

Increase `baseline_duration` in `angellab.toml` and send SIGHUP, then restart the Sentinel:

```toml
[[angel]]
type = "sentinel"
id   = "A-02"
baseline_duration = "600s"  # 10 minutes
```

```bash
sudo systemctl reload angellab
lab angel terminate A-02  # respawned with new config
```

---

## Common issues

### `lab: cannot connect to daemon`

Causes:
1. `labd` is not running: `sudo systemctl start angellab`
2. Wrong socket path: check `LAB_SOCKET` env var and `[lab] socket` in config
3. Permission denied: ensure your user is in the `angellab` group

```bash
# Check socket existence
ls -la /run/angellab/lab.sock

# Check daemon status
sudo systemctl status angellab

# Run doctor
lab doctor
```

### Angel stuck in TRAINING

The Sentinel and Process angels have a training window (`baseline_duration`). During this time they observe without alerting. If a Sentinel stays in TRAINING for longer than `baseline_duration`:

1. Check that `/proc/net/tcp` is readable: `lab doctor`
2. Check if the angel is receiving heartbeat responses (Lab is alive)
3. Check the log for errors: `journalctl -u angellab | grep A-02`

### Guardian not restoring files

If Guardian detects a change but does not restore:

1. Check that the `snapshot_dir` is writable: `ls -la /var/lib/angellab/snapshots/`
2. Check that the snapshot exists: `ls /var/lib/angellab/snapshots/*.snap`
3. The snapshot is taken at startup. If it was corrupted or missing at startup, Guardian will warn and continue without restore capability.

### Angel in CONTAINED state

An angel reaches CONTAINED after `max_restarts` consecutive failures. To manually restart:

```bash
# Create a new angel with the same config
lab angel create guardian --id A-01 --paths /etc/passwd,/etc/shadow

# Or restart the entire daemon to trigger boot recovery
sudo systemctl restart angellab
```

### High memory usage in Sentinel

The Sentinel's deduplicator can hold up to 100,000 entries before pruning. On machines with very high connection churn (container hosts, busy proxies), this is normal. If RSS is consistently above 200 MiB:

1. Reduce `inode_cache_ttl` to free the inode cache faster
2. Increase `poll_interval` to reduce the sampling rate
3. Consider running separate Sentinel instances for different network interfaces

### inotify limit exhausted

```
Error: inotify: no space left on device
```

Increase the kernel limit:

```bash
echo 524288 > /proc/sys/fs/inotify/max_user_watches
# Make permanent:
echo "fs.inotify.max_user_watches = 524288" >> /etc/sysctl.conf
sysctl -p
```
