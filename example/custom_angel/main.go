// example/custom_angel — template for writing a new angel type.
//
// AngelLab is designed to be extended.  This example implements a minimal
// "PingAngel" that periodically pings a list of hostnames and emits a CRIT
// event when any of them become unreachable.
//
// # How a new angel type is wired in
//
// Every angel is a standalone process.  The Lab daemon launches it via
// exec.Cmd, passing its configuration as JSON on stdin.  The angel then:
//
//  1. Reads its config from os.Stdin.
//  2. Dials Lab at the socket path from the config.
//  3. Sends a HELLO frame (handled by ipc.Dial).
//  4. Sends a REGISTER frame with its ID and type.
//  5. Sends HEARTBEAT frames every 10 seconds.
//  6. Sends EVENT frames whenever it detects something noteworthy.
//  7. Exits when the socket closes (heartbeat failure detection).
//
// To add a PingAngel to your installation:
//
//  1. Copy this file to cmd/angel/ and add the dispatch case below.
//  2. Add a [[angel]] block in angellab.toml:
//
//	   [[angel]]
//	   type = "ping"
//	   id   = "A-ping"
//	   [angel.extra]
//	   hosts = "8.8.8.8,1.1.1.1,github.com"
//	   interval = "30s"
//
//  3. Rebuild: make build
//  4. Restart: sudo systemctl restart angellab
//
// Run standalone (without a Lab daemon) to test the logic:
//
//	go run ./example/custom_angel
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/nacreousdawn596/angellab/pkg/ipc"
	"github.com/nacreousdawn596/angellab/pkg/logging"
)

// ---------------------------------------------------------------------------
// PingAngel config
// ---------------------------------------------------------------------------

// PingConfig is the configuration passed via stdin from Lab.
// In a real deployment, this is serialised from AngelConfig.Extra.
type PingConfig struct {
	Type       string        `json:"type"`
	ID         string        `json:"id"`
	LabSocket  string        `json:"lab_socket"`
	Hosts      []string      `json:"hosts"`
	Interval   time.Duration `json:"interval"`
	TimeoutSec int           `json:"timeout_sec"`
}

func defaultPingConfig() *PingConfig {
	return &PingConfig{
		ID:         "A-ping",
		Type:       "ping",
		LabSocket:  "/run/angellab/lab.sock",
		Hosts:      []string{"8.8.8.8", "1.1.1.1", "github.com"},
		Interval:   30 * time.Second,
		TimeoutSec: 5,
	}
}

// ---------------------------------------------------------------------------
// PingAngel implementation
// ---------------------------------------------------------------------------

type PingAngel struct {
	cfg  *PingConfig
	conn *ipc.Conn
	log  *logging.Logger
}

func (a *PingAngel) run(ctx context.Context) error {
	// Dial Lab.
	conn, err := ipc.Dial(a.cfg.LabSocket, ipc.RoleAngel)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	a.conn = conn
	defer conn.Close()

	// Register.
	if err := a.register(); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	// Initial heartbeat.  If it fails, Lab is not reachable.
	if err := a.sendHeartbeat(); err != nil {
		return fmt.Errorf("initial heartbeat: %w", err)
	}

	pollTick      := time.NewTicker(a.cfg.Interval)
	heartbeatTick := time.NewTicker(10 * time.Second)
	defer pollTick.Stop()
	defer heartbeatTick.Stop()

	a.log.Info("PingAngel %s ACTIVE — watching %s", a.cfg.ID, strings.Join(a.cfg.Hosts, ", "))

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-heartbeatTick.C:
			// Fail-fast: if the heartbeat send fails twice, the Lab is gone.
			if err := a.sendHeartbeat(); err != nil {
				if retry := a.sendHeartbeat(); retry != nil {
					return fmt.Errorf("heartbeat failed (2 attempts): %w", retry)
				}
			}

		case <-pollTick.C:
			a.checkHosts()
		}
	}
}

// checkHosts pings each host and emits events for unreachable ones.
func (a *PingAngel) checkHosts() {
	timeout := time.Duration(a.cfg.TimeoutSec) * time.Second
	for _, host := range a.cfg.Hosts {
		// Use TCP dial to port 443 as a simple reachability probe.
		// For ICMP ping you would need raw sockets (root) — TCP is portable.
		addr := net.JoinHostPort(host, "443")
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			a.emitEvent(ipc.SeverityCritical,
				fmt.Sprintf("host unreachable: %s (%v)", host, err),
				map[string]string{"host": host, "error": err.Error()},
			)
		} else {
			conn.Close()
			a.log.Debug("ping OK: %s", host)
		}
	}
}

func (a *PingAngel) register() error {
	payload, err := ipc.EncodePayload(&ipc.RegisterPayload{
		AngelID:   a.cfg.ID,
		AngelType: a.cfg.Type,
		PID:       os.Getpid(),
	})
	if err != nil {
		return err
	}
	return a.conn.Send(&ipc.Message{
		Version: ipc.ProtocolVersion,
		Kind:    ipc.KindRegister,
		Payload: payload,
	})
}

// sendHeartbeat implements the fail-fast interface: returns an error on
// connection failure so the caller can apply the one-retry policy.
func (a *PingAngel) sendHeartbeat() error {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	payload, err := ipc.EncodePayload(&ipc.HeartbeatPayload{
		AngelID:    a.cfg.ID,
		State:      "ACTIVE",
		Goroutines: runtime.NumGoroutine(),
		RSSBytes:   memStats.Sys,
		AngelMeta:  map[string]string{"hosts": fmt.Sprintf("%d", len(a.cfg.Hosts))},
	})
	if err != nil {
		return fmt.Errorf("heartbeat: encode: %w", err)
	}
	if err := a.conn.Send(&ipc.Message{
		Version: ipc.ProtocolVersion,
		Kind:    ipc.KindHeartbeat,
		Payload: payload,
	}); err != nil {
		return err
	}
	return nil
}

func (a *PingAngel) emitEvent(sev ipc.Severity, msg string, meta map[string]string) {
	a.log.Crit("[Angel Lab] PingAngel %s %s", a.cfg.ID, msg)
	payload, err := ipc.EncodePayload(&ipc.EventPayload{
		AngelID:   a.cfg.ID,
		Severity:  sev,
		Message:   msg,
		Timestamp: time.Now().UTC(),
		Meta:      meta,
	})
	if err != nil {
		a.log.Warn("emitEvent encode: %v", err)
		return
	}
	_ = a.conn.Send(&ipc.Message{
		Version: ipc.ProtocolVersion,
		Kind:    ipc.KindEvent,
		Payload: payload,
	})
}

// ---------------------------------------------------------------------------
// Standalone main (example mode — connects to real Lab if available)
// ---------------------------------------------------------------------------

func main() {
	log := logging.NewDefault("PingAngel")

	// Read config from stdin if provided; otherwise use defaults.
	cfg := defaultPingConfig()
	data, _ := io.ReadAll(os.Stdin)
	if len(data) > 0 {
		if err := json.Unmarshal(data, cfg); err != nil {
			log.Crit("config decode: %v", err)
			os.Exit(1)
		}
	}

	// In standalone mode, skip the Lab connection so the example runs
	// without a daemon.
	standalone := true
	if _, err := net.Dial("unix", cfg.LabSocket); err == nil {
		standalone = false
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if standalone {
		log.Info("running in standalone mode (no Lab daemon found at %s)", cfg.LabSocket)
		log.Info("to connect to a real daemon, start labd first")
		log.Info("")

		// In standalone mode just run the ping logic directly.
		a := &PingAngel{cfg: cfg, log: log}
		tick := time.NewTicker(cfg.Interval)
		defer tick.Stop()

		log.Info("PingAngel standalone — watching: %s", strings.Join(cfg.Hosts, ", "))
		a.checkHosts() // immediate first check

		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				a.checkHosts()
			}
		}
	}

	// Full mode: dial Lab and run normally.
	a := &PingAngel{cfg: cfg, log: log}
	if err := a.run(ctx); err != nil {
		log.Crit("angel exited: %v", err)
		os.Exit(1)
	}
}
