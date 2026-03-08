package sentinel

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Config holds Sentinel-specific configuration passed from Lab via stdin.
type Config struct {
	Type string `json:"type"`
	ID   string `json:"id"`

	// PollInterval is how often /proc/net/tcp is polled.
	// Lower values detect connections faster; minimum recommended: 1s.
	PollInterval time.Duration `json:"poll_interval"`

	// BaselineDuration is the training window length.
	// Longer values learn more traffic patterns. Minimum recommended: 60s.
	BaselineDuration time.Duration `json:"baseline_duration"`

	// StateDir is where the sentinel writes its baseline JSON.
	// Defaults to /var/lib/angellab.
	StateDir string `json:"state_dir"`

	// WarnThreshold overrides the default score warn level (3).
	WarnThreshold int `json:"warn_threshold,omitempty"`

	// CritThreshold overrides the default score critical level (6).
	CritThreshold int `json:"crit_threshold,omitempty"`

	// InodeCacheTTL overrides the inode map rebuild interval.
	// Defaults to 10s (rebuildInterval constant).
	InodeCacheTTL time.Duration `json:"inode_cache_ttl,omitempty"`
}

func readConfig(r io.Reader) (*Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{
		PollInterval:     2 * time.Second,
		BaselineDuration: 60 * time.Second,
		StateDir:         "/var/lib/angellab",
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
		cfg.BaselineDuration = 60 * time.Second
	}
	if cfg.StateDir == "" {
		cfg.StateDir = "/var/lib/angellab"
	}
	return cfg, nil
}
