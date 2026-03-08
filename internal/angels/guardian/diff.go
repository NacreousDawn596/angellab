// Package guardian — diff.go
//
// DiffSnapshot compares every file currently on-disk against its stored
// snapshot hash and reports differences.  This powers "lab angel diff <id>".
//
// The diff is intentionally lightweight: we only compare SHA-256 hashes and
// file sizes, not byte-by-byte content.  The goal is to give the operator a
// fast "did anything change?" answer, not a full unified diff of config files
// (those can contain secrets).
//
// Calling convention:
//
//	results, err := DiffSnapshot(snapshotDir, watchedPaths)
//
// Each DiffEntry in the result either:
//   - has Changed=true and both hashes filled in, or
//   - has Missing=true because the file no longer exists on disk, or
//   - has NoSnapshot=true because no snapshot exists yet (new file).
package guardian

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// DiffEntry describes the comparison result for a single watched file.
type DiffEntry struct {
	// Path is the absolute path of the watched file.
	Path string `json:"path" msgpack:"path"`

	// Changed is true when the current file hash differs from the snapshot.
	Changed bool `json:"changed,omitempty" msgpack:"changed"`

	// Missing is true when the file no longer exists on disk.
	Missing bool `json:"missing,omitempty" msgpack:"missing"`

	// NoSnapshot is true when no snapshot has been taken yet.
	NoSnapshot bool `json:"no_snapshot,omitempty" msgpack:"no_snapshot"`

	// SnapshotHash is the SHA-256 hex of the snapshot (if it exists).
	SnapshotHash string `json:"snapshot_hash,omitempty" msgpack:"snapshot_hash"`

	// CurrentHash is the SHA-256 hex of the live file (if readable).
	CurrentHash string `json:"current_hash,omitempty" msgpack:"current_hash"`

	// SnapshotSize and CurrentSize are byte counts (for size-delta display).
	SnapshotSize int64 `json:"snapshot_size,omitempty" msgpack:"snapshot_size"`
	CurrentSize  int64 `json:"current_size,omitempty" msgpack:"current_size"`

	// SnapshotTaken is when the snapshot file was last written.
	SnapshotTaken time.Time `json:"snapshot_taken,omitempty" msgpack:"snapshot_taken"`
}

// SizeDelta returns CurrentSize - SnapshotSize.
func (d *DiffEntry) SizeDelta() int64 { return d.CurrentSize - d.SnapshotSize }

// SnapshotAge returns a human-readable duration since the snapshot was taken.
func (d *DiffEntry) SnapshotAge() string {
	if d.SnapshotTaken.IsZero() {
		return "unknown"
	}
	return fmtAge(time.Since(d.SnapshotTaken))
}

// DiffSnapshot compares every path in watchedPaths against its snapshot in
// snapshotDir.  It returns one DiffEntry per path, but only for entries
// that are changed, missing, or have no snapshot.  Files that match their
// snapshot are omitted — the caller can infer "all clear" from an empty slice.
//
// snapshotDir is the same directory passed to guardian.Config.SnapshotDir.
// watchedPaths is the current watch list from the Guardian's config.
func DiffSnapshot(snapshotDir string, watchedPaths []string) ([]DiffEntry, error) {
	var results []DiffEntry

	for _, p := range watchedPaths {
		entry, err := diffOne(snapshotDir, p)
		if err != nil {
			// Unreadable path: surface it as a missing entry.
			results = append(results, DiffEntry{
				Path:    p,
				Missing: true,
			})
			continue
		}
		// Only include entries that differ.
		if entry.Changed || entry.Missing || entry.NoSnapshot {
			results = append(results, *entry)
		}
	}

	return results, nil
}

// diffOne performs the hash comparison for a single path.
func diffOne(snapshotDir, path string) (*DiffEntry, error) {
	entry := &DiffEntry{Path: path}

	// --- Snapshot side ---
	snapPath := snapshotFilePath(snapshotDir, path)
	snapInfo, err := os.Stat(snapPath)
	if os.IsNotExist(err) {
		entry.NoSnapshot = true
		return entry, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat snapshot %s: %w", snapPath, err)
	}

	entry.SnapshotTaken = snapInfo.ModTime()
	entry.SnapshotSize  = snapInfo.Size()

	snapHash, err := hashFile(snapPath)
	if err != nil {
		return nil, fmt.Errorf("hash snapshot %s: %w", snapPath, err)
	}
	entry.SnapshotHash = snapHash

	// --- Live file side ---
	liveInfo, err := os.Stat(path)
	if os.IsNotExist(err) {
		entry.Missing = true
		return entry, nil
	}
	if err != nil {
		// Unreadable — treat as missing.
		entry.Missing = true
		return entry, nil
	}

	entry.CurrentSize = liveInfo.Size()

	liveHash, err := hashFile(path)
	if err != nil {
		// Permission denied reading live file is unusual but possible.
		entry.Missing = true
		return entry, nil
	}
	entry.CurrentHash = liveHash

	// --- Compare ---
	if entry.SnapshotHash != entry.CurrentHash {
		entry.Changed = true
	}

	return entry, nil
}

// snapshotFilePath returns the path where the Guardian stores the snapshot
// for a given watched file.  Must stay in sync with guardian.go's logic.
func snapshotFilePath(snapshotDir, watchedPath string) string {
	// Guardian uses filepath.Base(path) + ".snap" as the snapshot filename.
	// When multiple watched files have the same basename, guardian.go uses
	// the full path hash — mirror that here.
	base := filepath.Base(watchedPath)
	candidate := filepath.Join(snapshotDir, base+".snap")

	// If a collision marker exists, use the full-path-derived name.
	// For simplicity (and because this matches guardian.go exactly), we
	// check whether a ".snap" with the base name is a valid JSON snapshot
	// for this exact path.  If not, fall back to the hashed name.
	if isSnapshotFor(candidate, watchedPath) {
		return candidate
	}
	// Hashed fallback: sha256(watchedPath) truncated to 16 hex chars.
	sum := sha256.Sum256([]byte(watchedPath))
	name := hex.EncodeToString(sum[:])[:16] + ".snap"
	return filepath.Join(snapshotDir, name)
}

// isSnapshotFor reads candidate and checks whether its "path" field matches
// watchedPath.  Returns false on any error.
func isSnapshotFor(candidate, watchedPath string) bool {
	data, err := os.ReadFile(candidate)
	if err != nil {
		return false
	}
	var meta struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		// Snapshot might be raw file content, not JSON — treat as matching
		// the base-name convention.
		return true
	}
	return meta.Path == watchedPath || meta.Path == ""
}

// hashFile computes the SHA-256 hex digest of a file.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fmtAge formats a duration as a compact human-readable age string.
// e.g. "3s", "2m", "1h14m", "2d"
func fmtAge(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
