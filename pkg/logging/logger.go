// Package logging provides a structured, levelled logger for AngelLab.
//
// Output format (ISO 8601, aligned columns):
//
//	2025-03-06T14:22:01Z  INFO  [Lab]            Angel A-03 transitioned TRAINING → ACTIVE
//	2025-03-06T14:22:04Z  CRIT  [Guardian/A-01]  Modification detected: /etc/shadow
//
// Each logger instance carries a component label that appears in the
// third column.  The Lab daemon and each angel process create their own
// Logger with the appropriate label.
//
// Log output goes to two sinks simultaneously:
//
//  1. os.Stdout  — captured by systemd/journald when run as a unit
//  2. A rotating log file at the configured path
package logging

import (
	"fmt"
	"strings"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Level represents log verbosity.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelCrit
)

// String returns the fixed-width log level label.
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO "
	case LevelWarn:
		return "WARN "
	case LevelCrit:
		return "CRIT "
	default:
		return "?????"
	}
}

// ParseLevel converts a string to a Level.
// Unrecognised strings default to LevelInfo.
func ParseLevel(s string) Level {
	switch s {
	case "debug":
		return LevelDebug
	case "info":
		return LevelInfo
	case "warn":
		return LevelWarn
	case "crit", "critical", "error":
		return LevelCrit
	default:
		return LevelInfo
	}
}

// ---------------------------------------------------------------------------
// Logger
// ---------------------------------------------------------------------------

// Logger is a structured logger for a named component.
// It is safe for concurrent use.
type Logger struct {
	mu        sync.Mutex
	component string // e.g. "Lab", "Guardian/A-01"
	minLevel  Level
	writers   []io.Writer
	// format controls the output layout.
	// "text" (default): human-readable aligned columns.
	// "json": JSON objects, one per line — for log aggregators.
	format    string
}

// SetFormat sets the log output format: "text" or "json".
// Must be called before the logger starts emitting lines (not thread-safe
// if called concurrently with logging).
func (l *Logger) SetFormat(f string) {
	l.format = f
}

// SetLevel updates the minimum log level.
func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	l.minLevel = level
	l.mu.Unlock()
}

// New creates a Logger that writes to all provided writers.
// component is the label shown in the third column.
func New(component string, minLevel Level, writers ...io.Writer) *Logger {
	return &Logger{
		component: component,
		minLevel:  minLevel,
		writers:   writers,
		format:    "text",
	}
}

// NewJSON creates a Logger that emits JSON lines for log aggregators.
func NewJSON(component string, minLevel Level, writers ...io.Writer) *Logger {
	l := New(component, minLevel, writers...)
	l.format = "json"
	return l
}

// NewDefault creates a Logger writing to stdout only.
// Useful for angel processes before a file sink is available.
func NewDefault(component string) *Logger {
	return New(component, LevelInfo, os.Stdout)
}

// Debug logs at DEBUG level.
func (l *Logger) Debug(msg string, args ...any) {
	l.log(LevelDebug, msg, args...)
}

// Info logs at INFO level.
func (l *Logger) Info(msg string, args ...any) {
	l.log(LevelInfo, msg, args...)
}

// Warn logs at WARN level.
func (l *Logger) Warn(msg string, args ...any) {
	l.log(LevelWarn, msg, args...)
}

// Crit logs at CRIT level.  Use for active protective actions taken by angels.
func (l *Logger) Crit(msg string, args ...any) {
	l.log(LevelCrit, msg, args...)
}

// AngelEvent emits a specially formatted line for monitored events.
// This is what operators see in the live event stream:
//
//	[Angel Lab]
//	Guardian A-01 restored modified file /etc/shadow
func (l *Logger) AngelEvent(angelType, angelID, message string) {
	l.log(LevelCrit,
		fmt.Sprintf("%s %s %s", angelType, angelID, message),
	)
}

func (l *Logger) log(level Level, msg string, args ...any) {
	if level < l.minLevel {
		return
	}

	formatted := msg
	if len(args) > 0 {
		formatted = fmt.Sprintf(msg, args...)
	}

	now := time.Now().UTC()
	var line string

	if l.format == "json" {
		// JSON Lines format — one JSON object per log entry.
		// Compatible with Loki, Splunk, Datadog, and most log aggregators.
		//
		// {"ts":"2025-03-06T14:22:01Z","level":"INFO","component":"Lab","msg":"..."}
		//
		// We hand-roll the JSON to avoid importing encoding/json in the hot path.
		// The component and message are escaped via jsonEscapeString.
		lvl := strings.TrimRight(level.String(), " ")
		line = fmt.Sprintf(`{"ts":%q,"level":%q,"component":%q,"msg":%q}`+"\n",
			now.Format(time.RFC3339Nano),
			lvl,
			l.component,
			formatted,
		)
	} else {
		// Fixed-width columns: timestamp (20) + level (5) + component (padded to 18)
		line = fmt.Sprintf("%-20s  %s  [%-16s]  %s\n",
			now.Format("2006-01-02T15:04:05Z"),
			level.String(),
			l.component,
			formatted,
		)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	for _, w := range l.writers {
		_, _ = io.WriteString(w, line)
	}
}

// ---------------------------------------------------------------------------
// Rotating file sink
// ---------------------------------------------------------------------------

// RotatingFile is an io.WriteCloser that rotates once the file exceeds
// MaxBytes.  It keeps at most MaxBackups old files with a timestamp suffix.
type RotatingFile struct {
	mu         sync.Mutex
	path       string
	maxBytes   int64
	maxBackups int
	file       *os.File
	size       int64
}

// OpenRotating opens path for appending, creating it if necessary.
func OpenRotating(path string, maxBytes int64, maxBackups int) (*RotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, fmt.Errorf("logging: mkdir %s: %w", filepath.Dir(path), err)
	}
	f, size, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	return &RotatingFile{
		path:       path,
		maxBytes:   maxBytes,
		maxBackups: maxBackups,
		file:       f,
		size:       size,
	}, nil
}

// Write implements io.Writer.  It rotates the file if the size threshold
// would be exceeded.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}

	n, err := r.file.Write(p)
	r.size += int64(n)
	return n, err
}

// Close flushes and closes the current log file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}

func (r *RotatingFile) rotate() error {
	if r.file != nil {
		_ = r.file.Close()
	}
	// Rename current file: angellab.log → angellab.log.2025-03-06T14:22:01Z
	backup := r.path + "." + time.Now().UTC().Format("2006-01-02T15-04-05Z")
	_ = os.Rename(r.path, backup)

	// Prune oldest backups.
	_ = pruneBackups(r.path, r.maxBackups)

	f, _, err := openAppend(r.path)
	if err != nil {
		return err
	}
	r.file = f
	r.size = 0
	return nil
}

func openAppend(path string) (*os.File, int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return nil, 0, fmt.Errorf("logging: open %s: %w", path, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

func pruneBackups(base string, keep int) error {
	dir := filepath.Dir(base)
	prefix := filepath.Base(base) + "."

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	var backups []string
	for _, e := range entries {
		if !e.IsDir() && len(e.Name()) > len(prefix) &&
			e.Name()[:len(prefix)] == prefix {
			backups = append(backups, filepath.Join(dir, e.Name()))
		}
	}
	// entries from ReadDir are already sorted lexicographically.
	// Oldest backups have smaller timestamps (earlier in sort order).
	for len(backups) > keep {
		_ = os.Remove(backups[0])
		backups = backups[1:]
	}
	return nil
}
