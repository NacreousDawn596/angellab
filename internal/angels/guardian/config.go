package guardian

import (
	"encoding/json"
	"fmt"
	"io"
)

// Config is the guardian-specific configuration decoded from stdin.
// Lab passes this as a JSON blob when spawning the angel.
type Config struct {
	Type        string   `json:"type"`
	ID          string   `json:"id"`
	Paths       []string `json:"paths"`
	SnapshotDir string   `json:"snapshot_dir"`
}

// readConfig deserialises a Config from r (typically os.Stdin).
func readConfig(r io.Reader) (*Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if len(data) == 0 {
		// No config provided — return defaults.
		return &Config{
			SnapshotDir: "/var/lib/angellab/snapshots",
		}, nil
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if cfg.SnapshotDir == "" {
		cfg.SnapshotDir = "/var/lib/angellab/snapshots"
	}
	return &cfg, nil
}
