// Package linux — proc.go
//
// Helpers for reading process and system telemetry from /proc.
// Used by HeartbeatPayload collection and the Memory Angel.
package linux

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// ProcStat — /proc/<pid>/stat
// ---------------------------------------------------------------------------

// ProcStat holds fields parsed from /proc/<pid>/stat.
// Field numbers follow the kernel documentation (proc(5)).
type ProcStat struct {
	PID        int
	Comm       string // executable name (field 2)
	State      byte   // R, S, D, Z, T, …
	UserTime   uint64 // utime: user-mode CPU ticks
	SystemTime uint64 // stime: kernel-mode CPU ticks
	StartTime  uint64 // starttime: ticks since boot
	VSize      uint64 // virtual memory size in bytes
	RSS        int64  // resident set size in pages
}

// RSSBytes returns the resident set size in bytes using the system page size.
func (s *ProcStat) RSSBytes() uint64 {
	pageSize := uint64(os.Getpagesize())
	if s.RSS < 0 {
		return 0
	}
	return uint64(s.RSS) * pageSize
}

// ReadProcStat reads /proc/<pid>/stat and returns a ProcStat.
func ReadProcStat(pid int) (*ProcStat, error) {
	path := fmt.Sprintf("/proc/%d/stat", pid)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("proc: read %s: %w", path, err)
	}
	return parseProcStat(string(data))
}

// ReadSelfStat reads /proc/self/stat for the calling process.
func ReadSelfStat() (*ProcStat, error) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return nil, fmt.Errorf("proc: read /proc/self/stat: %w", err)
	}
	return parseProcStat(string(data))
}

// parseProcStat handles the awkward /proc/stat format where field 2 (comm)
// can contain spaces and is delimited by parentheses.
func parseProcStat(raw string) (*ProcStat, error) {
	// Find the comm field boundaries: "1234 (my prog) S …"
	lparen := strings.Index(raw, "(")
	rparen := strings.LastIndex(raw, ")")
	if lparen == -1 || rparen == -1 {
		return nil, fmt.Errorf("proc: malformed stat line")
	}

	pidStr := strings.TrimSpace(raw[:lparen])
	comm := raw[lparen+1 : rparen]
	rest := strings.Fields(raw[rparen+1:])
	// rest[0] is field 3 (state), rest[10] is field 13 (utime), etc.
	// Fields are 1-indexed in the man page; rest is 0-indexed starting at field 3.
	if len(rest) < 22 {
		return nil, fmt.Errorf("proc: too few fields in stat")
	}

	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return nil, fmt.Errorf("proc: parse pid: %w", err)
	}

	utime, _ := strconv.ParseUint(rest[11], 10, 64)  // field 14 → rest[11]
	stime, _ := strconv.ParseUint(rest[12], 10, 64)  // field 15 → rest[12]
	starttime, _ := strconv.ParseUint(rest[19], 10, 64) // field 22 → rest[19]
	vsize, _ := strconv.ParseUint(rest[20], 10, 64)  // field 23 → rest[20]
	rss, _ := strconv.ParseInt(rest[21], 10, 64)     // field 24 → rest[21]

	return &ProcStat{
		PID:        pid,
		Comm:       comm,
		State:      rest[0][0],
		UserTime:   utime,
		SystemTime: stime,
		StartTime:  starttime,
		VSize:      vsize,
		RSS:        rss,
	}, nil
}

// ---------------------------------------------------------------------------
// CPUSampler — delta-based CPU% computation
// ---------------------------------------------------------------------------

// CPUSampler tracks consecutive /proc/self/stat samples to compute CPU%.
// Instantiate once and call Sample() on each heartbeat tick.
type CPUSampler struct {
	prevTotal    uint64
	prevCPUTicks uint64
	prevTime     time.Time
	clkTck       float64 // clock ticks per second (usually 100)
}

// NewCPUSampler creates a CPUSampler with an initial sample.
func NewCPUSampler() (*CPUSampler, error) {
	clkTck, err := sysconf_CLK_TCK()
	if err != nil {
		clkTck = 100 // safe fallback
	}
	s := &CPUSampler{clkTck: float64(clkTck)}
	if err := s.prime(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *CPUSampler) prime() error {
	stat, err := ReadSelfStat()
	if err != nil {
		return err
	}
	s.prevCPUTicks = stat.UserTime + stat.SystemTime
	s.prevTime = time.Now()
	return nil
}

// Sample returns the CPU% used since the last call to Sample (or NewCPUSampler).
// Returns 0 if called too quickly for a meaningful measurement.
func (s *CPUSampler) Sample() (float64, error) {
	stat, err := ReadSelfStat()
	if err != nil {
		return 0, err
	}
	now := time.Now()
	cpuTicks := stat.UserTime + stat.SystemTime

	elapsed := now.Sub(s.prevTime).Seconds()
	if elapsed < 0.01 {
		return 0, nil
	}

	tickDelta := float64(cpuTicks-s.prevCPUTicks) / s.clkTck
	pct := (tickDelta / elapsed) * 100.0

	s.prevCPUTicks = cpuTicks
	s.prevTime = now
	return pct, nil
}

// ---------------------------------------------------------------------------
// ProcStatus — /proc/<pid>/status (human-readable key:value pairs)
// ---------------------------------------------------------------------------

// ProcStatus holds selected fields from /proc/<pid>/status.
type ProcStatus struct {
	Name     string
	VmRSS    uint64 // resident set size, kB
	VmSwap   uint64 // swap usage, kB
	Threads  int
	PPID     int    // parent process ID
	UID      int    // real user ID
	EUID     int    // effective user ID (differs from UID on setuid binaries)
	GID      int    // real group ID
}

// ReadProcStatus reads /proc/<pid>/status.
func ReadProcStatus(pid int) (*ProcStatus, error) {
	path := fmt.Sprintf("/proc/%d/status", pid)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("proc: open %s: %w", path, err)
	}
	defer f.Close()

	var ps ProcStatus
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		switch key {
		case "Name":
			ps.Name = val
		case "VmRSS":
			ps.VmRSS = parseKB(val)
		case "VmSwap":
			ps.VmSwap = parseKB(val)
		case "Threads":
			ps.Threads, _ = strconv.Atoi(strings.TrimSpace(val))
		case "PPid":
			ps.PPID, _ = strconv.Atoi(strings.TrimSpace(val))
		case "Uid":
			// Format: "real effective saved fs"
			fields := strings.Fields(val)
			if len(fields) >= 2 {
				ps.UID, _ = strconv.Atoi(fields[0])
				ps.EUID, _ = strconv.Atoi(fields[1])
			}
		case "Gid":
			fields := strings.Fields(val)
			if len(fields) >= 1 {
				ps.GID, _ = strconv.Atoi(fields[0])
			}
		}
	}
	return &ps, scanner.Err()
}

// parseKB parses a value like "1234 kB" → 1234.
func parseKB(s string) uint64 {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseUint(fields[0], 10, 64)
	return v
}

// ---------------------------------------------------------------------------
// Cgroup v2 memory reader
// ---------------------------------------------------------------------------

// CgroupMemory holds memory statistics from a cgroup v2 hierarchy.
type CgroupMemory struct {
	// Current is the current memory usage in bytes (memory.current).
	Current uint64
	// High is the high memory threshold in bytes (memory.high).
	// math.MaxUint64 means "max" / unlimited.
	High uint64
	// OOMKills is the cumulative number of OOM kill events (memory.events).
	OOMKills uint64
}

// ReadCgroupMemory reads cgroup v2 memory stats from cgroupPath.
// cgroupPath is the directory, e.g. /sys/fs/cgroup/system.slice/angellab.service.
func ReadCgroupMemory(cgroupPath string) (*CgroupMemory, error) {
	var cm CgroupMemory

	// memory.current
	cur, err := readSingleUint64(cgroupPath + "/memory.current")
	if err != nil {
		return nil, err
	}
	cm.Current = cur

	// memory.events: one "key value" per line
	eventsData, err := os.ReadFile(cgroupPath + "/memory.events")
	if err == nil {
		scanner := bufio.NewScanner(strings.NewReader(string(eventsData)))
		for scanner.Scan() {
			parts := strings.Fields(scanner.Text())
			if len(parts) == 2 && parts[0] == "oom_kill" {
				cm.OOMKills, _ = strconv.ParseUint(parts[1], 10, 64)
			}
		}
	}
	return &cm, nil
}

func readSingleUint64(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("proc: read %s: %w", path, err)
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("proc: parse %s: %w", path, err)
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// sysconf CLK_TCK — clock ticks per second
// ---------------------------------------------------------------------------

// sysconf_CLK_TCK returns the number of clock ticks per second via
// the AT_CLKTCK auxv entry.  Falls back to 100 on error.
func sysconf_CLK_TCK() (int64, error) {
	// Read from /proc/self/auxv.
	data, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		return 100, err
	}
	// auxv is a sequence of (type, value) uint64 pairs on 64-bit systems.
	const AT_CLKTCK = 17
	for i := 0; i+16 <= len(data); i += 16 {
		typ := readUint64LE(data[i:])
		val := readUint64LE(data[i+8:])
		if typ == AT_CLKTCK {
			return int64(val), nil
		}
		if typ == 0 { // AT_NULL
			break
		}
	}
	return 100, fmt.Errorf("proc: AT_CLKTCK not found in auxv")
}

func readUint64LE(b []byte) uint64 {
	_ = b[7] // bounds check
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

// ---------------------------------------------------------------------------
// File descriptor count
// ---------------------------------------------------------------------------

// CountFDs returns the number of open file descriptors for the calling process
// by counting entries in /proc/self/fd.
//
// This is cheap (a single directory read) and extremely useful for detecting
// file descriptor leaks in long-running daemon processes.  A typical angel
// holds 3–15 FDs; growth over time indicates a leak.
func CountFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	// Subtract 1 to exclude the dirfd opened by ReadDir itself,
	// which appears as an entry while we are counting.
	n := len(entries) - 1
	if n < 0 {
		return 0
	}
	return n
}
