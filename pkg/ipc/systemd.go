package ipc

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
)

// InheritSystemdListener returns a Listener wrapping the socket passed by
// systemd socket activation (SD_LISTEN_FDS).
//
// systemd sets:
//   - LISTEN_FDS=<n>   number of inherited file descriptors
//   - LISTEN_PID=<pid> PID that should receive the fds
//
// The first inherited fd is always fd=3 (SD_LISTEN_FDS_START).
// We only support the single-socket case (LISTEN_FDS=1).
func InheritSystemdListener() (*Listener, error) {
	listenPID, err := strconv.Atoi(os.Getenv("LISTEN_PID"))
	if err != nil || listenPID != os.Getpid() {
		return nil, fmt.Errorf("ipc: systemd activation: LISTEN_PID mismatch or unset")
	}

	listenFDs, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || listenFDs < 1 {
		return nil, fmt.Errorf("ipc: systemd activation: LISTEN_FDS not set or zero")
	}

	const sdListenFDsStart = 3
	fd := sdListenFDsStart

	// Set close-on-exec so child processes don't inherit the socket.
	syscall.CloseOnExec(fd)

	file := os.NewFile(uintptr(fd), "systemd-socket")
	if file == nil {
		return nil, fmt.Errorf("ipc: systemd activation: could not wrap fd %d", fd)
	}

	raw, err := net.FileListener(file)
	if err != nil {
		return nil, fmt.Errorf("ipc: systemd activation: FileListener: %w", err)
	}
	_ = file.Close() // net.FileListener dups the fd; close the original.

	return &Listener{raw: raw}, nil
}
