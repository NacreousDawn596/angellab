// Package guardian implements the Guardian Angel.
//
// The Guardian protects a configurable set of filesystem paths by watching
// them with inotify and restoring any deviation from the last-good snapshot.
//
// Monitoring loop:
//  1. Take initial snapshots (SHA-256 + raw bytes) of all watched paths.
//  2. Register inotify watches with DefaultWatchMask.
//  3. On IN_MODIFY / IN_CLOSE_WRITE: hash current file vs snapshot.
//     Mismatch → atomic restore → emit CRITICAL event.
//  4. On IN_DELETE / IN_DELETE_SELF: restore from snapshot.
//     Re-add the inotify watch after restore (atomic-write editors remove
//     and recreate files, causing IN_DELETE_SELF to fire and the watch to
//     vanish silently without this step).
//  5. On IN_CREATE (new file in watched dir): take a baseline snapshot.
//  6. Every snapshotInterval: refresh snapshots for all watched paths.
//  7. Every heartbeatInterval: send HeartbeatPayload with FDCount.
package guardian

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	// "io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/nacreousdawn596/angellab/pkg/ipc"
	"github.com/nacreousdawn596/angellab/pkg/linux"
	"github.com/nacreousdawn596/angellab/pkg/logging"
)

// Run is the guardian subcommand entry point called from cmd/angel/main.go.
func Run() {
	var (
		id        = flag.String("id", "", "angel ID assigned by Lab")
		labSocket = flag.String("lab", "/run/angellab/lab.sock", "path to lab.sock")
	)
	flag.Parse()

	if *id == "" {
		fmt.Fprintln(os.Stderr, "guardian: --id is required")
		os.Exit(1)
	}

	log := logging.NewDefault(fmt.Sprintf("Guardian/%s", *id))

	cfg, err := readConfig(os.Stdin)
	if err != nil {
		log.Crit("read config: %v", err)
		os.Exit(1)
	}

	g := &Guardian{
		id:          *id,
		labSocket:   *labSocket,
		paths:       cfg.Paths,
		snapshotDir: cfg.SnapshotDir,
		log:         log,
		snapshots:   make(map[string]*snapshot),
		startedAt:   time.Now(),
	}

	if err := g.run(context.Background()); err != nil {
		log.Crit("guardian exited: %v", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Internal types
// ---------------------------------------------------------------------------

// snapshot holds the last-known-good state of one watched file.
type snapshot struct {
	mu      sync.RWMutex
	sha256  string
	content []byte
	takenAt time.Time
}

// Guardian is the Guardian Angel implementation.
type Guardian struct {
	id          string
	labSocket   string
	paths       []string
	snapshotDir string
	log         *logging.Logger
	startedAt   time.Time

	conn       *ipc.Conn
	watcher    *linux.Watcher
	cpuSampler *linux.CPUSampler

	snapMu    sync.RWMutex
	snapshots map[string]*snapshot
}

// ---------------------------------------------------------------------------
// Main run loop
// ---------------------------------------------------------------------------

func (g *Guardian) run(ctx context.Context) error {
	// Dial Lab and perform HELLO handshake.
	conn, err := ipc.Dial(g.labSocket, ipc.RoleAngel)
	if err != nil {
		return fmt.Errorf("dial lab: %w", err)
	}
	g.conn = conn
	defer conn.Close()

	if err := g.register(); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	// Take initial snapshots before registering watches.
	// This ensures we have a baseline before inotify starts firing.
	g.log.Info("taking initial snapshots of %d path(s)", len(g.paths))
	for _, p := range g.paths {
		if err := g.takeSnapshot(p); err != nil {
			g.log.Warn("initial snapshot %s: %v", p, err)
		}
	}

	// Create inotify watcher.
	watcher, err := linux.NewWatcher(512)
	if err != nil {
		return fmt.Errorf("inotify: %w", err)
	}
	g.watcher = watcher
	defer watcher.Close()

	for _, p := range g.paths {
		if err := watcher.AddWatch(p, linux.DefaultWatchMask); err != nil {
			g.log.Warn("watch %s: %v", p, err)
		}
	}

	g.cpuSampler, _ = linux.NewCPUSampler()

	heartbeatTick := time.NewTicker(10 * time.Second)
	snapshotTick  := time.NewTicker(5 * time.Minute)
	defer heartbeatTick.Stop()
	defer snapshotTick.Stop()

	g.log.Info("Guardian %s ACTIVE — watching: %v", g.id, g.paths)
	// Send first heartbeat immediately so Lab transitions us to ACTIVE quickly.
	if err := g.sendHeartbeat(); err != nil {
		// First heartbeat failed — Lab is not reachable.
		return fmt.Errorf("initial heartbeat: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil

		case ev, ok := <-watcher.Events:
			if !ok {
				return fmt.Errorf("inotify channel closed unexpectedly")
			}
			g.handleFSEvent(ev)

		case watchErr := <-watcher.Errors:
			g.log.Warn("inotify: %v", watchErr)

		case <-heartbeatTick.C:
			if err := g.sendHeartbeat(); err != nil {
				// First attempt failed — try once more before exiting.
				// This filters transient socket hiccups from genuine Lab death.
				if retry := g.sendHeartbeat(); retry != nil {
					return fmt.Errorf("heartbeat failed (2 attempts): %w", retry)
				}
			}

		case <-snapshotTick.C:
			g.refreshSnapshots()
		}
	}
}

// ---------------------------------------------------------------------------
// Filesystem event handling
// ---------------------------------------------------------------------------

// handleFSEvent is the core protective logic. It runs synchronously in the
// event loop — it must be fast. Disk I/O is minimal: we read the file once
// to hash it, and write it once to restore it.
func (g *Guardian) handleFSEvent(ev *linux.InotifyEvent) {
	path := ev.Path()

	// ----------------------------------------------------------------
	// IN_DELETE_SELF / IN_MOVE_SELF
	// ----------------------------------------------------------------
	// Many editors (vim, emacs, sed -i) write a new file and rename it
	// over the original.  This generates IN_DELETE_SELF on the original
	// inode — which *removes the watch*.  Without re-adding the watch,
	// the guardian would silently stop monitoring the path.
	//
	// We: restore the file from snapshot, then re-add the watch.
	if ev.Mask&(linux.InDeleteSelf|linux.InMoveSelf) != 0 {
		g.snapMu.RLock()
		snap, known := g.snapshots[path]
		g.snapMu.RUnlock()

		if known {
			g.log.Crit("deletion/move of watched path: %s — restoring", path)
			if err := g.restoreFromSnapshot(path, snap); err != nil {
				g.log.Warn("restore %s: %v", path, err)
				g.emitEvent(ipc.SeverityCritical,
					fmt.Sprintf("detected deletion of %s — restore FAILED: %v", path, err),
					map[string]string{"path": path, "action": "restore_failed"})
				return
			}
			g.emitEvent(ipc.SeverityCritical,
				fmt.Sprintf("restored deleted path %s", path),
				map[string]string{"path": path, "action": "restored_deleted"})
		}

		// Re-add the watch regardless — the file now exists again after restore,
		// or it may have been moved to a new location we want to watch.
		if err := g.watcher.AddWatch(path, linux.DefaultWatchMask); err != nil {
			g.log.Warn("re-watch %s after delete: %v", path, err)
		} else {
			g.log.Debug("re-added inotify watch for %s after IN_DELETE_SELF", path)
		}
		return
	}

	// ----------------------------------------------------------------
	// IN_DELETE (entry removed from watched directory)
	// ----------------------------------------------------------------
	if ev.IsDelete() {
		g.snapMu.RLock()
		snap, known := g.snapshots[path]
		g.snapMu.RUnlock()
		if !known {
			return // untracked file — ignore
		}

		g.log.Crit("file deleted in watched directory: %s — restoring", path)
		if err := g.restoreFromSnapshot(path, snap); err != nil {
			g.log.Warn("restore %s: %v", path, err)
			return
		}
		g.emitEvent(ipc.SeverityCritical,
			fmt.Sprintf("restored deleted file %s", path),
			map[string]string{"path": path, "action": "restored_deleted"})
		// Re-add watch for the restored file.
		_ = g.watcher.AddWatch(path, linux.DefaultWatchMask)
		return
	}

	// ----------------------------------------------------------------
	// IN_CREATE (new file in watched directory)
	// ----------------------------------------------------------------
	if ev.IsCreate() {
		// Take a baseline snapshot for the new file and start watching it.
		if err := g.takeSnapshot(path); err != nil {
			g.log.Debug("snapshot new file %s: %v", path, err)
			return
		}
		_ = g.watcher.AddWatch(path, linux.DefaultWatchMask)
		g.log.Info("Guardian %s: new file %s — snapshot taken", g.id, path)
		return
	}

	// ----------------------------------------------------------------
	// IN_MODIFY / IN_CLOSE_WRITE
	// ----------------------------------------------------------------
	if ev.IsModify() {
		g.snapMu.RLock()
		snap, known := g.snapshots[path]
		g.snapMu.RUnlock()
		if !known {
			_ = g.takeSnapshot(path) // first time we've seen this path
			return
		}

		currentHash, err := hashFile(path)
		if err != nil {
			g.log.Warn("hash %s: %v", path, err)
			return
		}

		snap.mu.RLock()
		expectedHash := snap.sha256
		snap.mu.RUnlock()

		if currentHash == expectedHash {
			return // write was a no-op (same content) — harmless
		}

		g.log.Crit("[Angel Lab] Guardian %s detected modification of %s",
			g.id, path)
		g.log.Debug("  was: sha256:%s", expectedHash[:16])
		g.log.Debug("  now: sha256:%s", currentHash[:16])

		if err := g.restoreFromSnapshot(path, snap); err != nil {
			g.log.Warn("restore %s: %v", path, err)
			g.emitEvent(ipc.SeverityCritical,
				fmt.Sprintf("detected modification of %s — restore FAILED: %v", path, err),
				map[string]string{"path": path, "action": "restore_failed",
					"detected_sha256": currentHash})
			return
		}

		g.log.Info("[Angel Lab] Guardian %s restored modified file %s", g.id, path)
		g.emitEvent(ipc.SeverityCritical,
			fmt.Sprintf("restored modified file %s", path),
			map[string]string{
				"path":            path,
				"action":          "restored",
				"snapshot_sha256": expectedHash,
				"detected_sha256": currentHash,
			})
	}
}

// ---------------------------------------------------------------------------
// Snapshot management
// ---------------------------------------------------------------------------

// takeSnapshot reads path, computes SHA-256, and stores in memory + disk.
func (g *Guardian) takeSnapshot(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])

	snap := &snapshot{
		sha256:  hash,
		content: content,
		takenAt: time.Now(),
	}

	g.snapMu.Lock()
	g.snapshots[path] = snap
	g.snapMu.Unlock()

	if g.snapshotDir != "" {
		_ = g.writeSnapshotFile(path, content)
	}
	return nil
}

// refreshSnapshots re-snapshots all watched paths that have not changed.
// Paths that have been restored are skipped (their snapshot is already fresh).
func (g *Guardian) refreshSnapshots() {
	g.snapMu.RLock()
	paths := make([]string, 0, len(g.snapshots))
	for p := range g.snapshots {
		paths = append(paths, p)
	}
	g.snapMu.RUnlock()

	for _, p := range paths {
		currentHash, err := hashFile(p)
		if err != nil {
			continue // file may be temporarily absent
		}
		g.snapMu.RLock()
		snap := g.snapshots[p]
		g.snapMu.RUnlock()
		snap.mu.RLock()
		unchanged := snap.sha256 == currentHash
		snap.mu.RUnlock()
		if unchanged {
			_ = g.takeSnapshot(p)
		}
	}
}

// restoreFromSnapshot writes snapshot content back to path atomically.
func (g *Guardian) restoreFromSnapshot(path string, snap *snapshot) error {
	snap.mu.RLock()
	content := snap.content
	snap.mu.RUnlock()

	if content == nil {
		return fmt.Errorf("no snapshot content for %s", path)
	}

	info, err := os.Stat(path)
	var perm os.FileMode = 0644
	if err == nil {
		perm = info.Mode().Perm()
	}

	// Write atomically via a temp file in the same directory so the rename
	// is on the same filesystem (guaranteed atomic on Linux).
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".angelrestore-*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (g *Guardian) writeSnapshotFile(originalPath string, content []byte) error {
	if err := os.MkdirAll(g.snapshotDir, 0700); err != nil {
		return err
	}
	name := filepath.Base(originalPath) + ".snap"
	return os.WriteFile(filepath.Join(g.snapshotDir, name), content, 0600)
}

// ---------------------------------------------------------------------------
// IPC helpers
// ---------------------------------------------------------------------------

func (g *Guardian) register() error {
	payload, err := ipc.EncodePayload(&ipc.RegisterPayload{
		AngelID:   g.id,
		AngelType: "guardian",
		PID:       os.Getpid(),
	})
	if err != nil {
		return err
	}
	return g.conn.Send(&ipc.Message{
		Version: ipc.ProtocolVersion,
		Kind:    ipc.KindRegister,
		Payload: payload,
	})
}

func (g *Guardian) sendHeartbeat() error {
	stat, _ := linux.ReadSelfStat()
	var rss uint64
	if stat != nil {
		rss = stat.RSSBytes()
	}
	var cpu float64
	if g.cpuSampler != nil {
		cpu, _ = g.cpuSampler.Sample()
	}
	g.snapMu.RLock()
	watchedCount := len(g.snapshots)
	g.snapMu.RUnlock()

	payload, err := ipc.EncodePayload(&ipc.HeartbeatPayload{
		AngelID:    g.id,
		State:      "ACTIVE",
		Uptime:     int64(time.Since(g.startedAt).Seconds()),
		CPUPercent: cpu,
		RSSBytes:   rss,
		Goroutines: runtime.NumGoroutine(),
		FDCount:    linux.CountFDs(),
		AngelMeta:  map[string]string{"watched_paths": fmt.Sprintf("%d", watchedCount)},
	})
	if err != nil {
		return fmt.Errorf("heartbeat: encode: %w", err)
	}
	if err := g.conn.Send(&ipc.Message{
		Version: ipc.ProtocolVersion,
		Kind:    ipc.KindHeartbeat,
		Payload: payload,
	}); err != nil {
		return err
	}
	return nil
}

func (g *Guardian) emitEvent(severity ipc.Severity, message string, meta map[string]string) {
	payload, err := ipc.EncodePayload(&ipc.EventPayload{
		AngelID:   g.id,
		Severity:  severity,
		Message:   message,
		Timestamp: time.Now(),
		Meta:      meta,
	})
	if err != nil {
		g.log.Warn("encode event: %v", err)
		return
	}

	if err := g.conn.Send(&ipc.Message{
		Version: ipc.ProtocolVersion,
		Kind:    ipc.KindEvent,
		Payload: payload,
	}); err != nil {
		g.log.Warn("send event: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

// func hashFile(path string) (string, error) {
// 	f, err := os.Open(path)
// 	if err != nil {
// 		return "", err
// 	}
// 	defer f.Close()
// 	h := sha256.New()
// 	if _, err := io.Copy(h, f); err != nil {
// 		return "", err
// 	}
// 	return hex.EncodeToString(h.Sum(nil)), nil
// }
