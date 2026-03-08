// Package memory implements the Memory Angel.
//
// The Memory Angel monitors RSS memory usage for a configurable list of
// processes (by PID or name) and for the host as a whole via cgroup v2.
//
// Detection model:
//
//   - Per-process sliding window (last windowSize samples).
//     Growth rate = (newest RSS - oldest RSS) / oldest RSS.
//     If growth > GrowthWarnPct  → WARN event (once per cooldown).
//     If growth > GrowthCritPct  → CRITICAL event.
//
//   - Absolute threshold: RSS > AbsWarnMB → WARN.
//
//   - Cgroup v2 (optional): if CgroupPath is configured, watch
//     memory.current and memory.events (oom_kill counter).
//     OOM kill event → always CRITICAL.
//
// Heartbeat meta includes per-pid current RSS and growth% for lab inspect.
package memory

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nacreousdawn596/angellab/pkg/ipc"
	"github.com/nacreousdawn596/angellab/pkg/linux"
	"github.com/nacreousdawn596/angellab/pkg/logging"
)

// Run is the memory subcommand entry point called from cmd/angel/main.go.
func Run() {
	var (
		id        = flag.String("id", "", "angel ID assigned by Lab")
		labSocket = flag.String("lab", "/run/angellab/lab.sock", "path to lab.sock")
	)
	flag.Parse()

	if *id == "" {
		fmt.Fprintln(os.Stderr, "memory: --id is required")
		os.Exit(1)
	}

	log := logging.NewDefault(fmt.Sprintf("Memory/%s", *id))

	cfg, err := readMemConfig(os.Stdin)
	if err != nil {
		log.Crit("read config: %v", err)
		os.Exit(1)
	}

	m := &MemoryAngel{
		id:        *id,
		labSocket: *labSocket,
		cfg:       cfg,
		log:       log,
		startedAt: time.Now(),
		windows:   make(map[int]*rssWindow),
		alerted:   make(map[int]time.Time),
	}

	if err := m.run(context.Background()); err != nil {
		log.Crit("memory angel exited: %v", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// MemConfig is the memory angel-specific configuration.
type MemConfig struct {
	Type string `json:"type"`
	ID   string `json:"id"`

	// PIDs is a static list of process IDs to monitor.
	// If empty and WatchAll is false, the angel watches only itself.
	PIDs []int `json:"pids,omitempty"`

	// WatchAll enables monitoring of every visible process in /proc.
	// High overhead on large machines — use PIDs or ProcessNames instead.
	WatchAll bool `json:"watch_all,omitempty"`

	// ProcessNames is a list of process names (comm) to watch.
	// The angel resolves names to PIDs on each poll cycle.
	ProcessNames []string `json:"process_names,omitempty"`

	// PollInterval is how often /proc/<pid>/status is read.
	PollInterval time.Duration `json:"poll_interval"`

	// WindowSize is the number of samples kept per process for trend analysis.
	WindowSize int `json:"window_size"`

	// GrowthWarnPct is the RSS growth percentage that triggers a WARN event.
	// E.g. 50 means "warn when RSS grows by 50% within the window".
	GrowthWarnPct float64 `json:"growth_warn_pct"`

	// GrowthCritPct triggers a CRITICAL event.
	GrowthCritPct float64 `json:"growth_crit_pct"`

	// AbsWarnMB triggers a WARN event when any process exceeds this RSS (MB).
	AbsWarnMB uint64 `json:"abs_warn_mb"`

	// AbsCritMB triggers a CRITICAL event.
	AbsCritMB uint64 `json:"abs_crit_mb"`

	// AlertCooldown prevents repeated alerts for the same process.
	AlertCooldown time.Duration `json:"alert_cooldown"`

	// GrowthRateWarnKBps triggers a WARN when a process grows faster than
	// this many KB/s within the sliding window.  0 = disabled.
	// Example: 10240 = 10 MB/s sustained growth triggers WARN.
	GrowthRateWarnKBps float64 `json:"growth_rate_warn_kbps"`

	// GrowthRateCritKBps triggers a CRITICAL event.
	// Example: 102400 = 100 MB/s.
	GrowthRateCritKBps float64 `json:"growth_rate_crit_kbps"`

	// CgroupPath is the cgroup v2 directory to watch for host-level pressure.
	// Example: /sys/fs/cgroup/system.slice/angellab.service
	// Leave empty to disable cgroup monitoring.
	CgroupPath string `json:"cgroup_path,omitempty"`
}

func readMemConfig(r io.Reader) (*MemConfig, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &MemConfig{
		PollInterval:       5 * time.Second,
		WindowSize:         12,     // 12 samples × 5s = 1 minute trend window
		GrowthWarnPct:      50,
		GrowthCritPct:      200,
		AbsWarnMB:          512,
		AbsCritMB:          2048,
		AlertCooldown:      5 * time.Minute,
		GrowthRateWarnKBps: 10240,  // 10 MB/s sustained growth → WARN
		GrowthRateCritKBps: 102400, // 100 MB/s → CRITICAL
	}
	if len(data) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.WindowSize < 2 {
		cfg.WindowSize = 2
	}
	return cfg, nil
}

// ---------------------------------------------------------------------------
// Sliding window
// ---------------------------------------------------------------------------

// rssSample is one RSS measurement at a point in time.
type rssSample struct {
	ts    time.Time
	rssKB uint64
}

// rssWindow is a fixed-size circular buffer of rssSample.
type rssWindow struct {
	samples []rssSample
	head    int
	size    int
}

func newRSSWindow(cap int) *rssWindow {
	return &rssWindow{samples: make([]rssSample, cap)}
}

// add inserts a sample, overwriting the oldest if full.
func (w *rssWindow) add(s rssSample) {
	w.samples[w.head%len(w.samples)] = s
	w.head++
	if w.size < len(w.samples) {
		w.size++
	}
}

// full reports whether the window has been filled at least once.
func (w *rssWindow) full() bool { return w.size == len(w.samples) }

// oldest returns the oldest sample in the window.
func (w *rssWindow) oldest() rssSample {
	if w.size < len(w.samples) {
		return w.samples[0]
	}
	return w.samples[w.head%len(w.samples)]
}

// newest returns the most recently added sample.
func (w *rssWindow) newest() rssSample {
	if w.size == 0 {
		return rssSample{}
	}
	idx := (w.head - 1 + len(w.samples)) % len(w.samples)
	return w.samples[idx]
}

// growthPct returns the percentage growth from oldest to newest RSS.
// Returns 0 if the window is not full or oldest RSS is zero.
func (w *rssWindow) growthPct() float64 {
	if !w.full() {
		return 0
	}
	oldest := w.oldest().rssKB
	newest := w.newest().rssKB
	if oldest == 0 {
		return 0
	}
	return float64(newest-oldest) / float64(oldest) * 100.0
}

// growthRateKBps returns the RSS growth rate in KB/second over the window.
//
// Rate-based detection catches fast leaks (200 MB in 5s) that percentage-
// based thresholds miss when the baseline RSS is already large.  A process
// at 1 GiB RSS growing by 200 MB shows only 20% growth but a rate of
// 40 MB/s — clearly anomalous.
//
// Returns 0 if the window is not full, the elapsed time is zero, or RSS
// is shrinking (negative growth is not an alert condition).
func (w *rssWindow) growthRateKBps() float64 {
	if !w.full() {
		return 0
	}
	oldest := w.oldest()
	newest := w.newest()
	if newest.rssKB <= oldest.rssKB {
		return 0 // shrinking or flat
	}
	elapsed := newest.ts.Sub(oldest.ts).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return float64(newest.rssKB-oldest.rssKB) / elapsed
}

// ---------------------------------------------------------------------------
// MemoryAngel
// ---------------------------------------------------------------------------

// MemoryAngel is the Memory Angel implementation.
type MemoryAngel struct {
	id        string
	labSocket string
	cfg       *MemConfig
	log       *logging.Logger
	startedAt time.Time

	conn       *ipc.Conn
	cpuSampler *linux.CPUSampler

	// windows maps PID → sliding window.
	windows map[int]*rssWindow
	// alerted maps PID → time of last alert (for cooldown).
	alerted map[int]time.Time

	// Cgroup tracking.
	lastOOMKills uint64

	// Summary stats for heartbeat.
	trackedCount int
	totalAlerts  int
}

// ---------------------------------------------------------------------------
// Main loop
// ---------------------------------------------------------------------------

func (m *MemoryAngel) run(ctx context.Context) error {
	conn, err := ipc.Dial(m.labSocket, ipc.RoleAngel)
	if err != nil {
		return fmt.Errorf("dial lab: %w", err)
	}
	m.conn = conn
	defer conn.Close()

	if err := m.register(); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	m.cpuSampler, _ = linux.NewCPUSampler()

	pollTick      := time.NewTicker(m.cfg.PollInterval)
	heartbeatTick := time.NewTicker(10 * time.Second)
	defer pollTick.Stop()
	defer heartbeatTick.Stop()

	m.log.Info("[Angel Lab] Memory %s ACTIVE — poll %s, window %d samples",
		m.id, m.cfg.PollInterval, m.cfg.WindowSize)

	if err := m.sendHeartbeat(); err != nil {
		// First heartbeat failed — Lab is not reachable.
		return fmt.Errorf("initial heartbeat: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-pollTick.C:
			m.poll()
		case <-heartbeatTick.C:
			if err := m.sendHeartbeat(); err != nil {
				// First attempt failed — try once more before exiting.
				// This filters transient socket hiccups from genuine Lab death.
				if retry := m.sendHeartbeat(); retry != nil {
					return fmt.Errorf("heartbeat failed (2 attempts): %w", retry)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Poll cycle
// ---------------------------------------------------------------------------

func (m *MemoryAngel) poll() {
	pids := m.resolvePIDs()
	m.trackedCount = len(pids)

	for _, pid := range pids {
		status, err := linux.ReadProcStatus(pid)
		if err != nil {
			// Process may have exited between resolution and read — expected.
			delete(m.windows, pid)
			continue
		}

		w, ok := m.windows[pid]
		if !ok {
			w = newRSSWindow(m.cfg.WindowSize)
			m.windows[pid] = w
		}
		w.add(rssSample{ts: time.Now(), rssKB: status.VmRSS})

		m.checkThresholds(pid, status)
	}

	// Prune windows for PIDs no longer in our target set.
	pidSet := make(map[int]struct{}, len(pids))
	for _, p := range pids {
		pidSet[p] = struct{}{}
	}
	for pid := range m.windows {
		if _, ok := pidSet[pid]; !ok {
			delete(m.windows, pid)
			delete(m.alerted, pid)
		}
	}

	// Cgroup v2 check.
	if m.cfg.CgroupPath != "" {
		m.checkCgroup()
	}
}

// checkThresholds evaluates one process against all configured rules.
func (m *MemoryAngel) checkThresholds(pid int, status *linux.ProcStatus) {
	w := m.windows[pid]
	rssKB := status.VmRSS
	rssMB := rssKB / 1024

	// Check alert cooldown.
	if last, ok := m.alerted[pid]; ok {
		if time.Since(last) < m.cfg.AlertCooldown {
			return
		}
	}

	// --- Absolute threshold ---
	if m.cfg.AbsCritMB > 0 && rssMB >= m.cfg.AbsCritMB {
		msg := fmt.Sprintf("%s (pid %d) RSS %d MiB exceeds critical threshold %d MiB",
			status.Name, pid, rssMB, m.cfg.AbsCritMB)
		m.log.Crit("[Angel Lab] Memory %s %s", m.id, msg)
		m.emitEvent(ipc.SeverityCritical, msg, map[string]string{
			"pid": strconv.Itoa(pid), "process": status.Name,
			"rss_mb": strconv.FormatUint(rssMB, 10),
			"rule":   "abs_crit",
		})
		m.alerted[pid] = time.Now()
		m.totalAlerts++
		return
	}

	if m.cfg.AbsWarnMB > 0 && rssMB >= m.cfg.AbsWarnMB {
		msg := fmt.Sprintf("%s (pid %d) RSS %d MiB exceeds warn threshold %d MiB",
			status.Name, pid, rssMB, m.cfg.AbsWarnMB)
		m.log.Warn("[Angel Lab] Memory %s %s", m.id, msg)
		m.emitEvent(ipc.SeverityWarn, msg, map[string]string{
			"pid": strconv.Itoa(pid), "process": status.Name,
			"rss_mb": strconv.FormatUint(rssMB, 10),
			"rule":   "abs_warn",
		})
		m.alerted[pid] = time.Now()
		m.totalAlerts++
		return
	}

	// --- Growth threshold (requires a full window) ---
	if !w.full() {
		return
	}

	growth := w.growthPct()
	if growth <= 0 {
		return // shrinking or flat — fine
	}

	windowDuration := w.newest().ts.Sub(w.oldest().ts).Round(time.Second)

	if m.cfg.GrowthCritPct > 0 && growth >= m.cfg.GrowthCritPct {
		msg := fmt.Sprintf("%s (pid %d) RSS grew %.0f%% over %s (now %d MiB)",
			status.Name, pid, growth, windowDuration, rssMB)
		m.log.Crit("[Angel Lab] Memory %s %s", m.id, msg)
		m.emitEvent(ipc.SeverityCritical, msg, map[string]string{
			"pid": strconv.Itoa(pid), "process": status.Name,
			"rss_mb":      strconv.FormatUint(rssMB, 10),
			"growth_pct":  fmt.Sprintf("%.1f", growth),
			"window_secs": strconv.Itoa(int(windowDuration.Seconds())),
			"rule":        "growth_crit",
		})
		m.alerted[pid] = time.Now()
		m.totalAlerts++
	} else if m.cfg.GrowthWarnPct > 0 && growth >= m.cfg.GrowthWarnPct {
		msg := fmt.Sprintf("%s (pid %d) RSS grew %.0f%% over %s (now %d MiB)",
			status.Name, pid, growth, windowDuration, rssMB)
		m.log.Warn("[Angel Lab] Memory %s %s", m.id, msg)
		m.emitEvent(ipc.SeverityWarn, msg, map[string]string{
			"pid": strconv.Itoa(pid), "process": status.Name,
			"rss_mb":      strconv.FormatUint(rssMB, 10),
			"growth_pct":  fmt.Sprintf("%.1f", growth),
			"window_secs": strconv.Itoa(int(windowDuration.Seconds())),
			"rule":        "growth_warn",
		})
		m.alerted[pid] = time.Now()
		m.totalAlerts++
		return
	}

	// --- Growth rate threshold (ΔRSS/Δtime, catches fast leaks) ---
	// Evaluated after % thresholds so a fast AND large leak only emits once.
	rate := w.growthRateKBps()
	if rate > 0 {
		rateMBps := rate / 1024.0
		if m.cfg.GrowthRateCritKBps > 0 && rate >= m.cfg.GrowthRateCritKBps {
			msg := fmt.Sprintf("%s (pid %d) RSS growing at %.1f MB/s (now %d MiB) — possible leak",
				status.Name, pid, rateMBps, rssMB)
			m.log.Crit("[Angel Lab] Memory %s %s", m.id, msg)
			m.emitEvent(ipc.SeverityCritical, msg, map[string]string{
				"pid": strconv.Itoa(pid), "process": status.Name,
				"rss_mb":       strconv.FormatUint(rssMB, 10),
				"rate_mb_per_s": fmt.Sprintf("%.2f", rateMBps),
				"rule":          "rate_crit",
			})
			m.alerted[pid] = time.Now()
			m.totalAlerts++
		} else if m.cfg.GrowthRateWarnKBps > 0 && rate >= m.cfg.GrowthRateWarnKBps {
			msg := fmt.Sprintf("%s (pid %d) RSS growing at %.1f MB/s (now %d MiB)",
				status.Name, pid, rateMBps, rssMB)
			m.log.Warn("[Angel Lab] Memory %s %s", m.id, msg)
			m.emitEvent(ipc.SeverityWarn, msg, map[string]string{
				"pid": strconv.Itoa(pid), "process": status.Name,
				"rss_mb":        strconv.FormatUint(rssMB, 10),
				"rate_mb_per_s": fmt.Sprintf("%.2f", rateMBps),
				"rule":          "rate_warn",
			})
			m.alerted[pid] = time.Now()
			m.totalAlerts++
		}
	}
}

// checkCgroup reads cgroup v2 memory stats and alerts on OOM kill events.
func (m *MemoryAngel) checkCgroup() {
	cg, err := linux.ReadCgroupMemory(m.cfg.CgroupPath)
	if err != nil {
		return
	}
	if cg.OOMKills > m.lastOOMKills {
		delta := cg.OOMKills - m.lastOOMKills
		msg := fmt.Sprintf("cgroup OOM killed %d process(es) — current memory %s",
			delta, formatMB(cg.Current))
		m.log.Crit("[Angel Lab] Memory %s %s", m.id, msg)
		m.emitEvent(ipc.SeverityCritical, msg, map[string]string{
			"oom_kills":   strconv.FormatUint(delta, 10),
			"cgroup_path": m.cfg.CgroupPath,
			"memory_bytes": strconv.FormatUint(cg.Current, 10),
		})
		m.totalAlerts++
	}
	m.lastOOMKills = cg.OOMKills
}

// ---------------------------------------------------------------------------
// PID resolution
// ---------------------------------------------------------------------------

// resolvePIDs builds the current set of PIDs to monitor.
func (m *MemoryAngel) resolvePIDs() []int {
	seen := make(map[int]struct{})

	// Static PIDs from config.
	for _, pid := range m.cfg.PIDs {
		seen[pid] = struct{}{}
	}

	// ProcessNames: scan /proc for matching comm values.
	if len(m.cfg.ProcessNames) > 0 {
		nameSet := make(map[string]struct{}, len(m.cfg.ProcessNames))
		for _, n := range m.cfg.ProcessNames {
			nameSet[strings.ToLower(n)] = struct{}{}
		}
		forEachPID(func(pid int, comm string) {
			if _, ok := nameSet[strings.ToLower(comm)]; ok {
				seen[pid] = struct{}{}
			}
		})
	}

	// WatchAll: monitor every visible process.
	if m.cfg.WatchAll {
		forEachPID(func(pid int, _ string) {
			seen[pid] = struct{}{}
		})
	}

	// Default: watch at least our own process.
	if len(seen) == 0 {
		seen[os.Getpid()] = struct{}{}
	}

	pids := make([]int, 0, len(seen))
	for pid := range seen {
		pids = append(pids, pid)
	}
	return pids
}

// forEachPID calls fn(pid, comm) for every readable process in /proc.
func forEachPID(fn func(pid int, comm string)) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		commBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if err != nil {
			continue
		}
		fn(pid, strings.TrimSpace(string(commBytes)))
	}
}

// ---------------------------------------------------------------------------
// IPC helpers
// ---------------------------------------------------------------------------

func (m *MemoryAngel) register() error {
	payload, err := ipc.EncodePayload(&ipc.RegisterPayload{
		AngelID:   m.id,
		AngelType: "memory",
		PID:       os.Getpid(),
	})
	if err != nil {
		return err
	}
	return m.conn.Send(&ipc.Message{
		Version: ipc.ProtocolVersion,
		Kind:    ipc.KindRegister,
		Payload: payload,
	})
}

func (m *MemoryAngel) sendHeartbeat() error {
	stat, _ := linux.ReadSelfStat()
	var selfRSS uint64
	if stat != nil {
		selfRSS = stat.RSSBytes()
	}
	var cpu float64
	if m.cpuSampler != nil {
		cpu, _ = m.cpuSampler.Sample()
	}

	meta := map[string]string{
		"tracked_pids": strconv.Itoa(m.trackedCount),
		"total_alerts": strconv.Itoa(m.totalAlerts),
	}
	// Include current RSS for each tracked PID (up to 8 to keep meta compact).
	count := 0
	for pid, w := range m.windows {
		if count >= 8 {
			break
		}
		if w.size > 0 {
			newest := w.newest()
			key := fmt.Sprintf("pid_%d_rss_mb", pid)
			meta[key] = strconv.FormatUint(newest.rssKB/1024, 10)
		}
		count++
	}

	payload, err := ipc.EncodePayload(&ipc.HeartbeatPayload{
		AngelID:    m.id,
		State:      "ACTIVE",
		Uptime:     int64(time.Since(m.startedAt).Seconds()),
		CPUPercent: cpu,
		RSSBytes:   selfRSS,
		Goroutines: runtime.NumGoroutine(),
		FDCount:    linux.CountFDs(),
		AngelMeta:  meta,
	})
	if err != nil {
		return fmt.Errorf("heartbeat: encode: %w", err)
	}
	if err := m.conn.Send(&ipc.Message{
		Version: ipc.ProtocolVersion,
		Kind:    ipc.KindHeartbeat,
		Payload: payload,
	}); err != nil {
		return err
	}
	return nil
}

func (m *MemoryAngel) emitEvent(severity ipc.Severity, message string, meta map[string]string) {
	payload, err := ipc.EncodePayload(&ipc.EventPayload{
		AngelID:   m.id,
		Severity:  severity,
		Message:   message,
		Timestamp: time.Now(),
		Meta:      meta,
	})
	if err != nil {
		m.log.Warn("encode event: %v", err)
		return
	}

	if err := m.conn.Send(&ipc.Message{
		Version: ipc.ProtocolVersion,
		Kind:    ipc.KindEvent,
		Payload: payload,
	}); err != nil {
		m.log.Warn("send event: %v", err)
	}
}

// formatMB returns a human-readable memory string (bytes → MiB).
func formatMB(bytes uint64) string {
	return fmt.Sprintf("%d MiB", bytes>>20)
}
