// Package sentinel — baseline.go
//
// The Baseline model learns the normal network behaviour of this host
// during the TRAINING phase (configurable duration, default 60s).
//
// What is learned:
//
//   - Set of remote IPs seen in outbound ESTABLISHED connections.
//   - Set of remote ports seen in those connections.
//   - Histogram of concurrent connection count (for spike detection).
//   - Per-second connection rate (for burst detection).
//
// After the training window closes, the model becomes immutable and
// is used as the reference for anomaly scoring.  The Sentinel transitions
// from TRAINING to ACTIVE and begins emitting events.
//
// We keep the model deliberately simple.  Accuracy improves with longer
// baseline windows; the default 60s is a reasonable minimum for a demo.
// In production you would want 24h+ to cover daily traffic patterns.
package sentinel

import (
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Baseline
// ---------------------------------------------------------------------------

// Baseline holds the learned normal-behaviour model.
// After Freeze() is called it is safe to read concurrently from any goroutine.
type Baseline struct {
	mu sync.RWMutex

	// knownIPs is the set of remote IPv4 addresses seen during training.
	// Stored as [4]byte to avoid ip.String() allocations on every lookup.
	knownIPs map[[4]byte]struct{}

	// knownPorts is the set of remote ports seen during training.
	knownPorts map[uint16]struct{}

	// maxConcurrent is the peak concurrent connection count during training.
	maxConcurrent int

	// samples is the number of poll cycles used to build the baseline.
	samples int

	// frozen is set to true by Freeze(). After that, readers skip the mutex.
	frozen bool

	// frozenAt records when the baseline was frozen (end of training).
	frozenAt time.Time
}

// NewBaseline creates an empty, mutable Baseline.
func NewBaseline() *Baseline {
	return &Baseline{
		knownIPs:   make(map[[4]byte]struct{}),
		knownPorts: make(map[uint16]struct{}),
	}
}

// ipKey returns the 4-byte key used for IP lookups in knownIPs.
// If ip is a 16-byte IPv4-in-IPv6 form, we extract the IPv4 tail.
func ipKey(ip net.IP) [4]byte {
	if ip4 := ip.To4(); ip4 != nil {
		return [4]byte{ip4[0], ip4[1], ip4[2], ip4[3]}
	}
	// Fallback for non-IPv4 (shouldn't occur in tcp, but safe).
	var k [4]byte
	copy(k[:], ip)
	return k
}

// Observe feeds one poll snapshot to the model during the training phase.
// It is a no-op after Freeze() is called.
func (b *Baseline) Observe(conns []OutboundConn) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.frozen {
		return
	}

	b.samples++
	if len(conns) > b.maxConcurrent {
		b.maxConcurrent = len(conns)
	}

	for _, c := range conns {
		b.knownIPs[ipKey(c.RemoteIP)] = struct{}{}
		b.knownPorts[c.RemotePort] = struct{}{}
	}
}

// Freeze marks the baseline as complete.
// After Freeze, Observe is a no-op and all read methods are lock-free safe.
func (b *Baseline) Freeze() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.frozen = true
	b.frozenAt = time.Now()
}

// IsFrozen reports whether the baseline has been frozen.
func (b *Baseline) IsFrozen() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.frozen
}

// KnowsIP reports whether ip was seen during training.
// After Freeze(), reads are lock-free (the map is immutable).
func (b *Baseline) KnowsIP(ip net.IP) bool {
	key := ipKey(ip)
	if b.frozen {
		_, ok := b.knownIPs[key]
		return ok
	}
	b.mu.RLock()
	_, ok := b.knownIPs[key]
	b.mu.RUnlock()
	return ok
}

// KnowsPort reports whether port was seen during training.
// After Freeze(), reads are lock-free (the map is immutable).
func (b *Baseline) KnowsPort(port uint16) bool {
	if b.frozen {
		_, ok := b.knownPorts[port]
		return ok
	}
	b.mu.RLock()
	_, ok := b.knownPorts[port]
	b.mu.RUnlock()
	return ok
}

// MaxConcurrent returns the peak concurrent connection count from training.
// After Freeze(), reads are lock-free.
func (b *Baseline) MaxConcurrent() int {
	if b.frozen {
		return b.maxConcurrent
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.maxConcurrent
}

// Summary returns a human-readable description of the model.
func (b *Baseline) Summary() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return fmt.Sprintf("%d IPs, %d ports, max %d concurrent, %d samples",
		len(b.knownIPs), len(b.knownPorts), b.maxConcurrent, b.samples)
}

// KnownIPCount returns the number of IPs in the baseline.
func (b *Baseline) KnownIPCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.knownIPs)
}

// KnownPortCount returns the number of ports in the baseline.
func (b *Baseline) KnownPortCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.knownPorts)
}

// ---------------------------------------------------------------------------
// outboundConn — internal type used by Baseline and Scorer
// ---------------------------------------------------------------------------

// outboundConn is a compact representation of one outbound connection.
// It is cheaper than carrying the full linux.NetConn through the scoring path.
type OutboundConn struct {
	RemoteIP   net.IP
	RemotePort uint16
	inode      uint64
	// Process fields populated by InodeCache (best-effort).
	procName string // short name from /proc/<pid>/comm
	pid      int
	exePath  string // full path from /proc/<pid>/exe
	// proto is "tcp", "tcp6", or "udp".
	proto string
}

// CompactKey returns a 6-byte unique identifier for this connection
// (4 bytes IPv4 + 2 bytes port). Zero allocations.
func (c OutboundConn) CompactKey() [6]byte {
	var k [6]byte
	ip4 := c.RemoteIP.To4()
	if ip4 != nil {
		copy(k[0:4], ip4)
	} else {
		copy(k[0:4], c.RemoteIP)
	}
	k[4] = byte(c.RemotePort >> 8)
	k[5] = byte(c.RemotePort)
	return k
}

// Key returns the deduplication key for this connection.
func (c OutboundConn) Key() string {
	// Reverted to string for now to avoid breaking other callers,
	// but Deduplicator will move to CompactKey().
	return c.RemoteIP.String() + ":" + strconv.Itoa(int(c.RemotePort))
}

// displayRemote returns a formatted "ip:port" string for event messages.
func (c OutboundConn) displayRemote() string {
	return fmt.Sprintf("%s:%d", c.RemoteIP.String(), c.RemotePort)
}
