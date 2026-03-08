// Package lab — connection.go
//
// ConnState tracks the health of the IPC link between Lab and each angel.
// It is orthogonal to AngelState (which tracks the lifecycle): an angel
// can be ACTIVE (lifecycle) but LOST (connection) during a reconnect window.
//
// State machine:
//
//	               spawn process
//	DISCONNECTED ─────────────────► CONNECTING
//	                                    │
//	                               REGISTER msg
//	                                    │
//	                                    ▼
//	                               REGISTERED ──── first HEARTBEAT ──► ACTIVE
//	                                    │                                  │
//	                               conn drop                          conn drop
//	                                    │                                  │
//	                                    └──────────────┬───────────────────┘
//	                                                   ▼
//	                                                  LOST
//	                                                   │
//	                                    ┌──── within recovery window?
//	                                    │                  │
//	                                  yes                  no
//	                                    │                  │
//	                                    ▼                  ▼
//	                              RECOVERING          (restart policy)
//	                                    │
//	                            REGISTER received
//	                                    │
//	                               REGISTERED → ACTIVE (as above)
package lab

import (
	"fmt"
	"sync"
	"time"
)

// ConnState describes the current IPC link status for one angel.
type ConnState uint8

const (
	// ConnStateDisconnected is the zero value — process not yet started.
	ConnStateDisconnected ConnState = iota
	// ConnStateConnecting — process started, REGISTER not yet received.
	ConnStateConnecting
	// ConnStateRegistered — REGISTER received, awaiting first heartbeat.
	ConnStateRegistered
	// ConnStateActive — heartbeats arriving within the expected window.
	ConnStateActive
	// ConnStateLost — connection dropped; recovery window active.
	ConnStateLost
	// ConnStateRecovering — within recovery window, angel may reconnect.
	ConnStateRecovering
)

// String returns a fixed-width label for display in the CLI.
func (s ConnState) String() string {
	switch s {
	case ConnStateDisconnected:
		return "DISCONNECTED"
	case ConnStateConnecting:
		return "CONNECTING"
	case ConnStateRegistered:
		return "REGISTERED"
	case ConnStateActive:
		return "ACTIVE"
	case ConnStateLost:
		return "LOST"
	case ConnStateRecovering:
		return "RECOVERING"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", s)
	}
}

// ---------------------------------------------------------------------------
// ConnTracker manages the recovery window for one angel.
// ---------------------------------------------------------------------------

// recoveryWindow is how long Lab waits for an angel to reconnect after an
// unexpected connection drop before applying the restart policy.
const recoveryWindow = 10 * time.Second

// ConnTracker holds the connection state for a single angel and manages
// the recovery timer.  It is embedded in AngelEntry.
type ConnTracker struct {
	mu    sync.Mutex
	state ConnState

	// lostAt records when the connection was last lost.
	// Used to compute whether we are still inside the recovery window.
	lostAt time.Time

	// onRecoveryExpired is called (in a goroutine) when the recovery window
	// closes without a reconnect.  Typically triggers the restart policy.
	onRecoveryExpired func()

	// recoveryTimer is the active recovery deadline timer, if any.
	recoveryTimer *time.Timer
}

// State returns the current ConnState (safe for concurrent reads).
func (ct *ConnTracker) State() ConnState {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return ct.state
}

// Transition moves the tracker to newState.
// Invalid transitions are no-ops and return an error (for logging).
func (ct *ConnTracker) Transition(newState ConnState) error {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if !validTransition(ct.state, newState) {
		return fmt.Errorf("conn: invalid transition %s → %s", ct.state, newState)
	}
	ct.state = newState
	return nil
}

// MarkLost transitions to LOST, starts the recovery window timer, and sets
// the tracker to RECOVERING once the window opens.
// If the angel reconnects (MarkRecovered) before the timer fires, the timer
// is cancelled.
func (ct *ConnTracker) MarkLost(onExpired func()) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if ct.state == ConnStateDisconnected ||
		ct.state == ConnStateLost ||
		ct.state == ConnStateRecovering {
		return // already in a lost state
	}

	ct.state = ConnStateLost
	ct.lostAt = time.Now()
	ct.onRecoveryExpired = onExpired

	// Immediately open the recovery window.
	ct.state = ConnStateRecovering

	// Cancel any previous timer (shouldn't exist, but be safe).
	if ct.recoveryTimer != nil {
		ct.recoveryTimer.Stop()
	}
	ct.recoveryTimer = time.AfterFunc(recoveryWindow, func() {
		ct.mu.Lock()
		expired := ct.state == ConnStateRecovering
		ct.mu.Unlock()
		if expired && onExpired != nil {
			onExpired()
		}
	})
}

// MarkRecovered cancels the recovery timer and transitions to REGISTERED.
// Returns true if we were in the recovery window (reconnect was expected).
func (ct *ConnTracker) MarkRecovered() bool {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	wasRecovering := ct.state == ConnStateRecovering
	if ct.recoveryTimer != nil {
		ct.recoveryTimer.Stop()
		ct.recoveryTimer = nil
	}
	ct.state = ConnStateRegistered
	return wasRecovering
}

// validTransition returns true if old → new is a legal state transition.
func validTransition(old, new ConnState) bool {
	// Build the allowed-transition table.
	allowed := map[ConnState][]ConnState{
		ConnStateDisconnected: {ConnStateConnecting},
		ConnStateConnecting:   {ConnStateRegistered, ConnStateLost, ConnStateRecovering},
		ConnStateRegistered:   {ConnStateActive, ConnStateLost, ConnStateRecovering},
		ConnStateActive:       {ConnStateLost, ConnStateRecovering},
		ConnStateLost:         {ConnStateRecovering, ConnStateConnecting},
		ConnStateRecovering:   {ConnStateRegistered, ConnStateConnecting},
	}
	for _, s := range allowed[old] {
		if s == new {
			return true
		}
	}
	return false
}
