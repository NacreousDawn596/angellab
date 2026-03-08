// lab doctor — system prerequisite checker.
//
// Checks all requirements for running AngelLab and prints a clear
// PASS / WARN / FAIL report.  Run before first deployment, or when
// diagnosing "lab angel create succeeded but angel never showed ACTIVE".
//
// Exit codes:
//
//	0  all checks PASS (or WARN only)
//	1  one or more checks FAIL
//
// Checks performed:
//
//	Lab socket         Is the daemon reachable?  (FAIL if not running)
//	Angel binary       Can we exec the angel binary?
//	/proc/net/tcp      Readable — needed by Sentinel
//	/proc/<pid>/fd     Accessible — needed by inode mapper
//	inotify limits     fs.inotify.max_user_watches (WARN if < 65536)
//	cgroup v2          Is /sys/fs/cgroup unified hierarchy present?
//	state directory    Is /var/lib/angellab writable?
//	Protocol version   Does the running daemon match our protocol version?
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nacreousdawn596/angellab/pkg/ipc"
)

// ---------------------------------------------------------------------------
// Check result types
// ---------------------------------------------------------------------------

type checkStatus int

const (
	checkPass checkStatus = iota
	checkWarn
	checkFail
)

func (s checkStatus) String() string {
	switch s {
	case checkPass:
		return ansiGreen + "PASS" + ansiReset
	case checkWarn:
		return ansiYellow + "WARN" + ansiReset
	case checkFail:
		return ansiBoldRed + "FAIL" + ansiReset
	}
	return "????"
}

type checkResult struct {
	name   string
	status checkStatus
	detail string
}

// ---------------------------------------------------------------------------
// cmdDoctor entry point
// ---------------------------------------------------------------------------

func cmdDoctor() {
	fmt.Println()
	fmt.Printf("%s  AngelLab system check%s\n", ansiBoldWhite, ansiReset)
	fmt.Printf("%s  %-36s  %s  %s\n",
		ansiDim, "Check", "Result", "Detail"+ansiReset)
	fmt.Println(ansiDim + "  " + strings.Repeat("─", 72) + ansiReset)

	results := runChecks()

	anyFail := false
	for _, r := range results {
		fmt.Printf("  %-36s  %s  %s\n", r.name, r.status.String(), ansiDim+r.detail+ansiReset)
		if r.status == checkFail {
			anyFail = true
		}
	}

	fmt.Println()
	if anyFail {
		fmt.Printf("  %s  One or more checks failed.%s\n\n", ansiBoldRed+"✗"+ansiReset, "")
		os.Exit(1)
	}
	fmt.Printf("  %s  All checks passed.%s\n\n", ansiBoldGreen+"✓"+ansiReset, "")
}

// runChecks executes every check and returns the results in order.
func runChecks() []checkResult {
	return []checkResult{
		checkSocket(),
		checkProtocolVersion(),
		checkAngelBinary(),
		checkProcNetTCP(),
		checkProcFD(),
		checkInotifyLimit(),
		checkCgroupV2(),
		checkStateDir(),
		checkKernelVersion(),
	}
}

// ---------------------------------------------------------------------------
// Individual checks
// ---------------------------------------------------------------------------

func checkSocket() checkResult {
	name := "Lab socket reachable"
	sock := socketPath()

	// Check the file exists first.
	if _, err := os.Stat(sock); err != nil {
		return checkResult{name, checkFail,
			fmt.Sprintf("%s: not found (is labd running?)", sock)}
	}

	// Try to dial.
	c, err := ipc.NewClient(sock)
	if err != nil {
		return checkResult{name, checkFail, err.Error()}
	}
	c.Close()
	return checkResult{name, checkPass, sock}
}

func checkProtocolVersion() checkResult {
	name := "Protocol version match"
	c, err := ipc.NewClient(socketPath())
	if err != nil {
		return checkResult{name, checkWarn, "cannot connect — skipping"}
	}
	defer c.Close()

	resp, err := c.Request(ipc.CLICmdLabStatus, nil)
	if err != nil || !resp.OK {
		return checkResult{name, checkWarn, "cannot fetch status — skipping"}
	}

	var status ipc.LabStatus
	if err := ipc.DecodeAs(resp.Data, &status); err != nil {
		return checkResult{name, checkWarn, "cannot decode status"}
	}

	return checkResult{name, checkPass,
		fmt.Sprintf("daemon %s, client protocol v%d", status.Version, ipc.ProtocolVersion)}
}

func checkAngelBinary() checkResult {
	name := "Angel binary executable"

	// Try common locations.
	candidates := []string{
		"/usr/local/bin/angel",
		"/usr/bin/angel",
		"./angel",
	}
	// Also check $PATH via LookPath-equivalent.
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.Mode()&0o111 == 0 {
			return checkResult{name, checkFail,
				fmt.Sprintf("%s exists but is not executable", path)}
		}
		return checkResult{name, checkPass, path}
	}
	return checkResult{name, checkFail,
		"angel binary not found in /usr/local/bin, /usr/bin, or ./"}
}

func checkProcNetTCP() checkResult {
	name := "/proc/net/tcp readable"
	f, err := os.Open("/proc/net/tcp")
	if err != nil {
		return checkResult{name, checkFail,
			"cannot read /proc/net/tcp — Sentinel will not work"}
	}
	f.Close()

	// Also check tcp6.
	f6, err := os.Open("/proc/net/tcp6")
	if err != nil {
		return checkResult{name, checkWarn,
			"/proc/net/tcp OK, /proc/net/tcp6 unreadable (IPv6 connections won't be monitored)"}
	}
	f6.Close()
	return checkResult{name, checkPass, "/proc/net/tcp and /proc/net/tcp6"}
}

func checkProcFD() checkResult {
	name := "/proc/<pid>/fd accessible"

	// Check our own fd directory as a proxy.
	pid := os.Getpid()
	path := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(path)
	if err != nil {
		return checkResult{name, checkFail,
			"cannot read /proc/<pid>/fd — inode→process mapping will fail"}
	}
	return checkResult{name, checkPass,
		fmt.Sprintf("own fd dir readable (%d open fds)", len(entries))}
}

func checkInotifyLimit() checkResult {
	name := "inotify watch limit"

	data, err := os.ReadFile("/proc/sys/fs/inotify/max_user_watches")
	if err != nil {
		return checkResult{name, checkWarn,
			"cannot read /proc/sys/fs/inotify/max_user_watches"}
	}

	val, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return checkResult{name, checkWarn,
			fmt.Sprintf("cannot parse value: %s", data)}
	}

	const recommended = 65536
	if val < recommended {
		return checkResult{name, checkWarn,
			fmt.Sprintf("current %d < recommended %d — run: echo %d > /proc/sys/fs/inotify/max_user_watches",
				val, recommended, recommended)}
	}
	return checkResult{name, checkPass, fmt.Sprintf("max_user_watches = %d", val)}
}

func checkCgroupV2() checkResult {
	name := "cgroup v2 (unified hierarchy)"

	// cgroup v2 mounts the unified hierarchy at /sys/fs/cgroup.
	// If it's v1, /sys/fs/cgroup/memory exists as a separate controller.
	memCtrl := "/sys/fs/cgroup/memory"
	if _, err := os.Stat(memCtrl); err == nil {
		return checkResult{name, checkWarn,
			"cgroup v1 detected — memory.events OOM detection unavailable"}
	}

	cgroupRoot := "/sys/fs/cgroup"
	if _, err := os.Stat(cgroupRoot); err != nil {
		return checkResult{name, checkFail,
			"/sys/fs/cgroup not found — Memory Angel cgroup monitoring disabled"}
	}

	// Check for a cgroup v2 marker file.
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return checkResult{name, checkWarn,
			"cannot confirm cgroup v2 (cgroup.controllers missing)"}
	}

	// Check that memory controller is available.
	data, _ := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if !strings.Contains(string(data), "memory") {
		return checkResult{name, checkWarn,
			"cgroup v2 present but memory controller not enabled"}
	}

	return checkResult{name, checkPass, "cgroup v2 with memory controller"}
}

func checkStateDir() checkResult {
	name := "State directory writable"

	dir := "/var/lib/angellab"
	if err := os.MkdirAll(dir, 0750); err != nil {
		return checkResult{name, checkFail,
			fmt.Sprintf("cannot create %s: %v", dir, err)}
	}

	// Write a probe file.
	probe := fmt.Sprintf("%s/.doctor_probe_%d", dir, time.Now().UnixNano())
	if err := os.WriteFile(probe, []byte("ok"), 0600); err != nil {
		return checkResult{name, checkFail,
			fmt.Sprintf("%s is not writable: %v", dir, err)}
	}
	os.Remove(probe)
	return checkResult{name, checkPass, dir}
}

func checkKernelVersion() checkResult {
	name := "Kernel version"

	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return checkResult{name, checkWarn, "cannot read /proc/version"}
	}

	// Extract "Linux version X.Y.Z ..." — take first field after "version".
	fields := strings.Fields(string(data))
	version := "unknown"
	for i, f := range fields {
		if f == "version" && i+1 < len(fields) {
			version = fields[i+1]
			break
		}
	}

	// Require at least 4.18 for inotify improvements and cgroup v2.
	major, minor := parseKernelVersion(version)
	if major < 4 || (major == 4 && minor < 18) {
		return checkResult{name, checkWarn,
			fmt.Sprintf("kernel %s — recommend ≥ 4.18 for full cgroup v2 support", version)}
	}
	return checkResult{name, checkPass, "Linux " + version}
}

// parseKernelVersion extracts the major and minor version numbers from a
// kernel version string like "5.15.0-76-generic".
func parseKernelVersion(v string) (major, minor int) {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) >= 1 {
		major, _ = strconv.Atoi(parts[0])
	}
	if len(parts) >= 2 {
		// Strip any non-numeric suffix from minor (e.g. "15-generic").
		m := parts[1]
		for i, r := range m {
			if r < '0' || r > '9' {
				m = m[:i]
				break
			}
		}
		minor, _ = strconv.Atoi(m)
	}
	return
}
