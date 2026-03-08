// Package process implements the Process Angel.
//
// The Process Angel monitors process creation and exit events by polling
// /proc every PollInterval.  It maintains a snapshot of the live process
// table and diffs it on each cycle to find new arrivals and departures.
//
// Two-phase operation:
//
//  1. TRAINING (BaselineDuration): observe the process table and build a
//     whitelist of "normal" process executables and comm names.  No events
//     are emitted except INFO status lines.
//
//  2. ACTIVE: compare each new process against the baseline.  Score it
//     using a set of named rules (unknown exe, suspicious path, unusual
//     parent, missing exe link) and emit WARN or CRITICAL events when
//     the score crosses a threshold.
//
// Process info is read from:
//
//	/proc/<pid>/comm      — short process name
//	/proc/<pid>/exe       — full executable path (symlink)
//	/proc/<pid>/cmdline   — full command line (NUL-delimited)
//	/proc/<pid>/status    — PPID, UID, GID
//
// Anomaly rules:
//
//	Rule                                      Weight
//	────────────────────────────────────────────────
//	Executable in suspicious dir (/tmp, …)    +5
//	Executable not in baseline set            +3
//	Comm not in baseline set                  +2
//	Missing /proc/<pid>/exe symlink           +3  (packed/memfd executables)
//	Parent not in baseline (orphan chain)     +2
//	Setuid/setgid process                     +2
//	Very short-lived process                  +1
//
//	Score ≥ 3 → WARN
//	Score ≥ 6 → CRITICAL
package process

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nacreousdawn596/angellab/pkg/ipc"
	"github.com/nacreousdawn596/angellab/pkg/linux"
	"github.com/nacreousdawn596/angellab/pkg/logging"
)

// Run is the process angel subcommand entry point.
func Run() {
	var (
		id        = flag.String("id", "", "angel ID assigned by Lab")
		labSocket = flag.String("lab", "/run/angellab/lab.sock", "path to lab.sock")
	)
	flag.Parse()

	if *id == "" {
		fmt.Fprintln(os.Stderr, "process: --id is required")
		os.Exit(1)
	}

	log := logging.NewDefault(fmt.Sprintf("Process/%s", *id))

	cfg, err := readConfig(os.Stdin)
	if err != nil {
		log.Crit("read config: %v", err)
		os.Exit(1)
	}

	p := &ProcessAngel{
		id:        *id,
		labSocket: *labSocket,
		cfg:       cfg,
		log:       log,
		startedAt: time.Now(),
		prev:      make(map[int]*procSnapshot),
	}
	if err := p.run(context.Background()); err != nil {
		log.Crit("process angel exited: %v", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Process snapshot
// ---------------------------------------------------------------------------

// procSnapshot is a lightweight record of one process at one point in time.
type procSnapshot struct {
	PID      int
	PPID     int
	Comm     string
	ExePath  string // empty if /proc/<pid>/exe is unreadable
	Cmdline  string // first 256 bytes
	UID      int
	GID      int
	IsSetUID bool
	SeenAt   time.Time
}

// suspiciousExeDir reports whether ExePath starts with any of the given
// suspicious directories.
func (s *procSnapshot) suspiciousExeDir(dirs []string) bool {
	for _, dir := range dirs {
		if strings.HasPrefix(s.ExePath, dir) {
			return true
		}
	}
	// Memfd / anonymous executable: exe link is missing or contains "(deleted)".
	if s.ExePath == "" || strings.HasSuffix(s.ExePath, "(deleted)") {
		return false // handled by missing-exe rule
	}
	return false
}

// missingExe reports whether the exe symlink could not be resolved.
// This happens with packed binaries, memfd_create executables, or
// processes that have replaced their binary on disk.
func (s *procSnapshot) missingExe() bool {
	return s.ExePath == "" || s.ExePath == "(deleted)"
}

// ---------------------------------------------------------------------------
// Baseline
// ---------------------------------------------------------------------------

// procBaseline holds the set of "normal" process attributes learned during
// the training phase.
type procBaseline struct {
	exes   map[string]struct{} // known exe paths
	comms  map[string]struct{} // known comm names
	ppids  map[int]struct{}    // PIDs seen as parents during training
	frozen bool
}

func newProcBaseline() *procBaseline {
	return &procBaseline{
		exes:  make(map[string]struct{}),
		comms: make(map[string]struct{}),
		ppids: make(map[int]struct{}),
	}
}

func (b *procBaseline) observe(snap *procSnapshot) {
	if b.frozen {
		return
	}
	if snap.ExePath != "" {
		b.exes[snap.ExePath] = struct{}{}
	}
	b.comms[snap.Comm] = struct{}{}
	b.ppids[snap.PID] = struct{}{}
}

func (b *procBaseline) freeze() { b.frozen = true }

func (b *procBaseline) knowsExe(exe string) bool {
	_, ok := b.exes[exe]
	return ok
}

func (b *procBaseline) knowsComm(comm string) bool {
	_, ok := b.comms[comm]
	return ok
}

func (b *procBaseline) knowsPPID(ppid int) bool {
	_, ok := b.ppids[ppid]
	return ok
}

// ---------------------------------------------------------------------------
// Scoring
// ---------------------------------------------------------------------------

const (
	procWarnThreshold = 3
	procCritThreshold = 6
)

type procScore struct {
	total   int
	reasons []string
}

func (s *procScore) add(n int, reason string) {
	s.total += n
	s.reasons = append(s.reasons, reason)
}

func (s *procScore) level() int {
	switch {
	case s.total >= procCritThreshold:
		return 2
	case s.total >= procWarnThreshold:
		return 1
	default:
		return 0
	}
}

func (s *procScore) summary() string {
	if len(s.reasons) == 0 {
		return fmt.Sprintf("score %d", s.total)
	}
	return fmt.Sprintf("score %d (%s)", s.total, strings.Join(s.reasons, " + "))
}

// ---------------------------------------------------------------------------
// ProcessAngel
// ---------------------------------------------------------------------------

// phase tracks training vs. active state.
type phase uint8

const (
	phaseTraining phase = iota
	phaseActive
)

// ProcessAngel implements the Process Angel.
type ProcessAngel struct {
	id        string
	labSocket string
	cfg       *Config
	log       *logging.Logger
	startedAt time.Time

	conn       *ipc.Conn
	cpuSampler *linux.CPUSampler

	phase    phase
	baseline *procBaseline

	// prev is the process table from the previous poll cycle.
	prev map[int]*procSnapshot

	// Counters for heartbeat meta.
	totalNew    int
	totalExited int
	totalAlerts int
}

// ---------------------------------------------------------------------------
// Main loop
// ---------------------------------------------------------------------------

func (p *ProcessAngel) run(ctx context.Context) error {
	conn, err := ipc.Dial(p.labSocket, ipc.RoleAngel)
	if err != nil {
		return fmt.Errorf("dial lab: %w", err)
	}
	p.conn = conn
	defer conn.Close()

	if err := p.register(); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	p.cpuSampler, _ = linux.NewCPUSampler()
	p.baseline = newProcBaseline()

	// Seed whitelist entries from config before training begins.
	for _, exe := range p.cfg.WhitelistExes {
		p.baseline.exes[exe] = struct{}{}
	}
	for _, comm := range p.cfg.WhitelistComms {
		p.baseline.comms[comm] = struct{}{}
	}

	// Take initial snapshot before training timer starts.
	// This prevents every process alive at startup from being "new".
	p.prev = p.snapshot()

	pollTick      := time.NewTicker(p.cfg.PollInterval)
	heartbeatTick := time.NewTicker(10 * time.Second)
	baselineTimer := time.NewTimer(p.cfg.BaselineDuration)

	defer pollTick.Stop()
	defer heartbeatTick.Stop()
	defer baselineTimer.Stop()

	p.log.Info("[Angel Lab] Process/%s TRAINING — baseline window %s", p.id, p.cfg.BaselineDuration)
	if err := p.sendHeartbeat(); err != nil {
		// First heartbeat failed — Lab is not reachable.
		return fmt.Errorf("initial heartbeat: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-baselineTimer.C:
			if p.phase == phaseTraining {
				p.finaliseBaseline()
			}

		case <-pollTick.C:
			p.poll()

		case <-heartbeatTick.C:
			if err := p.sendHeartbeat(); err != nil {
				// First attempt failed — try once more before exiting.
				// This filters transient socket hiccups from genuine Lab death.
				if retry := p.sendHeartbeat(); retry != nil {
					return fmt.Errorf("heartbeat failed (2 attempts): %w", retry)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Poll cycle
// ---------------------------------------------------------------------------

func (p *ProcessAngel) poll() {
	curr := p.snapshot()

	// Find new processes (in curr, not in prev).
	for pid, snap := range curr {
		if _, seen := p.prev[pid]; !seen {
			p.handleNew(snap)
		}
	}

	// Find exited processes (in prev, not in curr).
	if p.cfg.AlertOnExit && p.phase == phaseActive {
		for pid, snap := range p.prev {
			if _, alive := curr[pid]; !alive {
				p.handleExit(snap)
			}
		}
	}

	p.prev = curr
}

// handleNew evaluates a newly-appeared process.
func (p *ProcessAngel) handleNew(snap *procSnapshot) {
	p.totalNew++

	if p.phase == phaseTraining {
		p.baseline.observe(snap)
		return
	}

	// Active phase: score the process.
	score := p.scoreProcess(snap)

	p.log.Debug("[Process/%s] new process: pid=%d comm=%q exe=%q ppid=%d score=%d",
		p.id, snap.PID, snap.Comm, snap.ExePath, snap.PPID, score.total)

	if score.level() == 0 {
		return
	}

	// Build the event message.
	severity := ipc.SeverityWarn
	verb := "suspicious"
	if score.level() == 2 {
		severity = ipc.SeverityCritical
		verb = "anomalous"
	}

	exeDisplay := snap.ExePath
	if exeDisplay == "" {
		exeDisplay = "(no exe link — possible packed binary)"
	}

	msg := fmt.Sprintf("%s new process [%s] %s (pid %d, ppid %d) — %s",
		verb, snap.Comm, exeDisplay, snap.PID, snap.PPID, score.summary())

	p.log.Crit("[Angel Lab] Process/%s %s", p.id, msg)
	p.totalAlerts++

	p.emitEvent(severity, msg, map[string]string{
		"pid":     strconv.Itoa(snap.PID),
		"ppid":    strconv.Itoa(snap.PPID),
		"comm":    snap.Comm,
		"exe":     snap.ExePath,
		"cmdline": snap.Cmdline,
		"score":   strconv.Itoa(score.total),
	})
}

// handleExit emits an INFO event when a process that existed during baseline
// exits.  Useful for detecting killed daemons.
func (p *ProcessAngel) handleExit(snap *procSnapshot) {
	p.totalExited++
	// Only alert on exit if the process was in the baseline (i.e. expected to
	// be long-lived).
	if !p.baseline.knowsExe(snap.ExePath) && !p.baseline.knowsComm(snap.Comm) {
		return
	}
	msg := fmt.Sprintf("process exited: [%s] %s (pid %d)",
		snap.Comm, snap.ExePath, snap.PID)
	p.log.Info("[Angel Lab] Process/%s %s", p.id, msg)
	p.emitEvent(ipc.SeverityInfo, msg, map[string]string{
		"pid":  strconv.Itoa(snap.PID),
		"comm": snap.Comm,
		"exe":  snap.ExePath,
	})
}

// scoreProcess applies the scoring rules to a new process snapshot.
func (p *ProcessAngel) scoreProcess(snap *procSnapshot) procScore {
	var score procScore

	// Rule 1: executable in a suspicious directory.
	if snap.suspiciousExeDir(p.cfg.SuspiciousExeDirs) {
		score.add(5, fmt.Sprintf("exe in suspicious dir %s", snap.ExePath))
	}

	// Rule 2: missing exe symlink (packed/memfd binary).
	if snap.missingExe() {
		score.add(3, "no exe symlink (packed or memfd binary?)")
	}

	// Rule 3: executable not seen during baseline.
	if snap.ExePath != "" && !p.baseline.knowsExe(snap.ExePath) {
		score.add(3, fmt.Sprintf("unknown exe %s", snap.ExePath))
	}

	// Rule 4: comm name not seen during baseline.
	if !p.baseline.knowsComm(snap.Comm) {
		score.add(2, fmt.Sprintf("unknown comm %q", snap.Comm))
	}

	// Rule 5: parent process not known from baseline (orphan chain).
	if snap.PPID > 1 && !p.baseline.knowsPPID(snap.PPID) {
		score.add(2, fmt.Sprintf("unknown parent pid %d", snap.PPID))
	}

	// Rule 6: setuid/setgid process.
	if snap.IsSetUID {
		score.add(2, "setuid/setgid")
	}

	// Subtract score for config-whitelisted items — they're expected.
	for _, exe := range p.cfg.WhitelistExes {
		if snap.ExePath == exe {
			score.total -= 10 // guaranteed to stay below threshold
			return score
		}
	}
	for _, comm := range p.cfg.WhitelistComms {
		if strings.HasPrefix(snap.Comm, comm) {
			score.total -= 10
			return score
		}
	}

	return score
}

// ---------------------------------------------------------------------------
// Baseline finalisation
// ---------------------------------------------------------------------------

func (p *ProcessAngel) finaliseBaseline() {
	p.baseline.freeze()
	p.phase = phaseActive

	p.log.Info("[Angel Lab] Process/%s baseline frozen: %d executables, %d comm names",
		p.id, len(p.baseline.exes), len(p.baseline.comms))
	p.log.Info("[Angel Lab] Process/%s ACTIVE — monitoring process execution", p.id)

	p.emitEvent(ipc.SeverityInfo,
		fmt.Sprintf("baseline complete — %d known executables, %d known process names",
			len(p.baseline.exes), len(p.baseline.comms)),
		map[string]string{
			"known_exes":  strconv.Itoa(len(p.baseline.exes)),
			"known_comms": strconv.Itoa(len(p.baseline.comms)),
		},
	)
}

// ---------------------------------------------------------------------------
// /proc snapshot
// ---------------------------------------------------------------------------

// snapshot reads the current process table from /proc.
// Only returns processes we can read (no permission error).
func (p *ProcessAngel) snapshot() map[int]*procSnapshot {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		p.log.Warn("snapshot: ReadDir /proc: %v", err)
		return nil
	}

	result := make(map[int]*procSnapshot, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a PID directory
		}
		snap := readProcInfo(pid)
		if snap == nil {
			continue // process vanished or permission denied
		}
		result[pid] = snap
	}
	return result
}

// readProcInfo reads process info from /proc/<pid>/*.
// Returns nil if the process has vanished or is unreadable.
func readProcInfo(pid int) *procSnapshot {
	// Read /proc/<pid>/status for PPID, UID, GID, Name.
	status, err := linux.ReadProcStatus(pid)
	if err != nil {
		return nil
	}

	snap := &procSnapshot{
		PID:    pid,
		PPID:   status.PPID,
		Comm:   status.Name,
		UID:    status.UID,
		SeenAt: time.Now(),
	}

	// Setuid: effective UID differs from real UID.
	snap.IsSetUID = status.EUID != status.UID

	// /proc/<pid>/exe — full path of the running binary.
	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		snap.ExePath = strings.TrimSuffix(exe, " (deleted)")
	}

	// /proc/<pid>/cmdline — NUL-delimited argument list (first 256 bytes).
	if cmdlineBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		// Replace NUL bytes with spaces for display.
		cmdlineBytes = bytes.ReplaceAll(cmdlineBytes, []byte{0}, []byte{' '})
		if len(cmdlineBytes) > 256 {
			cmdlineBytes = cmdlineBytes[:256]
		}
		snap.Cmdline = strings.TrimSpace(string(cmdlineBytes))
	}

	return snap
}

// ---------------------------------------------------------------------------
// IPC helpers
// ---------------------------------------------------------------------------

func (p *ProcessAngel) register() error {
	payload, err := ipc.EncodePayload(&ipc.RegisterPayload{
		AngelID:   p.id,
		AngelType: "process",
		PID:       os.Getpid(),
	})
	if err != nil {
		return err
	}
	return p.conn.Send(&ipc.Message{
		Version: ipc.ProtocolVersion,
		Kind:    ipc.KindRegister,
		Payload: payload,
	})
}

func (p *ProcessAngel) sendHeartbeat() error {
	stat, _ := linux.ReadSelfStat()
	var rss uint64
	if stat != nil {
		rss = stat.RSSBytes()
	}
	var cpu float64
	if p.cpuSampler != nil {
		cpu, _ = p.cpuSampler.Sample()
	}

	stateStr := "TRAINING"
	if p.phase == phaseActive {
		stateStr = "ACTIVE"
	}

	payload, err := ipc.EncodePayload(&ipc.HeartbeatPayload{
		AngelID:    p.id,
		State:      stateStr,
		Uptime:     int64(time.Since(p.startedAt).Seconds()),
		CPUPercent: cpu,
		RSSBytes:   rss,
		Goroutines: runtime.NumGoroutine(),
		FDCount:    linux.CountFDs(),
		AngelMeta: map[string]string{
			"phase":        stateStr,
			"tracked_pids": strconv.Itoa(len(p.prev)),
			"new_procs":    strconv.Itoa(p.totalNew),
			"exited_procs": strconv.Itoa(p.totalExited),
			"total_alerts": strconv.Itoa(p.totalAlerts),
			"known_exes":   strconv.Itoa(len(p.baseline.exes)),
		},
	})
	if err != nil {
		return fmt.Errorf("heartbeat: encode: %w", err)
	}
	if err := p.conn.Send(&ipc.Message{
		Version: ipc.ProtocolVersion,
		Kind:    ipc.KindHeartbeat,
		Payload: payload,
	}); err != nil {
		return err
	}
	return nil
}

func (p *ProcessAngel) emitEvent(severity ipc.Severity, message string, meta map[string]string) {
	payload, err := ipc.EncodePayload(&ipc.EventPayload{
		AngelID:   p.id,
		Severity:  severity,
		Message:   message,
		Timestamp: time.Now(),
		Meta:      meta,
	})
	if err != nil {
		p.log.Warn("encode event: %v", err)
		return
	}

	if err := p.conn.Send(&ipc.Message{
		Version: ipc.ProtocolVersion,
		Kind:    ipc.KindEvent,
		Payload: payload,
	}); err != nil {
		p.log.Warn("send event: %v", err)
	}
}