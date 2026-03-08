// Package lab — supervisor.go
//
// The Supervisor owns the in-memory process table and drives the angel
// lifecycle.  Every angel gets one long-running goroutine (runAngel) that:
//
//  1. Spawns the OS process with Setpgid=true (signal isolation).
//  2. Waits for the REGISTER message via RegisterConn.
//  3. Monitors heartbeats via HandleHeartbeat.
//  4. On connection drop: opens a recovery window (ConnTracker.MarkLost).
//  5. On clean process exit or recovery expiry: applies restart policy.
//  6. After MaxRestarts: moves angel to CONTAINED, stops restarting.
//
// Boot recovery (BootRecovery, called once at startup):
//
//  1. Load former ACTIVE/TRAINING angels from registry.
//  2. Verify each stale PID via /proc/<pid>/cmdline before killing it.
//  3. Respawn them as fresh processes.
package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/nacreousdawn596/angellab/pkg/ipc"
	"github.com/nacreousdawn596/angellab/pkg/logging"
	"github.com/nacreousdawn596/angellab/pkg/registry"
)

// ---------------------------------------------------------------------------
// AngelEntry
// ---------------------------------------------------------------------------

// AngelEntry is the in-memory record for one angel process.
// It mirrors the registry.Angel row and adds live runtime state.
type AngelEntry struct {
	mu sync.Mutex

	ID           string
	AngelType    string
	State        registry.AngelState
	RestartCount int
	PID          int
	StartedAt    time.Time
	configJSON   string

	// conn is the open IPC connection from the angel.
	// nil when the angel has not yet registered or has disconnected.
	conn *ipc.Conn

	// LastHeartbeat is the wall-clock time of the most recent heartbeat.
	LastHeartbeat time.Time

	// Telemetry is the most recent HeartbeatPayload, kept for CLI inspect.
	Telemetry *ipc.HeartbeatPayload

	// ConnTrack manages the IPC link state machine.
	ConnTrack ConnTracker

	// Config is the AngelConfig used to spawn this angel.
	// Needed by server.go for angel.diff (paths, snapshotDir).
	Config *AngelConfig

	// holder stores the active exec.Cmd.
	holder *cmdHolder
}

type cmdHolder struct{ c *exec.Cmd }

func (e *AngelEntry) setCmd(cmd *exec.Cmd) { e.holder = &cmdHolder{c: cmd} }
func (e *AngelEntry) getCmd() *exec.Cmd {
	if e.holder == nil {
		return nil
	}
	return e.holder.c
}

// ---------------------------------------------------------------------------
// Supervisor
// ---------------------------------------------------------------------------

// Supervisor manages all angel processes.
type Supervisor struct {
	cfg   *Config
	log   *logging.Logger
	reg   *registry.Registry
	bcast *Broadcaster

	mu     sync.RWMutex
	angels map[string]*AngelEntry

	// metricsReg is wired in by Daemon after the metrics server starts.
	// Nil when the metrics endpoint is disabled.
	metricsReg *metricsHook
}

// metricsHook avoids an import cycle between internal/lab and pkg/metrics
// by carrying the update functions as plain func values.
type metricsHook struct {
	UpdateAngel    func(id, typ, state, connState string, restarts int, rss uint64, cpu float64, fd, goroutines int, uptime int64)
	IncrementEvent func(angelID, angelType, severity string)
}

// NewSupervisor constructs a Supervisor. It starts no goroutines.
func NewSupervisor(cfg *Config, log *logging.Logger, reg *registry.Registry, bcast *Broadcaster) *Supervisor {
	return &Supervisor{
		cfg:    cfg,
		log:    log,
		reg:    reg,
		bcast:  bcast,
		angels: make(map[string]*AngelEntry),
	}
}

// ---------------------------------------------------------------------------
// Boot recovery
// ---------------------------------------------------------------------------

// BootRecovery reconciles the registry with the OS process table.
// Called once at Lab startup before accepting any connections.
func (s *Supervisor) BootRecovery(ctx context.Context) error {
	s.log.Info("boot recovery: scanning registry")

	recoverable, err := s.reg.ListRecoverableAngels()
	if err != nil {
		return fmt.Errorf("boot recovery: list: %w", err)
	}

	if err := s.reg.MarkStale(); err != nil {
		return fmt.Errorf("boot recovery: mark stale: %w", err)
	}

	s.log.Info("boot recovery: %d angel(s) to recover", len(recoverable))

	for _, a := range recoverable {
		// Verify PID via /proc/<pid>/cmdline before sending any signal.
		// PID recycling is rare but possible — we must not kill an unrelated process.
		if a.PID > 0 {
			if verifyAngelPID(a.PID) {
				if err := syscall.Kill(a.PID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
					s.log.Warn("boot recovery: kill pid %d for %s: %v", a.PID, a.ID, err)
				} else {
					s.log.Info("boot recovery: killed stale angel %s (pid %d)", a.ID, a.PID)
				}
			} else {
				s.log.Debug("boot recovery: pid %d for %s is gone or not an angel — skipping kill",
					a.PID, a.ID)
			}
		}

		cfg := &AngelConfig{Type: string(a.AngelType), ID: a.ID}
		if a.ConfigJSON != "" {
			_ = json.Unmarshal([]byte(a.ConfigJSON), cfg)
		}

		if err := s.SpawnAngel(ctx, cfg); err != nil {
			s.log.Warn("boot recovery: failed to respawn %s: %v", a.ID, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Spawning
// ---------------------------------------------------------------------------

// SpawnAngel creates or updates a registry entry and launches the process.
func (s *Supervisor) SpawnAngel(ctx context.Context, cfg *AngelConfig) error {
	if cfg.ID == "" {
		cfg.ID = s.generateID()
	}

	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("spawn: marshal config: %w", err)
	}

	// Upsert — registry.InsertAngel is a no-op if the row exists.
	_ = s.reg.InsertAngel(&registry.Angel{
		ID:         cfg.ID,
		AngelType:  cfg.Type,
		ConfigJSON: string(configJSON),
	})
	_ = s.reg.UpdateState(cfg.ID, registry.StateCreated)

	entry := &AngelEntry{
		ID:         cfg.ID,
		AngelType:  cfg.Type,
		State:      registry.StateCreated,
		configJSON: string(configJSON),
	}
	// Initialise connection tracker to CONNECTING state.
	_ = entry.ConnTrack.Transition(ConnStateConnecting)

	s.mu.Lock()
	s.angels[cfg.ID] = entry
	s.mu.Unlock()

	go s.runAngel(ctx, entry)
	return nil
}

// runAngel is the per-angel goroutine that owns subprocess lifecycle.
func (s *Supervisor) runAngel(ctx context.Context, e *AngelEntry) {
	for {
		if err := s.startProcess(e); err != nil {
			s.log.Warn("[Lab] angel %s failed to start: %v", e.ID, err)
			_ = s.reg.UpdateState(e.ID, registry.StateUnstable)
			return
		}

		s.log.Info("[Angel Lab] angel %s (%s) started — pid %d",
			e.ID, e.AngelType, e.PID)
		_ = s.reg.UpdateState(e.ID, registry.StateTraining)
		_ = s.reg.UpdatePID(e.ID, e.PID)

		// Wait for the process to exit.
		exitErr := e.getCmd().Wait()

		e.mu.Lock()
		e.State = registry.StateUnstable
		e.conn = nil
		e.mu.Unlock()

		if exitErr != nil {
			s.log.Warn("[Angel Lab] angel %s exited with error: %v", e.ID, exitErr)
		} else {
			s.log.Info("[Angel Lab] angel %s exited cleanly", e.ID)
		}

		// If we were in the recovery window, give it a moment before restarting
		// to avoid a spin-loop if the angel crashes on startup.
		if e.ConnTrack.State() == ConnStateRecovering {
			s.log.Debug("angel %s was in recovery window at exit — waiting briefly", e.ID)
			select {
			case <-ctx.Done():
				_ = s.reg.UpdateState(e.ID, registry.StateTerminated)
				return
			case <-time.After(500 * time.Millisecond):
			}
		}

		// Apply restart policy.
		count, _ := s.reg.IncrementRestarts(e.ID)
		e.mu.Lock()
		e.RestartCount = count
		e.mu.Unlock()

		if count >= s.cfg.Supervisor.MaxRestarts {
			s.log.Crit("[Angel Lab] angel %s exceeded max restarts (%d) — CONTAINED",
				e.ID, s.cfg.Supervisor.MaxRestarts)
			_ = s.reg.UpdateState(e.ID, registry.StateContained)
			return
		}

		select {
		case <-ctx.Done():
			_ = s.reg.UpdateState(e.ID, registry.StateTerminated)
			return
		case <-time.After(s.cfg.Supervisor.RestartBackoff.Duration):
			s.log.Info("[Angel Lab] restarting angel %s (attempt %d/%d)",
				e.ID, count+1, s.cfg.Supervisor.MaxRestarts)
			// Reset connection tracker for the new attempt.
			_ = entry_resetConn(e)
		}
	}
}

// startProcess builds the exec.Cmd and starts the angel subprocess.
func (s *Supervisor) startProcess(e *AngelEntry) error {
	cmd := exec.Command(
		s.cfg.Lab.AngelBinary,
		e.AngelType,
		"--id", e.ID,
		"--lab", s.cfg.Lab.SocketPath,
	)

	// Deliver config via stdin — keeps it out of /proc/<pid>/cmdline.
	cmd.Stdin = newConfigReader(e.configJSON)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Setpgid: true — angel runs in its own process group.
	// A signal sent to the Lab's process group (e.g. SIGINT from the terminal)
	// will NOT propagate to angels.  Lab shuts them down explicitly via IPC.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec %s %s: %w", s.cfg.Lab.AngelBinary, e.AngelType, err)
	}

	e.mu.Lock()
	e.PID = cmd.Process.Pid
	e.StartedAt = time.Now()
	e.setCmd(cmd)
	e.mu.Unlock()

	return nil
}

// entry_resetConn resets the connection-related fields of an entry for a restart.
func entry_resetConn(e *AngelEntry) error {
	e.mu.Lock()
	e.conn = nil
	e.LastHeartbeat = time.Time{}
	e.Telemetry = nil
	e.mu.Unlock()
	e.ConnTrack.mu.Lock()
	e.ConnTrack.state = ConnStateConnecting
	if e.ConnTrack.recoveryTimer != nil {
		e.ConnTrack.recoveryTimer.Stop()
		e.ConnTrack.recoveryTimer = nil
	}
	e.ConnTrack.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// Connection registration (called from Server on REGISTER message)
// ---------------------------------------------------------------------------

// RegisterConn binds an open IPC connection to the angel.
// If the angel was in RECOVERING state, this is a successful reconnect.
func (s *Supervisor) RegisterConn(angelID string, conn *ipc.Conn) error {
	s.mu.RLock()
	entry, ok := s.angels[angelID]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("supervisor: unknown angel %s", angelID)
	}

	wasRecovering := entry.ConnTrack.MarkRecovered()
	if wasRecovering {
		s.log.Info("[Angel Lab] angel %s reconnected within recovery window", angelID)
	}

	entry.mu.Lock()
	entry.conn = conn
	entry.State = registry.StateTraining
	entry.mu.Unlock()

	_ = entry.ConnTrack.Transition(ConnStateRegistered)
	_ = s.reg.UpdateState(angelID, registry.StateTraining)

	s.log.Info("[Angel Lab] angel %s (%s) registered — pid %d",
		angelID, entry.AngelType, entry.PID)
	return nil
}

// HandleHeartbeat updates telemetry and, on the first heartbeat,
// transitions the angel from TRAINING to ACTIVE.
func (s *Supervisor) HandleHeartbeat(payload *ipc.HeartbeatPayload) {
	s.mu.RLock()
	entry, ok := s.angels[payload.AngelID]
	s.mu.RUnlock()
	if !ok {
		return
	}

	entry.mu.Lock()
	wasTraining := entry.State == registry.StateTraining
	entry.LastHeartbeat = time.Now()
	entry.Telemetry = payload
	entry.State = registry.StateActive
	entry.mu.Unlock()

	_ = s.reg.TouchLastSeen(payload.AngelID)

	if wasTraining {
		_ = entry.ConnTrack.Transition(ConnStateActive)
		_ = s.reg.UpdateState(payload.AngelID, registry.StateActive)
		s.log.Info("[Angel Lab] %s %s  TRAINING → ACTIVE  CPU %.1f%%  RSS %s  FD %d",
			titleCase(entry.AngelType),
			payload.AngelID,
			payload.CPUPercent,
			formatBytes(payload.RSSBytes),
			payload.FDCount,
		)
	}
}

// MarkConnLost is called by the server when an angel connection drops.
// It opens the recovery window; if the angel does not reconnect in time,
// the restart policy takes over.
func (s *Supervisor) MarkConnLost(angelID string) {
	s.mu.RLock()
	entry, ok := s.angels[angelID]
	s.mu.RUnlock()
	if !ok {
		return
	}

	s.log.Warn("[Angel Lab] angel %s lost connection — opening %s recovery window",
		angelID, recoveryWindow)

	entry.mu.Lock()
	entry.conn = nil
	entry.mu.Unlock()

	entry.ConnTrack.MarkLost(func() {
		// Recovery window expired without reconnect.
		s.log.Warn("[Angel Lab] angel %s recovery window expired — applying restart policy",
			angelID)
		// The process may still be alive (e.g. stuck loop, no IPC).
		// Kill it so runAngel's cmd.Wait() returns and triggers the restart policy.
		entry.mu.Lock()
		pid := entry.PID
		entry.mu.Unlock()
		if verifyAngelPID(pid) {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	})
}

// ---------------------------------------------------------------------------
// Heartbeat watcher
// ---------------------------------------------------------------------------

// Run is the supervisor's background loop. It checks heartbeat freshness
// on HeartbeatInterval and marks late angels UNSTABLE.
//
// Two-layer failure detection (as of Step 8):
//
//  Layer 1 — Angel side (fail-fast):
//    Each angel's sendHeartbeat() now returns an error.  If the heartbeat
//    Send fails twice in a row, run() returns and the angel process exits.
//    Lab detects the connection drop via Recv() returning an error, calls
//    MarkConnLost(), and the restart policy takes over.  This is the primary
//    failure path — it fires within one heartbeat interval.
//
//  Layer 2 — Lab side (safety net):
//    This loop handles the case where the angel process hangs without exiting
//    (e.g. stuck in a blocking syscall, not the heartbeat path) or where
//    an angel predates the Step 8 fix.  If no heartbeat arrives within
//    HeartbeatTimeout, the angel is moved to UNSTABLE.  MarkConnLost then
//    triggers SIGTERM after the recovery window.
func (s *Supervisor) Run(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.Supervisor.HeartbeatInterval.Duration)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkHeartbeats()
		}
	}
}

func (s *Supervisor) checkHeartbeats() {
	timeout := s.cfg.Supervisor.HeartbeatTimeout.Duration
	now := time.Now()

	s.mu.RLock()
	defer s.mu.RUnlock()

	for id, entry := range s.angels {
		entry.mu.Lock()
		state := entry.State
		last := entry.LastHeartbeat
		entry.mu.Unlock()

		if state != registry.StateActive {
			continue
		}
		if !last.IsZero() && now.Sub(last) > timeout {
			s.log.Warn("[Angel Lab] angel %s missed heartbeat (last seen %s ago)",
				id, now.Sub(last).Round(time.Second))
			entry.mu.Lock()
			entry.State = registry.StateUnstable
			entry.mu.Unlock()
			_ = s.reg.UpdateState(id, registry.StateUnstable)
		}
	}
}

// ---------------------------------------------------------------------------
// Termination
// ---------------------------------------------------------------------------

// TerminateAngel sends a graceful shutdown sequence to one angel.
func (s *Supervisor) TerminateAngel(id string) error {
	s.mu.RLock()
	entry, ok := s.angels[id]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("angel %s not found", id)
	}

	entry.mu.Lock()
	pid := entry.PID
	conn := entry.conn
	entry.mu.Unlock()

	// Prefer IPC terminate so the angel can flush state cleanly.
	if conn != nil {
		payload, _ := ipc.EncodePayload(nil)
		_ = conn.Send(&ipc.Message{
			Version: ipc.ProtocolVersion,
			Kind:    ipc.KindCmdTerminate,
			Payload: payload,
		})
	}

	// Also SIGTERM in case the IPC loop is blocked.
	if pid > 0 && verifyAngelPID(pid) {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}

	_ = s.reg.UpdateState(id, registry.StateTerminated)
	return nil
}

// TerminateAll sends SIGTERM to every non-terminated angel.
func (s *Supervisor) TerminateAll() {
	s.mu.RLock()
	ids := make([]string, 0, len(s.angels))
	for id, e := range s.angels {
		e.mu.Lock()
		if e.State != registry.StateTerminated && e.State != registry.StateContained {
			ids = append(ids, id)
		}
		e.mu.Unlock()
	}
	s.mu.RUnlock()

	for _, id := range ids {
		if err := s.TerminateAngel(id); err != nil {
			s.log.Warn("terminate %s: %v", id, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Query helpers
// ---------------------------------------------------------------------------

// GetEntry returns the in-memory AngelEntry for id.
func (s *Supervisor) GetEntry(id string) (*AngelEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.angels[id]
	return e, ok
}

// ListEntries returns a snapshot of all entries.
func (s *Supervisor) ListEntries() []*AngelEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*AngelEntry, 0, len(s.angels))
	for _, e := range s.angels {
		out = append(out, e)
	}
	return out
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// generateID produces the next available A-NN identifier.
func (s *Supervisor) generateID() string {
	s.mu.RLock()
	n := len(s.angels) + 1
	s.mu.RUnlock()
	for {
		id := fmt.Sprintf("A-%02d", n)
		s.mu.RLock()
		_, taken := s.angels[id]
		s.mu.RUnlock()
		if !taken {
			return id
		}
		n++
	}
}

// verifyAngelPID reads /proc/<pid>/cmdline and checks that it contains
// "angel" — the worker binary name.  This guards against PID recycling:
// if the OS has reassigned the PID to an unrelated process after a crash,
// we must not send it a signal.
func verifyAngelPID(pid int) bool {
	if pid <= 0 {
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		// ESRCH — process doesn't exist.
		return false
	}
	// /proc/<pid>/cmdline is NUL-delimited; check any argument contains "angel".
	return bytes.Contains(cmdline, []byte("angel"))
}

// formatBytes converts bytes to a human-readable string.
func formatBytes(b uint64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
	)
	switch {
	case b >= GiB:
		return fmt.Sprintf("%.1f GiB", float64(b)/GiB)
	case b >= MiB:
		return fmt.Sprintf("%.1f MiB", float64(b)/MiB)
	case b >= KiB:
		return fmt.Sprintf("%.1f KiB", float64(b)/KiB)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// titleCase capitalises the first letter of s.
func titleCase(s string) string {
	if len(s) == 0 {
		return s
	}
	if s[0] >= 'a' && s[0] <= 'z' {
		return string(s[0]-32) + s[1:]
	}
	return s
}
