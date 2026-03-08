# Failure Semantics

AngelLab is designed around two principles: **fail fast on broken control channels** and **let the supervisor handle recovery**. This document catalogues every failure mode and what the system does about it.

---

## The two-layer failure detection model

### Layer 1 — Angel-side (primary, fast)

Every angel's `sendHeartbeat()` function returns an error. On each heartbeat tick:

```go
case <-heartbeatTick.C:
    if err := a.sendHeartbeat(); err != nil {
        // First attempt failed — try once more.
        // This filters transient socket hiccups from genuine Lab death.
        if retry := a.sendHeartbeat(); retry != nil {
            return fmt.Errorf("heartbeat failed (2 attempts): %w", retry)
        }
    }
```

When `sendHeartbeat()` fails twice in a row, `run()` returns an error, which causes `os.Exit(1)`. This triggers the supervisor's restart machinery immediately.

**Detection time**: one heartbeat interval (default 10 s). At most 10 s after Lab disappears, every angel will have attempted two heartbeats and exited.

The one-retry grace period filters transient OS socket buffer glitches from genuine Lab death. A single write error can occur if the kernel briefly pauses the socket; a second consecutive failure within the same tick means the control plane is gone.

### Layer 2 — Lab-side (safety net, slow)

The Supervisor's `checkHeartbeats()` loop runs every `HeartbeatInterval` and marks any angel `UNSTABLE` that has not sent a heartbeat in more than `HeartbeatTimeout`. This is the safety net for angels that:

- Predate the Step 8 fail-fast fix (upgraded deployments)
- Are stuck in a blocking syscall that bypasses the heartbeat ticker
- Experienced a one-way failure where `Send()` succeeded but the data was silently dropped

When an angel is marked UNSTABLE, `MarkConnLost()` opens the 10-second recovery window. If no reconnect arrives, the Supervisor sends SIGTERM to the angel process and applies the restart policy.

---

## Failure mode catalogue

### F1: Angel process crashes

**What happens:**
1. The angel process exits (any exit code).
2. `exec.Cmd.Wait()` returns in the `runAngel` goroutine.
3. If the connection was active, the Lab server's `conn.Recv()` returns EOF; `MarkConnLost()` is called.
4. Boot recovery and the restart policy are applied.

**Recovery:**
- Wait `restart_backoff`, respawn.
- After `max_restarts` failures: move to CONTAINED.

**Detection time**: immediate (the process exit wakes `cmd.Wait()`).

**What is lost**: in-memory state (sliding windows, connection deduplicator, etc.) since the last heartbeat. Persisted baselines (Sentinel, Process) are loaded on the next startup.

---

### F2: Angel hangs — heartbeat stops, process still running

**What happens:**
1. Angel is stuck in a blocking syscall (e.g. inotify read that never returns, a file operation on a hung NFS mount).
2. The heartbeat ticker never fires in the angel's select loop.
3. Lab's `checkHeartbeats()` notices the missing heartbeat after `HeartbeatTimeout` (default 30 s) and marks the angel UNSTABLE.
4. `MarkConnLost()` opens a 10-second recovery window.
5. After the window: SIGTERM is sent to the angel's PID.

**Detection time**: up to `HeartbeatTimeout` (30 s) + 10 s recovery window = **40 s maximum**.

**Recovery:** same restart policy as F1.

---

### F3: Lab daemon crashes

**What happens:**
1. `labd` process exits (crash or OOM kill).
2. The Unix socket is deleted.
3. Each angel's next heartbeat `Send()` fails with `ECONNRESET` or `EPIPE`.
4. After the one-retry grace period, the angel calls `return fmt.Errorf("heartbeat failed (2 attempts): %w", err)`.
5. `os.Exit(1)` is called in each angel process.
6. Systemd restarts Lab (`Restart=on-failure`).
7. Lab's `BootRecovery()` verifies stale PIDs and respawns all static angels.

**Detection time**: at most one heartbeat interval (10 s) after Lab exits. All connected angels exit within this window.

**Recovery timeline:**
```
t=0    Lab crashes
t=0    Socket deleted
t=10s  Angel heartbeats fail (worst case: just sent one, waits 10s for next)
t=10s  Angels exit (os.Exit(1))
t=15s  systemd RestartSec elapses (default 5s in unit file)
t=15s  Lab restarts, socket recreated
t=15s  BootRecovery kills any lingering PIDs, spawns fresh angels
t=17s  Angels connect, register, send initial heartbeat
t=17s  System fully restored
```

**What is lost**: events emitted between the last successful send and Lab's restart. Persisted baselines are loaded on restart.

---

### F4: Lab daemon is restarted intentionally (systemctl restart)

systemd sends SIGTERM to `labd`. Lab's signal handler calls `Daemon.TerminateAll()`:

1. For each active angel: send `KindCmdTerminate` over the IPC connection.
2. Wait for `cmd.Wait()` (up to `TimeoutStopSec=30s` in the unit file).
3. If an angel does not exit within the timeout: SIGKILL.

After Lab restarts, `BootRecovery()` respawns all static angels. This is a clean path with no data loss beyond the restart window.

---

### F5: Transient network glitch (socket hiccup, brief kernel buffer pause)

**What happens:**
1. One `sendHeartbeat()` call fails with a transient error.
2. The retry attempt (within the same ticker cycle) succeeds.
3. The angel continues normally. No exit, no event.

**Detection**: the one-retry policy was specifically designed to absorb these. The retry happens immediately within the same goroutine; no backoff or jitter is needed because the ticker already provides the 10-second spacing between cycles.

---

### F6: Angel cannot connect at startup (Lab not yet running)

**What happens:**
1. `ipc.Dial()` fails.
2. `run()` returns `fmt.Errorf("dial lab: %w", err)`.
3. `os.Exit(1)` is called.

**Lab side:** `cmd.Wait()` returns; the Supervisor applies the restart policy with backoff. This naturally handles the race between Lab starting and angels dialing: after a few retries, Lab is up and the angel connects.

---

### F7: Angel reconnects within the recovery window

Angels do not self-reconnect (by design — see Architecture). If an angel's IPC connection drops but the process is still alive (e.g. a brief socket error that does not kill the process), the 10-second recovery window gives the angel a chance to reconnect.

In practice, this window is rarely used: if the connection drops, the angel's heartbeat will fail within the next tick and the angel will exit, triggering a clean restart. The recovery window handles the (rare) case where the angel detects the drop before the heartbeat fires and somehow re-dials — but this is not a supported code path in the current implementation.

---

### F8: Protocol version mismatch

If an angel binary has a different `ProtocolVersion` than Lab:

1. HELLO is exchanged; Lab checks the version field.
2. Mismatch: Lab closes the connection immediately with a log warning.
3. The angel's `Dial()` function returns an error: `"ipc: version mismatch"`.
4. The angel exits.
5. Lab applies the restart policy — the angel will keep failing until a matching binary is deployed.

**Detection**: immediate, on every connection attempt.

---

### F9: Angel registry row missing (angel existed before database wipe)

If the registry database is deleted while angels are running:

1. Lab cannot find the angel's row for `UpdateState` calls — these log warnings but do not crash.
2. Heartbeats are still processed (they use the in-memory map).
3. On next restart, `BootRecovery` finds no stale rows and spawns all static angels fresh.

**Recovery**: no data loss beyond the registry history. Running angels continue operating throughout.

---

### F10: Sentinel baseline version mismatch (schema upgrade)

If `baselineSchemaVersion` is bumped in a new release and an old baseline file exists:

1. `loadBaseline()` reads the file, checks the `version` field.
2. Mismatch: returns a typed error: `"baseline schema v0 is incompatible with current v1 — will retrain"`.
3. The Sentinel logs a warning and starts a fresh training window.

**Recovery**: the Sentinel retrains from scratch. The old baseline file is not deleted (for debugging); it will be overwritten when the new baseline is persisted at the end of training.

---

### F11: Guardian snapshot missing or corrupted

If a snapshot file is missing at startup (first run, or snapshot directory deleted):

1. Guardian takes a fresh snapshot of each watched file before activating inotify watches.
2. Subsequent modifications are detected and restored from these snapshots.

If a snapshot file is corrupted (truncated, invalid JSON):

1. Guardian logs a warning on the restore attempt.
2. The corrupt snapshot is overwritten with the current (modified) file content.
3. The guardian continues watching — but the "correct" state is now whatever the file contained at the time of the corruption.

**Operator action**: if the snapshot was corrupted by an attacker, check the file content against a known-good source (backup, package manager) and manually restore, then `lab angel terminate A-01` to trigger a fresh snapshot.

---

## Recovery matrix

| Failure | Detection | Mechanism | Data lost |
|---------|-----------|-----------|-----------|
| Angel crash | Immediate | `cmd.Wait()` | In-memory state since last heartbeat |
| Angel hang | ≤ 40 s | Lab heartbeat watcher + SIGTERM | Same as crash |
| Lab crash | ≤ 10 s | Angel heartbeat failure | Events in the crash window |
| Lab intentional restart | Graceful | `KindCmdTerminate` + `cmd.Wait()` | None |
| Transient glitch | Absorbed | One-retry policy | None |
| Version mismatch | Immediate | HELLO check | N/A (won't connect) |
| Baseline version mismatch | At startup | Schema check | Baseline (retrains) |

---

## Operational guarantees

1. **No silent disconnection**: every angel detects a dead Lab connection within one heartbeat interval (≤ 10 s) and exits cleanly.

2. **No orphan processes**: if Lab crashes, all angel processes exit within the detection window. Systemd restarts Lab, which kills any remaining angel PIDs via `BootRecovery` before spawning fresh ones.

3. **Deterministic recovery**: the recovery path is always `angel exits → supervisor detects → restart with backoff`. There is no hidden reconnect loop or recovery path that bypasses the supervisor.

4. **Baseline preservation**: Sentinel and Process baselines are persisted to disk at the end of the training window. A Lab restart does not require retraining (unless the schema version changed).

5. **At-most-once restart**: the Supervisor uses a `sync.Mutex`-protected state machine to ensure only one restart goroutine is active per angel at any time.
