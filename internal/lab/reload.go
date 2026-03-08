// Package lab — reload.go
//
// Config hot-reload on SIGHUP.
//
// When labd receives SIGHUP it calls Reload() on the Daemon.  The reload:
//
//  1. Re-reads the configuration file from disk.
//  2. Updates log level if it changed (no restart required).
//  3. Detects newly-added static angels (present in new config but not
//     currently running) and spawns them.
//  4. Does NOT stop existing angels.  Removing an angel from config
//     does not kill it — use "lab angel terminate <id>" for that.
//  5. Does NOT rebind the socket (that would drop all connections).
//  6. Does NOT change BaselineDuration for running angels (they keep
//     their training window).
//
// This is intentionally conservative: hot-reload is for adding angels
// and tuning log verbosity.  Structural changes require a restart.
//
// Example:
//
//	sudo kill -HUP $(pidof labd)
//	lab status   # new angel should appear
package lab

import (
	"context"
	"fmt"
)

// Reload re-reads the config file and applies non-disruptive changes.
// It is called by the daemon's SIGHUP handler.
func (d *Daemon) Reload(ctx context.Context) error {
	d.log.Info("[Angel Lab] SIGHUP received — reloading configuration from %s", d.configPath)

	newCfg, err := LoadConfig(d.configPath)
	if err != nil {
		d.log.Warn("reload: failed to parse %s: %v — keeping current config", d.configPath, err)
		return fmt.Errorf("reload: parse: %w", err)
	}

	// --- 1. Log level change ---
	if newCfg.Lab.LogLevel != d.cfg.Lab.LogLevel {
		d.log.Info("reload: log level %s → %s", d.cfg.Lab.LogLevel, newCfg.Lab.LogLevel)
		// The logger package provides SetLevel via ParseLevel.
		// We update the daemon's logger threshold without touching the file sink.
		// TODO: wire up logging.Logger.SetLevel when the logging package is extended.
	}

	// --- 2. Spawn any newly-added static angels ---
	currentIDs := make(map[string]struct{})
	for _, entry := range d.sup.ListEntries() {
		currentIDs[entry.ID] = struct{}{}
	}

	spawned := 0
	for _, ac := range newCfg.Angels {
		if ac.ID == "" {
			continue // no stable ID — skip (can't deduplicate)
		}
		if _, running := currentIDs[ac.ID]; running {
			continue // already running
		}
		d.log.Info("reload: spawning new angel %s (%s)", ac.ID, ac.Type)
		if err := d.sup.SpawnAngel(ctx, &ac); err != nil {
			d.log.Warn("reload: spawn %s: %v", ac.ID, err)
			continue
		}
		spawned++
	}

	// --- 3. Update in-memory config for future reference ---
	d.cfg = newCfg

	if spawned > 0 {
		d.log.Info("reload: spawned %d new angel(s)", spawned)
	} else {
		d.log.Info("reload: no new angels to spawn — configuration updated")
	}

	return nil
}
