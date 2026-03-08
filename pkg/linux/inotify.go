// Package linux provides thin wrappers around Linux-specific kernel interfaces.
//
// inotify.go — inotify(7) file system event notification.
//
// We wrap the raw syscall directly rather than using fsnotify or similar
// abstractions, so callers get the full IN_* event flag surface and
// direct control over watch descriptors.
package linux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------------------
// Inotify event flags (subset we care about)
// ---------------------------------------------------------------------------

const (
	// InAccess — file was accessed (read).
	InAccess uint32 = unix.IN_ACCESS
	// InModify — file was modified.
	InModify uint32 = unix.IN_MODIFY
	// InAttrib — metadata changed (permissions, timestamps, …).
	InAttrib uint32 = unix.IN_ATTRIB
	// InCloseWrite — file opened for writing was closed.
	InCloseWrite uint32 = unix.IN_CLOSE_WRITE
	// InCreate — file or directory was created in a watched directory.
	InCreate uint32 = unix.IN_CREATE
	// InDelete — file or directory was deleted from a watched directory.
	InDelete uint32 = unix.IN_DELETE
	// InDeleteSelf — the watched file or directory was deleted.
	InDeleteSelf uint32 = unix.IN_DELETE_SELF
	// InMoveSelf — the watched file or directory was moved.
	InMoveSelf uint32 = unix.IN_MOVE_SELF
	// InMovedTo — file moved into watched directory.
	InMovedTo uint32 = unix.IN_MOVED_TO

	// DefaultWatchMask is the set of events Guardian watches by default.
	DefaultWatchMask uint32 = InModify | InAttrib | InCloseWrite |
		InCreate | InDelete | InDeleteSelf | InMoveSelf | InMovedTo
)

// ---------------------------------------------------------------------------
// Event
// ---------------------------------------------------------------------------

// InotifyEvent is a high-level representation of a single inotify notification.
type InotifyEvent struct {
	// WatchPath is the path that was explicitly registered with AddWatch.
	WatchPath string
	// Name is the filename within WatchPath for directory events.
	// Empty for events on the watched path itself.
	Name string
	// Mask is the raw IN_* flag bitmask.
	Mask uint32
}

// Path returns the full affected path.
func (e *InotifyEvent) Path() string {
	if e.Name == "" {
		return e.WatchPath
	}
	return filepath.Join(e.WatchPath, e.Name)
}

// IsModify reports whether the event includes IN_MODIFY or IN_CLOSE_WRITE.
func (e *InotifyEvent) IsModify() bool {
	return e.Mask&(InModify|InCloseWrite) != 0
}

// IsDelete reports whether the event includes any deletion flag.
func (e *InotifyEvent) IsDelete() bool {
	return e.Mask&(InDelete|InDeleteSelf) != 0
}

// IsCreate reports whether the event includes IN_CREATE.
func (e *InotifyEvent) IsCreate() bool {
	return e.Mask&InCreate != 0
}

// MaskString returns a human-readable comma-separated list of set flags.
func (e *InotifyEvent) MaskString() string {
	var parts []string
	flags := map[uint32]string{
		InAccess:    "IN_ACCESS",
		InModify:    "IN_MODIFY",
		InAttrib:    "IN_ATTRIB",
		InCreate:    "IN_CREATE",
		InDelete:    "IN_DELETE",
		InDeleteSelf: "IN_DELETE_SELF",
		InMoveSelf:  "IN_MOVE_SELF",
		InMovedTo:   "IN_MOVED_TO",
		InCloseWrite: "IN_CLOSE_WRITE",
	}
	for bit, name := range flags {
		if e.Mask&bit != 0 {
			parts = append(parts, name)
		}
	}
	return strings.Join(parts, "|")
}

// ---------------------------------------------------------------------------
// Watcher
// ---------------------------------------------------------------------------

// Watcher manages an inotify instance and multiplexes events from all
// watched paths onto a single channel.
type Watcher struct {
	fd      int
	mu      sync.Mutex
	watches map[int]string  // watch descriptor → path
	paths   map[string]int  // path → watch descriptor
	Events  chan *InotifyEvent
	Errors  chan error
	done    chan struct{}
}

// NewWatcher creates an inotify instance and starts its read loop.
// Close() must be called to release the file descriptor and goroutine.
func NewWatcher(bufferSize int) (*Watcher, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("inotify: init: %w", err)
	}
	w := &Watcher{
		fd:      fd,
		watches: make(map[int]string),
		paths:   make(map[string]int),
		Events:  make(chan *InotifyEvent, bufferSize),
		Errors:  make(chan error, 8),
		done:    make(chan struct{}),
	}
	go w.readLoop()
	return w, nil
}

// AddWatch registers path with the given event mask.
// If path is already watched, its mask is updated.
func (w *Watcher) AddWatch(path string, mask uint32) error {
	wd, err := unix.InotifyAddWatch(w.fd, path, mask)
	if err != nil {
		return fmt.Errorf("inotify: add watch %s: %w", path, err)
	}
	w.mu.Lock()
	w.watches[int(wd)] = path
	w.paths[path] = int(wd)
	w.mu.Unlock()
	return nil
}

// RemoveWatch unregisters the watch for path.
func (w *Watcher) RemoveWatch(path string) error {
	w.mu.Lock()
	wd, ok := w.paths[path]
	w.mu.Unlock()
	if !ok {
		return nil
	}
	if _, err := unix.InotifyRmWatch(w.fd, uint32(wd)); err != nil {
		return fmt.Errorf("inotify: remove watch %s: %w", path, err)
	}
	w.mu.Lock()
	delete(w.watches, wd)
	delete(w.paths, path)
	w.mu.Unlock()
	return nil
}

// Close stops the read loop and closes the inotify file descriptor.
func (w *Watcher) Close() error {
	close(w.done)
	return unix.Close(w.fd)
}

// readLoop reads raw inotify events using epoll and forwards them.
func (w *Watcher) readLoop() {
	// Set up epoll to wait on the inotify fd.
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		w.Errors <- fmt.Errorf("inotify: epoll_create: %w", err)
		return
	}
	defer unix.Close(epfd)

	event := unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(w.fd),
	}
	if err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, w.fd, &event); err != nil {
		w.Errors <- fmt.Errorf("inotify: epoll_ctl: %w", err)
		return
	}

	buf := make([]byte, 4096)
	events := make([]unix.EpollEvent, 8)

	for {
		select {
		case <-w.done:
			return
		default:
		}

		n, err := unix.EpollWait(epfd, events, 250 /* ms timeout for done check */)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			w.Errors <- fmt.Errorf("inotify: epoll_wait: %w", err)
			return
		}
		if n == 0 {
			continue // timeout — check done channel
		}

		nread, err := unix.Read(w.fd, buf)
		if err != nil {
			if err == unix.EAGAIN {
				continue
			}
			w.Errors <- fmt.Errorf("inotify: read: %w", err)
			return
		}

		w.parseEvents(buf[:nread])
	}
}

// parseEvents walks a buffer of raw inotify_event structs and emits InotifyEvents.
func (w *Watcher) parseEvents(buf []byte) {
	const eventSize = unix.SizeofInotifyEvent
	offset := 0
	for offset+eventSize <= len(buf) {
		raw := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))

		var name string
		if raw.Len > 0 {
			// Name immediately follows the fixed header.
			nameBytes := buf[offset+eventSize : offset+eventSize+int(raw.Len)]
			// Trim null bytes — the kernel pads to a multiple of 4.
			name = strings.TrimRight(string(nameBytes), "\x00")
		}

		w.mu.Lock()
		watchPath := w.watches[int(raw.Wd)]
		w.mu.Unlock()

		if watchPath != "" {
			ev := &InotifyEvent{
				WatchPath: watchPath,
				Name:      name,
				Mask:      raw.Mask,
			}
			select {
			case w.Events <- ev:
			default:
				// Channel full — drop and record the error rather than blocking.
				w.Errors <- fmt.Errorf("inotify: event buffer full, dropped event on %s",
					ev.Path())
			}
		}

		offset += eventSize + int(raw.Len)
	}
}

// ---------------------------------------------------------------------------
// WatchTree — recursive directory watcher helper
// ---------------------------------------------------------------------------

// WatchTree adds an inotify watch for path and, if path is a directory,
// for every subdirectory beneath it.  This is a best-effort recursive
// watch; subdirectories created after WatchTree returns will not be
// automatically watched (the caller should call WatchTree again on
// IN_CREATE events for new directories).
func WatchTree(w *Watcher, path string, mask uint32) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inotify: stat %s: %w", path, err)
	}
	if err := w.AddWatch(path, mask); err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == path {
			return nil
		}
		return w.AddWatch(p, mask)
	})
}
