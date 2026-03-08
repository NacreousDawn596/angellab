package process

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Config holds Process Angel configuration delivered via stdin from Lab.
type Config struct {
	Type string `json:"type"`
	ID   string `json:"id"`

	// PollInterval is how often /proc is scanned for process changes.
	// Lower = faster detection; higher = less CPU.
	PollInterval time.Duration `json:"poll_interval"`

	// BaselineDuration is how long the angel observes before alerting.
	// During this period it learns which processes are "normal".
	BaselineDuration time.Duration `json:"baseline_duration"`

	// WhitelistExes is a list of executable paths that are always allowed.
	// Exact match against /proc/<pid>/exe.
	// Example: ["/usr/bin/sshd", "/usr/sbin/cron"]
	WhitelistExes []string `json:"whitelist_exes,omitempty"`

	// WhitelistComms is a list of process comm names that are always allowed.
	// Partial match: "systemd" matches "systemd-journald".
	WhitelistComms []string `json:"whitelist_comms,omitempty"`

	// SuspiciousExeDirs contains directory prefixes whose processes are
	// always scored high regardless of baseline.
	// Example: ["/tmp", "/dev/shm", "/var/tmp"]
	SuspiciousExeDirs []string `json:"suspicious_exe_dirs,omitempty"`

	// AlertOnExit emits an INFO event when a whitelisted or baseline process
	// exits unexpectedly.  Useful for detecting killed daemons.
	AlertOnExit bool `json:"alert_on_exit"`

	// StateDir is where the process baseline is persisted.
	StateDir string `json:"state_dir"`
}

func readConfig(r io.Reader) (*Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{
		PollInterval:     2 * time.Second,
		BaselineDuration: 30 * time.Second,
		AlertOnExit:      true,
		StateDir:         "/var/lib/angellab",
		SuspiciousExeDirs: []string{
			"/tmp",
			"/dev/shm",
			"/var/tmp",
			"/run/user",
		},
	}
	if len(data) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.BaselineDuration <= 0 {
		cfg.BaselineDuration = 30 * time.Second
	}
	if cfg.StateDir == "" {
		cfg.StateDir = "/var/lib/angellab"
	}
	return cfg, nil
}
