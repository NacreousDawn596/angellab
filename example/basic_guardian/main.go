// example/basic_guardian — minimal AngelLab setup.
//
// This example shows the simplest possible Lab daemon startup:
//   - One Guardian angel watching /etc/passwd and /etc/hosts
//   - Logs to stdout only (no file sink)
//   - No Prometheus metrics
//   - Runs until Ctrl-C
//
// Run (requires root for the socket path):
//
//	sudo go run ./example/basic_guardian
//
// In another terminal, watch events:
//
//	lab events
//
// Then trigger a detection by modifying a watched file:
//
//	sudo echo "# touched" >> /etc/hosts
//
// You should see a CRIT event within seconds, followed by an automatic restore.
//
// Configuration note:
// The socket is created at /run/angellab/lab.sock by default.
// Override with LAB_SOCKET or pass a custom Config.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nacreousdawn596/angellab/internal/lab"
	"github.com/nacreousdawn596/angellab/pkg/logging"
)

func main() {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "error: basic_guardian must run as root (needs /run/angellab and /etc access)")
		os.Exit(1)
	}

	// -----------------------------------------------------------------------
	// Configure
	// -----------------------------------------------------------------------

	cfg := lab.DefaultConfig()

	// Use a temp directory for this example so it doesn't conflict with a
	// production AngelLab installation.
	cfg.Lab.SocketPath  = "/tmp/angellab-example.sock"
	cfg.Lab.RegistryPath = "/tmp/angellab-example.db"
	cfg.Lab.LogPath     = "" // stdout only for the example

	// One Guardian watching common sensitive files.
	cfg.Angels = []lab.AngelConfig{
		{
			Type: "guardian",
			ID:   "A-01",
			Paths: []string{
				"/etc/passwd",
				"/etc/hosts",
				"/etc/hostname",
			},
			SnapshotDir: "/tmp/angellab-example-snapshots",
		},
	}

	// Accelerated heartbeat for demo purposes.
	cfg.Supervisor.HeartbeatInterval = lab.Duration{Duration: 5 * time.Second}
	cfg.Supervisor.HeartbeatTimeout  = lab.Duration{Duration: 15 * time.Second}

	// -----------------------------------------------------------------------
	// Logger (stdout only)
	// -----------------------------------------------------------------------

	log := logging.New("Lab", logging.LevelInfo, os.Stdout)

	log.Info("AngelLab basic_guardian example")
	log.Info("socket: %s", cfg.Lab.SocketPath)
	log.Info("watching: /etc/passwd, /etc/hosts, /etc/hostname")
	log.Info("modify any watched file to trigger a detection + restore")

	// -----------------------------------------------------------------------
	// Create snapshot directory
	// -----------------------------------------------------------------------

	if err := os.MkdirAll("/tmp/angellab-example-snapshots", 0700); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir snapshots: %v\n", err)
		os.Exit(1)
	}

	// -----------------------------------------------------------------------
	// Start daemon
	// -----------------------------------------------------------------------

	ctx, cancel := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT, syscall.SIGTERM,
	)
	defer cancel()

	daemon, err := lab.NewDaemon(cfg, log, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start daemon: %v\n", err)
		os.Exit(1)
	}

	log.Info("daemon started — Ctrl-C to stop")

	if err := daemon.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
		os.Exit(1)
	}

	// -----------------------------------------------------------------------
	// Cleanup
	// -----------------------------------------------------------------------

	_ = os.Remove("/tmp/angellab-example.sock")
	_ = os.Remove("/tmp/angellab-example.db")
	log.Info("shutdown complete")
}
