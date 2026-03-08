// Package lab — broadcaster.go
//
// The Broadcaster fans out EventPayload messages to all currently
// connected CLI clients that issued an event.subscribe command.
//
// This is what powers:
//
//	$ lab events
//	[Guardian/A-01] restored /etc/passwd
//	[Sentinel/A-02] suspicious outbound traffic
//
// Design: a single goroutine-safe publish method; subscribers register
// a channel and the broadcaster delivers to each in parallel with a
// short timeout so a slow subscriber cannot block an angel.
package lab

import (
	"sync"

	"github.com/nacreousdawn596/angellab/pkg/ipc"
)

// Broadcaster distributes EventPayload values to registered subscribers.
// It is safe for concurrent use.
type Broadcaster struct {
	mu   sync.RWMutex
	subs map[uint64]chan *ipc.EventPayload
	seq  uint64
}

// NewBroadcaster creates an empty Broadcaster.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		subs: make(map[uint64]chan *ipc.EventPayload),
	}
}

// Subscribe registers a new subscriber and returns its channel and
// a cancel function that must be called to unregister.
// The returned channel has a buffer of bufSize events.
func (b *Broadcaster) Subscribe(bufSize int) (<-chan *ipc.EventPayload, func()) {
	b.mu.Lock()
	b.seq++
	id := b.seq
	ch := make(chan *ipc.EventPayload, bufSize)
	b.subs[id] = ch
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
		close(ch)
	}
	return ch, cancel
}

// Publish sends ev to all active subscribers.
// Non-blocking: if a subscriber's channel is full the event is dropped
// for that subscriber only.
func (b *Broadcaster) Publish(ev *ipc.EventPayload) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
			// Subscriber too slow — drop rather than block.
		}
	}
}

// SubscriberCount returns the number of active event subscribers.
// Used for telemetry / debug.
func (b *Broadcaster) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
