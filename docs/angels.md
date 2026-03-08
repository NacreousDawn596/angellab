# Angel Reference

Detailed documentation for all four built-in angel types: internal design, event catalogue, configuration tuning, and operational notes.

---

## Guardian

### What it does

Guardian watches a fixed list of files using the Linux `inotify(7)` subsystem. On any detected change it:

1. Computes the SHA-256 of the current file
2. Compares against the stored snapshot
3. If different: restores the snapshot atomically via `rename(2)` (write to temp, then rename)
4. Emits events for both the detection and the restore

This makes Guardian a **self-healing integrity monitor**: it does not just alert, it corrects.

### Snapshot model

Snapshots are taken at startup and refreshed every **10 minutes** by the `refreshSnapshots()` goroutine. They are stored in `snapshot_dir` as flat binary files (exact copy of the watched file's content at snapshot time).

Naming: `<basename>.snap`. If two watched files share a basename (e.g. `/etc/network/interfaces` and `/etc/hosts/interfaces`), the collision is resolved by using `<sha256hex[:16]-of-fullpath>.snap` for the second file.

If a file does not exist when Guardian starts, it logs a warning and continues. The file will be watched if it is created later (via `IN_CREATE` events).

### inotify event handling

Guardian uses mask `IN_MODIFY | IN_CREATE | IN_DELETE | IN_MOVE`.

| Event | Action |
|-------|--------|
| `IN_MODIFY` | Hash + compare + restore if changed |
| `IN_CREATE` | Hash + compare + restore if changed |
| `IN_MOVED_TO` | Hash + compare + restore if changed |
| `IN_DELETE_SELF` | Re-add inotify watch for the new inode (handles atomic editors like vim, sed -i) |
| `IN_MOVE_SELF` | Re-add watch |
| `IN_IGNORED` | Watch descriptor was removed; re-add |

Editors that use atomic writes (vim, `sed -i`, `install(1)`) write to a temp file then rename. This removes the original inode and creates a new one. Guardian handles this by re-adding the inotify watch after `IN_DELETE_SELF`.

### Event catalogue

| Severity | Message pattern | When |
|----------|----------------|------|
| INFO | `Guardian A-01: snapshot refreshed (5 files)` | Every 10 minutes |
| CRIT | `Guardian A-01: modification detected: /etc/shadow` | Any inotify event on watched file |
| CRIT | `Guardian A-01: restored /etc/shadow from snapshot (delta: +43 bytes)` | After successful restore |
| WARN | `Guardian A-01: restore failed for /etc/shadow: permission denied` | Restore error |
| WARN | `Guardian A-01: snapshot missing for /etc/shadow — monitoring only` | File not snapshotted yet |

### Diff command

`lab angel diff A-01` calls `DiffSnapshot()` in `internal/angels/guardian/diff.go`. It walks the snapshot directory, finds matching watched files, and computes SHA-256 for both. It reports only differences — matching files are not listed.

Output includes: path, snapshot age, snapshot SHA-256 prefix, current SHA-256 prefix, and byte size delta. Full file content is never included (to avoid leaking secrets in the CLI output).

### Tuning

**More files:** Simply add them to `paths`. There is no limit other than the system `max_user_watches` limit (`/proc/sys/fs/inotify/max_user_watches`). `lab doctor` warns if this is below 65,536.

**Disable auto-restore:** Remove write access from the `angellab` user to the watched files. Guardian will log the restore failure and emit a WARN, but continue monitoring. This is useful when you want detection-only without corrective action.

**Watching directories:** Not directly supported — Guardian watches individual files. To watch a directory's contents, enumerate the files explicitly. Wildcard support is a planned future feature.

---

## Sentinel

### What it does

Sentinel monitors outbound TCP connections by polling `/proc/net/tcp` and `/proc/net/tcp6` every `poll_interval` (default 2 s). It learns what "normal" looks like during a training window, then alerts on deviations.

### Training phase

During the training window (`baseline_duration`, default 60 s), Sentinel calls `baseline.Observe(conns)` on each poll. `Observe` records:

- All unique (IP, port) pairs seen
- The concurrent connection count per sample
- The maximum concurrent count seen across all samples

At the end of the training window, `baseline.Freeze()` is called. The baseline is then persisted to `<state_dir>/sentinel-<id>-baseline.json`. On next startup, if the schema version matches, the saved baseline is loaded and training is skipped.

### Active phase

Each poll produces a list of currently established outbound connections. For each connection:

1. Check the `Deduplicator` — if this (IP, port) was seen recently, `isNew = false`
2. Call `Scorer.Score(conn, isNew, currentTotal)` to get an `AnomalyScore`
3. If `score.Level() >= ScoreWarn`: emit an event with the score and reasons

The Deduplicator uses a 2-minute TTL (connections are considered "new again" after 2 minutes of absence).

### Scoring rules in detail

```
Rule                          Weight  Notes
────────────────────────────────────────────────────────────────
New remote IP                   +3    Most common C2 indicator
New remote port                 +2    Unknown service
Ephemeral port (>49152)         +1    Above IANA registered range
Very high port (>60000)         +1    Stacks with above: total +2
Connection burst (>20 new/5s)   +4    Scanning or beaconing pattern
Concurrent spike (>2× max)      +3    Mass connection event
Private source (RFC1918)        −1    Internal traffic, usually OK
Loopback remote                 −5    Always below threshold (exits early)
```

Score 3 → WARN. Score 6+ → CRITICAL.

**Example scores:**

| Scenario | Score | Level |
|----------|-------|-------|
| Known IP, known port | 0 | OK |
| New IP, known port 443 | 3 | WARN |
| New IP, new port 443 | 5 | WARN |
| New IP, high port | 4 | WARN |
| New IP, new high port | 6 | CRIT |
| New IP, new port, burst | 9 | CRIT |
| RFC1918 new IP, new port | 4 | WARN |
| Loopback any | -5 | OK |

### Process attribution

For each connection, the Sentinel attempts to resolve the owning process:

1. Read the socket inode from `/proc/net/tcp`
2. Look up the inode in the `InodeCache` (maps inode → PID)
3. Read `/proc/<pid>/exe` and `/proc/<pid>/comm`

If the process is not found (race between connection establishment and polling), the event is emitted with `"process": "unknown"`.

### Inode cache

The `InodeCache` rebuilds by iterating `/proc/*/fd/` every `InodeCacheTTL` (default 10 s). This operation is bounded: it reads symlink targets for all file descriptors of all processes, which is O(total FDs). On a typical server with 1,000 processes and 20 FDs each, this is ~20,000 `readlink` calls per rebuild — taking ~5–20 ms.

Double-checked locking prevents concurrent rebuilds: the first goroutine to find a stale cache takes the write lock; others find the lock held and wait.

### Event catalogue

| Severity | Message pattern | When |
|----------|----------------|------|
| INFO | `Sentinel A-02: baseline frozen (47 known IPs, 12 ports, max concurrent 23)` | End of training |
| INFO | `Sentinel A-02: loaded saved baseline from disk` | Startup with existing baseline |
| WARN | `Sentinel A-02: anomaly score 4 (new IP 185.199.108.153 + new port 8443) — process: git` | Score ≥ WarnThreshold |
| CRIT | `Sentinel A-02: anomaly score 8 (new IP 45.33.32.156 + new port 4444 + high port) — process: bash (pid 8823, exe /tmp/.x)` | Score ≥ CritThreshold |
| WARN | `Sentinel A-02: connection burst: 23 new connections in 5s` | Burst rule triggered |

### Tuning

**Too many false positives on first deployment:**
- Increase `baseline_duration` to capture more traffic patterns (try `"300s"` or `"600s"`)
- Delete the saved baseline and retrain after the network traffic stabilises
- Raise `warn_threshold` from 3 to 4 or 5

**Missing legitimate connections:**
- Check if the missed IP/port appeared during training (if you added it after training, it won't be in the baseline)
- Lower `poll_interval` for faster detection at the cost of more CPU

**Too noisy on a container host:**
- Use a separate Sentinel per workload namespace
- Add known CDN IP ranges to the baseline by running a deliberate training window after deployment

---

## Memory Angel

### What it does

Memory Angel tracks RSS (Resident Set Size) for a set of processes over a sliding time window. It detects:

- Slow percentage-based growth (classic memory leak pattern)
- Fast rate-based growth (sudden allocation spike — possible runaway process or bug)
- Absolute RSS thresholds (catches processes that were already large at startup)
- cgroup v2 OOM kills (host-level pressure indicator)

### Process selection

Three modes, combinable:

| Mode | Config | Use case |
|------|--------|---------|
| Static PID list | `pids: [1234, 5678]` | Known long-lived processes (e.g. database, web server) |
| By comm name | `process_names: ["nginx", "postgres"]` | Processes where PID changes on restart |
| Watch all | `watch_all: true` | Container hosts, detect any runaway process |

When `process_names` is used, the angel resolves names to PIDs on each poll by scanning `/proc/*/comm`. If a name matches multiple processes, all are tracked.

### Sliding window model

Each tracked process has a `rssWindow`: a circular buffer of `(timestamp, rss_bytes)` samples. Size is `window_size` (default 12 samples × 5 s = 1 minute of history).

On each poll:
1. Read `/proc/<pid>/status` for current `VmRSS`
2. Append to the circular buffer (overwrites oldest on overflow)
3. Compute `growth_pct = (newest_rss - oldest_rss) / oldest_rss × 100`
4. Compute `growth_rate_kbps = (newest_rss - oldest_rss) / elapsed_seconds / 1024`
5. Compare both against WARN and CRIT thresholds

Alert cooldown (`alert_cooldown`, default 5 min) prevents repeated alerts for the same process. The cooldown is per-PID and per-alert-type.

### cgroup v2 monitoring

When `cgroup_path` is set, the angel reads `<cgroup_path>/memory.events` on each poll and checks the `oom_kill` counter. If it increased since the last poll, a CRIT event is emitted.

```
/sys/fs/cgroup/system.slice/angellab.service/memory.events:
  low 0
  high 0
  max 0
  oom 0
  oom_kill 3    ← this counter going up triggers CRIT
```

### Event catalogue

| Severity | Message pattern | When |
|----------|----------------|------|
| WARN | `MemoryAngel A-03: nginx (pid 1234) RSS 54.3 MiB → 82.1 MiB (+51.2% in 60s)` | growth_pct ≥ growth_warn_pct |
| CRIT | `MemoryAngel A-03: nginx (pid 1234) RSS 210.5 MiB (+215.3% in 60s)` | growth_pct ≥ growth_crit_pct |
| WARN | `MemoryAngel A-03: redis (pid 5678) RSS 523 MiB exceeded warn threshold (512 MiB)` | abs_warn_mb exceeded |
| CRIT | `MemoryAngel A-03: postgres (pid 9012) RSS 2.1 GiB exceeded critical threshold (2048 MiB)` | abs_crit_mb exceeded |
| WARN | `MemoryAngel A-03: nginx (pid 1234) growth rate 12.3 MB/s (warn threshold: 10 MB/s)` | growth_rate_warn_kbps exceeded |
| CRIT | `MemoryAngel A-03: cgroup OOM kill detected (oom_kill count: 3)` | cgroup oom_kill counter increased |
| INFO | `MemoryAngel A-03: process 1234 no longer exists — removing from watch list` | Watched process exited |

### Tuning

**Reduce noise for processes with naturally high growth:**
- Increase `growth_warn_pct` and `growth_crit_pct`
- Increase `alert_cooldown` to space out repeated alerts
- Increase `window_size` for a longer trend window (less sensitive to bursts)

**Detect faster leaks:**
- Lower `window_size` and `poll_interval`
- Lower `growth_rate_warn_kbps`

**Watch a Java / JVM process:** JVM has a large heap that grows intentionally. Focus on `abs_warn_mb` and `abs_crit_mb` thresholds rather than percentage growth, or set a very high `growth_crit_pct`.

---

## Process Angel

### What it does

Process Angel scans `/proc` every `poll_interval` (default 2 s) and diffs the process table against the previous snapshot. New processes are scored using a set of rules. Processes that score above the warn threshold emit events.

### Training phase

During `baseline_duration` (default 30 s), the angel scans `/proc` on each cycle and adds every discovered process to the baseline. At the end of training, the baseline is persisted to `<state_dir>/process-<id>-baseline.json`.

A process is considered "known" if its `exe` path, `comm` name, and `ppid` were all seen during training. A new process that matches all three is scored 0 (below threshold).

### Scoring rules in detail

```
Rule                                  Weight  Notes
──────────────────────────────────────────────────────────────────
Exe from suspicious dir                +5    /tmp, /dev/shm, /var/tmp, /run/user
Missing exe symlink (/proc/pid/exe)    +3    Packed binary or fileless execution
Unknown exe path (not in baseline)     +3    New binary not seen during training
Unknown comm name (not in baseline)    +2    New process name
Unknown PPID (parent not in baseline)  +2    Unexpected parent (injected process?)
Setuid or setgid                       +2    UID ≠ EUID, or GID ≠ EGID
Whitelist match (exe or comm)         −10    Guaranteed below threshold
```

Default threshold: WARN ≥ 3, CRIT ≥ 6 (same scale as Sentinel).

### Whitelist

`whitelist_exes` and `whitelist_comms` permanently suppress alerts for known-good processes. The whitelist is applied before scoring; a match contributes −10, making the total always below any threshold.

`whitelist_exes` is an exact match against `/proc/<pid>/exe`. `whitelist_comms` is a prefix/partial match against `/proc/<pid>/comm` (a comm of `"systemd"` matches `"systemd-journald"`, `"systemd-resolved"`, etc.).

### Suspicious directory detection

Processes with their executable in `/tmp`, `/dev/shm`, `/var/tmp`, or `/run/user` are scored +5 immediately. This catches the most common attacker technique: dropping a binary to a writable directory and executing it.

Custom suspicious directories can be added via `suspicious_exe_dirs`:

```toml
[[angel]]
type = "process"
[angel.extra]
suspicious_exe_dirs = "/tmp,/dev/shm,/var/tmp,/run/user,/home/ubuntu,/var/www"
```

### Exit monitoring

When `alert_on_exit = true` (default), the angel emits INFO events when a previously seen baseline process exits unexpectedly. This is useful for detecting:

- Killed system daemons (sshd, cron, systemd-resolved)
- Services that crash and are not automatically restarted
- Processes killed by an attacker to disable defenses

Short-lived processes (shells, cron jobs, one-shot services) will produce a lot of noise with `alert_on_exit`. Suppress them with `whitelist_comms`.

### Event catalogue

| Severity | Message pattern | When |
|----------|----------------|------|
| INFO | `ProcessAngel A-04: baseline frozen (147 processes)` | End of training |
| WARN | `ProcessAngel A-04: new process score 4 — pid 8823 comm bash exe /usr/bin/bash ppid 4211` | score ≥ WarnThreshold |
| CRIT | `ProcessAngel A-04: suspicious process score 8 — pid 9001 comm sh exe /tmp/.x ppid 1 (exe in suspicious dir + unknown exe + setuid)` | score ≥ CritThreshold |
| INFO | `ProcessAngel A-04: baseline process exited — pid 1234 comm sshd exe /usr/sbin/sshd` | alert_on_exit + known process exited |

### Tuning

**Too noisy on a system with many short-lived processes (container host, CI runner):**
- Add common shells to `whitelist_comms`: `sh,bash,dash`
- Add package manager binaries to `whitelist_exes`
- Set `alert_on_exit = false`
- Increase `baseline_duration` to capture more of the process lifecycle

**Not catching setuid binaries:**
- Verify `ProcessAngel` can read `/proc/<pid>/status` (requires read access to other processes' status files — usually available as root or with `CAP_SYS_PTRACE`)

**Known legitimate process in /tmp (rare):**
- Add its path to `whitelist_exes`
- Or move the binary out of `/tmp` (strongly recommended for security reasons)
