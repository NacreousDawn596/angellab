// labd — AngelLab supervisor daemon.
//
// labd is the long-running process that owns the angel registry,
// spawns angel subprocesses, and serves the lab.sock IPC endpoint.
//
// Designed to be managed by systemd. For development, run directly:
//
//	sudo labd --config ./configs/angellab.toml
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/nacreousdawn596/angellab/internal/lab"
	"github.com/nacreousdawn596/angellab/pkg/logging"
	"github.com/nacreousdawn596/angellab/pkg/version"
)

func main() {
	var (
		configPath  = flag.String("config", "/etc/angellab/angellab.toml", "path to angellab.toml")
		systemd     = flag.Bool("systemd", false, "inherit socket from systemd socket activation")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		os.Exit(0)
	}

	log := logging.NewDefault("Lab")

	cfg, err := lab.LoadConfig(*configPath)
	if err != nil {
		log.Crit("failed to load config %s: %v", *configPath, err)
		os.Exit(1)
	}

	if logFile, err := logging.OpenRotating(cfg.Lab.LogPath, 32<<20, 5); err != nil {
		log.Warn("cannot open log file %s: %v — stdout only", cfg.Lab.LogPath, err)
	} else {
		log = logging.New("Lab", logging.ParseLevel(cfg.Lab.LogLevel), os.Stdout, logFile)
	}

	log.Info("[Angel Lab] %s starting — pid %d", version.Version, os.Getpid())

	daemon, err := lab.NewDaemon(cfg, log, *systemd)
	if err != nil {
		log.Crit("initialisation failed: %v", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGTERM,
		syscall.SIGINT,
	)
	defer stop()

	if err := daemon.Run(ctx); err != nil {
		log.Crit("daemon error: %v", err)
		os.Exit(1)
	}

	log.Info("[Angel Lab] shutdown complete")
}
