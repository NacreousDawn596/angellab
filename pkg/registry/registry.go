// Package registry manages persistent state for the Lab daemon.
//
// It owns two SQLite tables:
//
//	angels — one row per managed angel (source of truth for lifecycle)
//	events — append-only event log emitted by angels
//
// All mutating operations are serialised through a single *sql.DB connection
// with WAL mode enabled, giving safe concurrent reads from CLI queries while
// Lab writes proceed without blocking.
package registry

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3" // register sqlite3 driver
)

// ---------------------------------------------------------------------------
// Schema
// ---------------------------------------------------------------------------

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS angels (
    id            TEXT PRIMARY KEY,
    angel_type    TEXT    NOT NULL,
    state         TEXT    NOT NULL DEFAULT 'CREATED',
    pid           INTEGER,
    created_at    DATETIME NOT NULL,
    updated_at    DATETIME NOT NULL,
    restart_count INTEGER  NOT NULL DEFAULT 0,
    last_seen     DATETIME,
    config_json   TEXT
);

CREATE TABLE IF NOT EXISTS events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    angel_id    TEXT     NOT NULL,
    severity    TEXT     NOT NULL,
    message     TEXT     NOT NULL,
    meta_json   TEXT,
    occurred_at DATETIME NOT NULL,
    FOREIGN KEY (angel_id) REFERENCES angels(id)
);

CREATE INDEX IF NOT EXISTS idx_events_angel   ON events(angel_id);
CREATE INDEX IF NOT EXISTS idx_events_ts      ON events(occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_angels_state   ON angels(state);
`

// ---------------------------------------------------------------------------
// Angel state constants (mirrors ipc package, kept here to avoid import cycle)
// ---------------------------------------------------------------------------

// AngelState represents a point in the angel lifecycle FSM.
type AngelState string

const (
	StateCreated    AngelState = "CREATED"
	StateTraining   AngelState = "TRAINING"
	StateActive     AngelState = "ACTIVE"
	StateUnstable   AngelState = "UNSTABLE"
	StateContained  AngelState = "CONTAINED"
	StateTerminated AngelState = "TERMINATED"
)

// ---------------------------------------------------------------------------
// Row types
// ---------------------------------------------------------------------------

// Angel is a registry row corresponding to one managed angel process.
type Angel struct {
	ID           string
	AngelType    string
	State        AngelState
	PID          int
	CreatedAt    time.Time
	UpdatedAt    time.Time
	RestartCount int
	LastSeen     *time.Time
	ConfigJSON   string
}

// Event is a registry row corresponding to one event emitted by an angel.
type Event struct {
	ID         int64
	AngelID    string
	Severity   string
	Message    string
	MetaJSON   string
	OccurredAt time.Time
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// Registry is a handle to the SQLite database.
// All exported methods are safe for concurrent use.
type Registry struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path and applies the schema.
func Open(path string) (*Registry, error) {
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("registry: open %s: %w", path, err)
	}
	// One writer at a time; WAL allows concurrent readers.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("registry: apply schema: %w", err)
	}
	return &Registry{db: db}, nil
}

// Close closes the underlying database connection.
func (r *Registry) Close() error {
	return r.db.Close()
}

// ---------------------------------------------------------------------------
// Angel CRUD
// ---------------------------------------------------------------------------

// InsertAngel writes a new angel row with state=CREATED.
func (r *Registry) InsertAngel(a *Angel) error {
	now := time.Now().UTC()
	_, err := r.db.Exec(
		`INSERT INTO angels (id, angel_type, state, pid, created_at, updated_at, config_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.AngelType, StateCreated, 0, now, now, a.ConfigJSON,
	)
	if err != nil {
		return fmt.Errorf("registry: insert angel %s: %w", a.ID, err)
	}
	return nil
}

// GetAngel retrieves an angel row by ID.
// Returns sql.ErrNoRows (wrapped) if not found.
func (r *Registry) GetAngel(id string) (*Angel, error) {
	row := r.db.QueryRow(
		`SELECT id, angel_type, state, pid, created_at, updated_at,
		        restart_count, last_seen, config_json
		 FROM angels WHERE id = ?`, id,
	)
	return scanAngel(row)
}

// ListAngels returns all angel rows ordered by created_at ascending.
func (r *Registry) ListAngels() ([]*Angel, error) {
	rows, err := r.db.Query(
		`SELECT id, angel_type, state, pid, created_at, updated_at,
		        restart_count, last_seen, config_json
		 FROM angels ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("registry: list angels: %w", err)
	}
	defer rows.Close()

	var angels []*Angel
	for rows.Next() {
		a, err := scanAngel(rows)
		if err != nil {
			return nil, err
		}
		angels = append(angels, a)
	}
	return angels, rows.Err()
}

// UpdateState sets the angel's state and updated_at timestamp.
func (r *Registry) UpdateState(id string, state AngelState) error {
	_, err := r.db.Exec(
		`UPDATE angels SET state = ?, updated_at = ? WHERE id = ?`,
		state, time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("registry: update state %s → %s: %w", id, state, err)
	}
	return nil
}

// UpdatePID records the OS PID of a live angel process.
func (r *Registry) UpdatePID(id string, pid int) error {
	_, err := r.db.Exec(
		`UPDATE angels SET pid = ?, updated_at = ? WHERE id = ?`,
		pid, time.Now().UTC(), id,
	)
	return err
}

// TouchLastSeen updates last_seen to now for the given angel.
func (r *Registry) TouchLastSeen(id string) error {
	now := time.Now().UTC()
	_, err := r.db.Exec(
		`UPDATE angels SET last_seen = ?, updated_at = ? WHERE id = ?`,
		now, now, id,
	)
	return err
}

// IncrementRestarts atomically increments restart_count and returns the new value.
func (r *Registry) IncrementRestarts(id string) (int, error) {
	_, err := r.db.Exec(
		`UPDATE angels SET restart_count = restart_count + 1, updated_at = ? WHERE id = ?`,
		time.Now().UTC(), id,
	)
	if err != nil {
		return 0, fmt.Errorf("registry: increment restarts %s: %w", id, err)
	}
	a, err := r.GetAngel(id)
	if err != nil {
		return 0, err
	}
	return a.RestartCount, nil
}

// ---------------------------------------------------------------------------
// Event log
// ---------------------------------------------------------------------------

// InsertEvent appends an event to the events table.
func (r *Registry) InsertEvent(e *Event) error {
	_, err := r.db.Exec(
		`INSERT INTO events (angel_id, severity, message, meta_json, occurred_at)
		 VALUES (?, ?, ?, ?, ?)`,
		e.AngelID, e.Severity, e.Message, e.MetaJSON, e.OccurredAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("registry: insert event: %w", err)
	}
	return nil
}

// ListEvents returns the n most recent events for an angel.
// If angelID is empty, events from all angels are returned.
func (r *Registry) ListEvents(angelID string, n int) ([]*Event, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if angelID == "" {
		rows, err = r.db.Query(
			`SELECT id, angel_id, severity, message, meta_json, occurred_at
			 FROM events ORDER BY occurred_at DESC LIMIT ?`, n,
		)
	} else {
		rows, err = r.db.Query(
			`SELECT id, angel_id, severity, message, meta_json, occurred_at
			 FROM events WHERE angel_id = ? ORDER BY occurred_at DESC LIMIT ?`,
			angelID, n,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("registry: list events: %w", err)
	}
	defer rows.Close()

	var events []*Event
	for rows.Next() {
		var ev Event
		var metaJSON sql.NullString
		if err := rows.Scan(&ev.ID, &ev.AngelID, &ev.Severity,
			&ev.Message, &metaJSON, &ev.OccurredAt); err != nil {
			return nil, fmt.Errorf("registry: scan event: %w", err)
		}
		if metaJSON.Valid {
			ev.MetaJSON = metaJSON.String
		}
		events = append(events, &ev)
	}
	return events, rows.Err()
}

// ---------------------------------------------------------------------------
// Boot recovery helpers
// ---------------------------------------------------------------------------

// ListRecoverableAngels returns angels that were ACTIVE or TRAINING at the
// time Lab last exited. These are candidates for respawning on startup.
func (r *Registry) ListRecoverableAngels() ([]*Angel, error) {
	rows, err := r.db.Query(
		`SELECT id, angel_type, state, pid, created_at, updated_at,
		        restart_count, last_seen, config_json
		 FROM angels WHERE state IN ('ACTIVE', 'TRAINING')
		 ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("registry: list recoverable: %w", err)
	}
	defer rows.Close()

	var angels []*Angel
	for rows.Next() {
		a, err := scanAngel(rows)
		if err != nil {
			return nil, err
		}
		angels = append(angels, a)
	}
	return angels, rows.Err()
}

// MarkStale sets all non-TERMINATED angels to CREATED and clears their PIDs.
// Called once at Lab startup before boot recovery begins, so that stale
// ACTIVE entries from the previous run are properly reconciled.
func (r *Registry) MarkStale() error {
	_, err := r.db.Exec(
		`UPDATE angels SET state = 'CREATED', pid = 0, updated_at = ?
		 WHERE state NOT IN ('TERMINATED')`,
		time.Now().UTC(),
	)
	return err
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanAngel(s scanner) (*Angel, error) {
	var a Angel
	var lastSeen sql.NullTime
	var configJSON sql.NullString
	err := s.Scan(
		&a.ID, &a.AngelType, &a.State, &a.PID,
		&a.CreatedAt, &a.UpdatedAt, &a.RestartCount,
		&lastSeen, &configJSON,
	)
	if err != nil {
		return nil, fmt.Errorf("registry: scan angel: %w", err)
	}
	if lastSeen.Valid {
		a.LastSeen = &lastSeen.Time
	}
	if configJSON.Valid {
		a.ConfigJSON = configJSON.String
	}
	return &a, nil
}
