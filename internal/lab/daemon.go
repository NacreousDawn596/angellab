// Package lab contains the core Lab daemon implementation.
//
// This file owns: configuration schema, Daemon struct, NewDaemon constructor,
// and the Run() entry point that ties all subsystems together.
package lab

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/nacreousdawn596/angellab/pkg/ipc"
	"github.com/nacreousdawn596/angellab/pkg/logging"
	"github.com/nacreousdawn596/angellab/pkg/metrics"
	"github.com/nacreousdawn596/angellab/pkg/registry"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// Config is the top-level configuration structure loaded from angellab.toml.
type Config struct {
	Lab        LabConfig        `toml:"lab"`
	Supervisor SupervisorConfig `toml:"supervisor"`
	Angels     []AngelConfig    `toml:"angel"`
}

// LabConfig holds Lab daemon settings.
type LabConfig struct {
	// SocketPath is the Unix domain socket Lab binds on.
	SocketPath string `toml:"socket"`
	// RegistryPath is the SQLite database file.
	RegistryPath string `toml:"registry"`
	// LogPath is the rotating log file destination.
	LogPath string `toml:"log_path"`
	// LogLevel controls minimum log verbosity: debug, info, warn, crit.
	LogLevel string `toml:"log_level"`
	// LogFormat controls output format: "text" (default) or "json".
	// Use "json" when feeding logs to Loki, Splunk, or Datadog.
	LogFormat string `toml:"log_format"`
	// AngelBinary is the path to the angel worker binary.
	AngelBinary string `toml:"angel_binary"`
	// MetricsAddr is the TCP address for the Prometheus metrics endpoint.
	// Example: ":9101". Leave empty to disable.
	MetricsAddr string `toml:"metrics_addr"`
}

// SupervisorConfig controls angel lifecycle management.
type SupervisorConfig struct {
	// HeartbeatInterval is how often each angel must send a heartbeat.
	HeartbeatInterval Duration `toml:"heartbeat_interval"`
	// HeartbeatTimeout is how long Lab waits before declaring an angel UNSTABLE.
	HeartbeatTimeout Duration `toml:"heartbeat_timeout"`
	// MaxRestarts is the maximum number of times Lab will restart a crashed angel
	// before moving it to CONTAINED.
	MaxRestarts int `toml:"max_restarts"`
	// RestartBackoff is the delay between successive restart attempts.
	RestartBackoff Duration `toml:"restart_backoff"`
}

// AngelConfig declares a statically configured angel to spawn on startup.
// Dynamic angels created via CLI are stored only in the registry.
type AngelConfig struct {
	// Type is the angel type: "guardian", "sentinel", "memory".
	Type string `toml:"type"`
	// ID is the desired angel identifier, e.g. "A-01".
	// If empty, Lab generates one from a counter.
	ID string `toml:"id"`
	// Paths is used by the Guardian angel.
	Paths []string `toml:"paths,omitempty"`
	// SnapshotDir is used by the Guardian angel.
	SnapshotDir string `toml:"snapshot_dir,omitempty"`
	// BaselineDuration is used by the Sentinel angel.
	BaselineDuration Duration `toml:"baseline_duration,omitempty"`
	// Extra is an escape hatch for angel-type-specific config not
	// covered by the above fields.
	Extra map[string]string `toml:"extra,omitempty"`
}

// Duration wraps time.Duration to support TOML string format like "10s".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// DefaultConfig returns a configuration with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Lab: LabConfig{
			SocketPath:   "/run/angellab/lab.sock",
			RegistryPath: "/var/lib/angellab/registry.db",
			LogPath:      "/var/log/angellab/lab.log",
			LogLevel:     "info",
			LogFormat:    "text",
			AngelBinary:  "/usr/local/bin/angel",
		},
		Supervisor: SupervisorConfig{
			HeartbeatInterval: Duration{10 * time.Second},
			HeartbeatTimeout:  Duration{30 * time.Second},
			MaxRestarts:       5,
			RestartBackoff:    Duration{5 * time.Second},
		},
	}
}

// LoadConfig reads and parses angellab.toml from path.
// Missing fields are filled in from DefaultConfig.
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil // run with defaults if no config file
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return cfg, nil
}

// ---------------------------------------------------------------------------
// Daemon
// ---------------------------------------------------------------------------

// Daemon is the top-level Lab supervisor.
// It owns the registry, socket listener, supervisor loop, and event broadcaster.
type Daemon struct {
	cfg        *Config
	configPath string // path to angellab.toml — stored for hot-reload
	log        *logging.Logger
	reg        *registry.Registry
	listener   *ipc.Listener
	sup        *Supervisor
	bcast      *Broadcaster
	metrics    *metrics.Server // Prometheus exporter (nil if disabled)
	startedAt  time.Time
}

// NewDaemon initialises all Lab subsystems.
// systemdSocket, if true, inherits the socket file descriptor from systemd
// socket activation (SD_LISTEN_FDS) instead of creating a new one.
func NewDaemon(cfg *Config, log *logging.Logger, systemdSocket bool) (*Daemon, error) {
	// Open (or create) the angel registry.
	reg, err := registry.Open(cfg.Lab.RegistryPath)
	if err != nil {
		return nil, fmt.Errorf("daemon: open registry: %w", err)
	}

	// Bind the Unix socket.
	var listener *ipc.Listener
	if systemdSocket {
		listener, err = ipc.InheritSystemdListener()
	} else {
		listener, err = ipc.Listen(cfg.Lab.SocketPath)
	}
	if err != nil {
		return nil, fmt.Errorf("daemon: bind socket: %w", err)
	}
	log.Info("listening on %s", cfg.Lab.SocketPath)

	bcast := NewBroadcaster()
	sup := NewSupervisor(cfg, log, reg, bcast)

	return &Daemon{
		cfg:      cfg,
		log:      log,
		reg:      reg,
		listener: listener,
		sup:      sup,
		bcast:    bcast,
	}, nil
}

// Run starts all Lab goroutines and blocks until ctx is cancelled.
// It performs an orderly shutdown: stops accepting new connections,
// sends TERMINATE to all active angels, then closes the registry.
func (d *Daemon) Run(ctx context.Context) error {
	d.startedAt = time.Now()

	// Boot recovery: reconcile the registry with live processes.
	if err := d.sup.BootRecovery(ctx); err != nil {
		d.log.Warn("boot recovery error: %v", err)
	}

	// Start Prometheus metrics endpoint if configured.
	if d.cfg.Lab.MetricsAddr != "" {
		metSrv := metrics.NewServer(d.cfg.Lab.MetricsAddr, d.startedAt)
		d.metrics = metSrv
		// Wire the hook so Supervisor can update metrics without an import cycle.
		d.sup.metricsReg = &metricsHook{
			UpdateAngel: func(id, typ, state, connState string, restarts int, rss uint64, cpu float64, fd, goroutines int, uptime int64) {
				metSrv.Registry.UpdateAngel(&metrics.AngelMetrics{
					ID: id, AngelType: typ, State: state, ConnState: connState,
					RestartCount: restarts, RSSBytes: rss, CPUPercent: cpu,
					FDCount: fd, Goroutines: goroutines, UptimeSecs: uptime,
					UpdatedAt: time.Now(),
				})
			},
			IncrementEvent: metSrv.Registry.IncrementEvent,
		}
		addr, err := metSrv.ListenAndServe()
		if err != nil {
			d.log.Warn("metrics server: %v — continuing without metrics", err)
			d.metrics = nil
		} else {
			d.log.Info("[Angel Lab] metrics endpoint → http://%s/metrics", addr)
		}
	}

	// Start the socket server (blocks in background).
	server := NewServer(d.cfg, d.log, d.reg, d.sup, d.bcast, d.startedAt)
	go server.Serve(ctx, d.listener)

	// Start the supervisor heartbeat watcher.
	go d.sup.Run(ctx)

	// SIGHUP → hot-reload config.
	sigHUP := make(chan os.Signal, 1)
	signal.Notify(sigHUP, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sigHUP:
				if err := d.Reload(ctx); err != nil {
					d.log.Warn("reload failed: %v", err)
				}
			}
		}
	}()

	// Block until context is cancelled.
	<-ctx.Done()
	d.log.Info("shutdown signal received, stopping angels")

	// Graceful shutdown.
	d.sup.TerminateAll()
	if d.metrics != nil {
		_ = d.metrics.Close()
	}
	d.listener.Close()
	d.reg.Close()
	return nil
}
