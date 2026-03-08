// Package sentinel — dedup.go
//
// Deduplicator prevents the Sentinel from emitting repeated events for the
// same connection.  Because we poll /proc/net/tcp every ~2 seconds, a single
// long-lived connection would otherwise produce hundreds of identical alerts.
//
// Design:
//
//	Each time we "see" a connection we record the time.
//	A connection is considered "new" if:
//	  (a) it was never seen before, OR
//	  (b) it was last seen more than ttl seconds ago (possible reconnect).
//
//	Expired entries are pruned on every Prune() call to keep memory bounded.
//	Prune is called once per poll cycle.
package sentinel

import (
	"sort"
	"sync"
	"time"
)

const defaultDedupTTL = 5 * time.Minute
const maxDedupSize = 100_000

type Deduplicator struct {
	mu          sync.Mutex
	seen        map[[6]byte]time.Time
	ttl         time.Duration
	pruneAt     int
	calls       int
	lastPruneAt time.Time // wall clock of last prune — skip prune if nothing can expire
}

func NewDeduplicator() *Deduplicator {
	return &Deduplicator{
		seen:        make(map[[6]byte]time.Time, 256),
		ttl:         defaultDedupTTL,
		pruneAt:     500, // prune every 500 calls instead of 100
		lastPruneAt: time.Now(),
	}
}

func (d *Deduplicator) IsNew(oc OutboundConn, now time.Time) bool {
	key := oc.CompactKey()

	d.mu.Lock()
	defer d.mu.Unlock()

	last, seen := d.seen[key]

	if !seen {
		d.seen[key] = now
		d.calls++
		if d.calls >= d.pruneAt {
			if now.Sub(d.lastPruneAt) >= d.ttl/4 {
				d.prune(now)
				d.lastPruneAt = now
			}
			d.calls = 0
		}
		if len(d.seen) >= maxDedupSize {
			d.pruneHalf(now)
		}
		return true
	}

	expired := now.Sub(last) > d.ttl
	if expired {
		// Refresh the timestamp only when the entry has expired.
		d.seen[key] = now
	}
	return expired
}

func (d *Deduplicator) Reset() {
	d.mu.Lock()
	d.seen = make(map[[6]byte]time.Time, 256)
	d.mu.Unlock()
}

func (d *Deduplicator) Size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}

func (d *Deduplicator) prune(now time.Time) {
	for key, last := range d.seen {
		if now.Sub(last) > d.ttl {
			delete(d.seen, key)
		}
	}
}

func (d *Deduplicator) pruneHalf(now time.Time) {
	type entry struct {
		key  [6]byte
		last time.Time
	}

	entries := make([]entry, 0, len(d.seen))

	for k, t := range d.seen {
		entries = append(entries, entry{k, t})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].last.Before(entries[j].last)
	})

	keep := len(entries) / 2

	newMap := make(map[[6]byte]time.Time, keep)

	for _, e := range entries[keep:] {
		newMap[e.key] = e.last
	}

	d.seen = newMap
}

// ---------------------------------------------------------------------------
// Rate tracker — detects connection bursts
// ---------------------------------------------------------------------------

// RateTracker counts events per time window and detects spikes.
// Used to catch connection storms: >N new connections in W seconds.
//
// Implementation: a pre-allocated ring-like slice with a logical start
// index so eviction is amortised O(1) (advance startIdx, never re-slice).
type RateTracker struct {
	mu       sync.Mutex
	window   time.Duration
	events   []time.Time // circular-ish buffer of event timestamps
	startIdx int         // logical start: events[startIdx:] are valid
	maxSeen  int         // max count seen in any window (for baseline)
}

const maxTrackerEvents = 4096 // cap to bound memory

// NewRateTracker creates a RateTracker with the given window duration.
func NewRateTracker(window time.Duration) *RateTracker {
	return &RateTracker{
		window: window,
		events: make([]time.Time, 0, 128),
	}
}

// Add records one event and returns the count of events in the current window.
func (r *RateTracker) Add() int {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()

	// Evict expired events by advancing startIdx (O(1) amortized).
	r.evict(now)

	// Append new event, capping at maxTrackerEvents to bound memory.
	if len(r.events)-r.startIdx < maxTrackerEvents {
		r.events = append(r.events, now)
	}
	count := len(r.events) - r.startIdx
	if count > r.maxSeen {
		r.maxSeen = count
	}
	return count
}

// Count returns the number of events recorded within the current window.
func (r *RateTracker) Count() int {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evict(now)
	return len(r.events) - r.startIdx
}

// MaxSeen returns the peak event count in any window (for baseline building).
func (r *RateTracker) MaxSeen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxSeen
}

// Reset clears all recorded events and the peak.
func (r *RateTracker) Reset() {
	r.mu.Lock()
	r.events = r.events[:0]
	r.startIdx = 0
	r.maxSeen = 0
	r.mu.Unlock()
}

// evict advances startIdx past events older than the window.
// Must be called with r.mu held. O(1) amortized: we compact when startIdx > half the slice.
func (r *RateTracker) evict(now time.Time) {
	cutoff := now.Add(-r.window)
	for r.startIdx < len(r.events) && r.events[r.startIdx].Before(cutoff) {
		r.startIdx++
	}
	// Compact: when we've consumed more than half the allocated slice, copy
	// the live portion to the front. This keeps memory bounded and avoids
	// the slice growing without bound during sustained bursts.
	if r.startIdx > len(r.events)/2 && r.startIdx > 64 {
		copy(r.events, r.events[r.startIdx:])
		r.events = r.events[:len(r.events)-r.startIdx]
		r.startIdx = 0
	}
}
