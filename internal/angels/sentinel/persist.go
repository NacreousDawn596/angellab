// Package sentinel — persist.go
//
// Baseline persistence: when the training window closes, the frozen
// baseline is serialised to a JSON file under the angel's state directory.
// On next startup the Sentinel loads this file and skips training entirely,
// transitioning directly to ACTIVE.
//
// File location: /var/lib/angellab/sentinel-<id>-baseline.json
//
// Schema (JSON):
//
//	{
//	  "angel_id": "A-02",
//	  "frozen_at": "2025-03-06T14:22:01Z",
//	  "ips": ["8.8.8.8", "1.1.1.1", ...],
//	  "ports": [80, 443, 53, ...],
//	  "max_concurrent": 42,
//	  "samples": 180
//	}
//
// The file is written atomically (temp + rename) to prevent partial reads
// if labd is killed mid-write.
package sentinel

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// baselineSchemaVersion is incremented whenever the scoring logic changes
// in a way that makes old baselines incompatible.  The Sentinel refuses to
// load a baseline whose version != baselineSchemaVersion, forcing retraining.
const baselineSchemaVersion = 1

// baselineRecord is the on-disk JSON representation of a frozen Baseline.
type baselineRecord struct {
	// Version is the schema version — used to reject stale baselines after
	// scoring logic changes.  Always written as baselineSchemaVersion.
	Version       int       `json:"version"`
	AngelID       string    `json:"angel_id"`
	// CreatedAt is when the baseline was frozen.  Used for display only.
	CreatedAt     time.Time `json:"created_at"`
	IPs           []string  `json:"ips"`
	Ports         []uint16  `json:"ports"`
	MaxConcurrent int       `json:"max_concurrent"`
	Samples       int       `json:"samples"`
}

// saveBaseline serialises b to a JSON file in stateDir.
// The write is atomic: a temporary file is written first, then renamed.
func saveBaseline(angelID, stateDir string, b *Baseline) error {
	if err := os.MkdirAll(stateDir, 0750); err != nil {
		return fmt.Errorf("persist: mkdir %s: %w", stateDir, err)
	}

	b.mu.RLock()
	rec := baselineRecord{
		Version:       baselineSchemaVersion,
		AngelID:       angelID,
		CreatedAt:     b.frozenAt,
		MaxConcurrent: b.maxConcurrent,
		Samples:       b.samples,
	}
	rec.IPs = make([]string, 0, len(b.knownIPs))
	for k := range b.knownIPs {
		ip := net.IP(k[:])
		rec.IPs = append(rec.IPs, ip.String())
	}
	rec.Ports = make([]uint16, 0, len(b.knownPorts))
	for port := range b.knownPorts {
		rec.Ports = append(rec.Ports, port)
	}
	b.mu.RUnlock()

	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("persist: marshal: %w", err)
	}

	dest := baselinePath(angelID, stateDir)
	tmp := dest + ".tmp"

	if err := os.WriteFile(tmp, data, 0640); err != nil {
		return fmt.Errorf("persist: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("persist: rename to %s: %w", dest, err)
	}
	return nil
}

// loadBaseline reads a previously-saved baseline from stateDir.
// Returns (nil, nil) if no baseline file exists — caller should train.
// Returns (nil, err) on parse errors.
func loadBaseline(angelID, stateDir string) (*Baseline, error) {
	path := baselinePath(angelID, stateDir)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil // no baseline on disk — need training
	}
	if err != nil {
		return nil, fmt.Errorf("persist: read %s: %w", path, err)
	}

	var rec baselineRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("persist: unmarshal %s: %w", path, err)
	}

	// Reject baselines written by a different schema version.
	// This prevents stale baselines from silently causing wrong scoring
	// when the detection logic changes between releases.
	if rec.Version != baselineSchemaVersion {
		return nil, fmt.Errorf(
			"persist: baseline schema v%d is incompatible with current v%d — will retrain",
			rec.Version, baselineSchemaVersion)
	}

	b := NewBaseline()
	b.maxConcurrent = rec.MaxConcurrent
	b.samples = rec.Samples
	b.frozenAt = rec.CreatedAt

	for _, ipStr := range rec.IPs {
		ip := net.ParseIP(ipStr)
		if ip != nil {
			b.knownIPs[ipKey(ip)] = struct{}{}
		}
	}
	for _, port := range rec.Ports {
		b.knownPorts[port] = struct{}{}
	}

	// Mark frozen — this baseline is ready to use immediately.
	b.frozen = true

	return b, nil
}

// baselinePath returns the full path for the baseline file.
func baselinePath(angelID, stateDir string) string {
	return filepath.Join(stateDir, fmt.Sprintf("sentinel-%s-baseline.json", angelID))
}
