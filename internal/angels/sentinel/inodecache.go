// Package sentinel — inodecache.go
//
// The InodeCache wraps linux.BuildInodeMap() with a TTL so we only
// traverse /proc/<pid>/fd every rebuildInterval seconds instead of
// every poll cycle.
//
// Cost profile (approximate, 200-process machine):
//
//	Full BuildInodeMap():  ~4ms  (ReadDir × N pids + Readlink × M fds)
//	Cached lookup:         ~0µs  (map read under RLock)
//
// At a 2s poll interval with a 10s rebuild interval, we pay the
// /proc traversal cost once every 5 polls — an ~80% reduction.
package sentinel

import (
	"sync"
	"time"

	"github.com/nacreousdawn596/angellab/pkg/linux"
)

// rebuildInterval is how often the inode→process map is refreshed.
// 10 seconds is a good default: short-lived processes (curl, wget) typically
// complete well within this window, so they'll appear in at least one map.
const rebuildInterval = 10 * time.Second

// InodeCache holds a cached InodeMap and rebuilds it on demand.
type InodeCache struct {
	mu          sync.RWMutex
	m           linux.InodeMap
	builtAt     time.Time
	interval    time.Duration
}

// NewInodeCache creates an InodeCache with the default rebuild interval.
// The map is built immediately on first Get() call, not in the constructor,
// so startup is fast.
func NewInodeCache() *InodeCache {
	return &InodeCache{interval: rebuildInterval}
}

// Get returns the current InodeMap, rebuilding it if the TTL has expired.
// Safe for concurrent use from the poll goroutine and any other reader.
func (c *InodeCache) Get() linux.InodeMap {
	// Fast path: check under read lock.
	c.mu.RLock()
	m := c.m
	age := time.Since(c.builtAt)
	c.mu.RUnlock()

	if m != nil && age < c.interval {
		return m
	}

	// Slow path: rebuild outside the lock so we don't block readers during
	// the (potentially slow) /proc walk.
	fresh := linux.BuildInodeMap()

	c.mu.Lock()
	// Check again — another goroutine may have rebuilt while we were walking.
	if time.Since(c.builtAt) >= c.interval {
		c.m = fresh
		c.builtAt = time.Now()
	} else {
		fresh = c.m // use the map the other goroutine built
	}
	c.mu.Unlock()

	return fresh
}

// Invalidate forces the next Get() to rebuild the map immediately.
// Call this after spawning a process whose sockets you want to track right away.
func (c *InodeCache) Invalidate() {
	c.mu.Lock()
	c.builtAt = time.Time{}
	c.mu.Unlock()
}
