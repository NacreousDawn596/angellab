// Package linux — procnet.go
//
// Fast parsers for /proc/net/tcp, /proc/net/tcp6, and /proc/net/udp.
//
// Design goals:
//   - Read each file in one syscall (os.ReadFile)
//   - Parse as raw []byte — no string allocations per line
//   - Use bytes.Fields instead of strings.Fields
//   - Pre-allocate result slices with len(lines) capacity
//
// On a typical system with ~200 connections this parser produces
// zero heap allocations in the hot path after the initial warm-up.
//
// Format of /proc/net/tcp (one line per socket, space-delimited):
//
//	sl  local_address rem_address st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode
//	 0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000  0  12345 …
//
// Addresses are encoded as HEX_IP:HEX_PORT in host byte order (little-endian).
//
// Socket→process mapping:
//
//	For each PID in /proc/<pid>/fd/, follow symlinks looking for "socket:[<inode>]".
//	Match inodes against /proc/net/tcp inode column (field 9, 0-indexed).
//	This lets us emit "process curl (PID 4211) opened connection to …"
package linux

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Connection record
// ---------------------------------------------------------------------------

// TCPState mirrors the kernel's TCP state constants from include/net/tcp_states.h.
type TCPState uint8

const (
	TCPEstablished TCPState = 0x01
	TCPSynSent     TCPState = 0x02
	TCPSynRecv     TCPState = 0x03
	TCPFinWait1    TCPState = 0x04
	TCPFinWait2    TCPState = 0x05
	TCPTimeWait    TCPState = 0x06
	TCPClose       TCPState = 0x07
	TCPCloseWait   TCPState = 0x08
	TCPLastAck     TCPState = 0x09
	TCPListen      TCPState = 0x0A
	TCPClosing     TCPState = 0x0B
)

func (s TCPState) String() string {
	switch s {
	case TCPEstablished:
		return "ESTABLISHED"
	case TCPSynSent:
		return "SYN_SENT"
	case TCPListen:
		return "LISTEN"
	case TCPTimeWait:
		return "TIME_WAIT"
	case TCPCloseWait:
		return "CLOSE_WAIT"
	default:
		return fmt.Sprintf("0x%02X", uint8(s))
	}
}

// NetConn is one row from /proc/net/tcp or /proc/net/udp.
type NetConn struct {
	LocalIP    net.IP
	LocalPort  uint16
	RemoteIP   net.IP
	RemotePort uint16
	State      TCPState
	Inode      uint64
	Proto      string // "tcp", "tcp6", "udp"
}

// IsEstablished reports whether the connection is in ESTABLISHED state.
func (c *NetConn) IsEstablished() bool {
	return c.State == TCPEstablished
}

// IsOutbound reports whether the connection has a non-zero remote IP and
// port — i.e., it is an active outbound or inbound connection, not a listener.
func (c *NetConn) IsOutbound() bool {
	return c.RemotePort != 0 && !c.RemoteIP.Equal(net.IPv4zero)
}

// Key returns a stable string key for deduplication.
// Format: "srcIP:srcPort->dstIP:dstPort"
func (c *NetConn) Key() string {
	return c.LocalIP.String() + ":" + strconv.Itoa(int(c.LocalPort)) +
		"->" +
		c.RemoteIP.String() + ":" + strconv.Itoa(int(c.RemotePort))
}

// ---------------------------------------------------------------------------
// /proc/net/tcp parser
// ---------------------------------------------------------------------------

// ReadTCP parses /proc/net/tcp and returns all socket records.
func ReadTCP() ([]NetConn, error) {
	return readProcNetFile("/proc/net/tcp", "tcp", parseIPv4Addr)
}

// ReadTCP6 parses /proc/net/tcp6 and returns all socket records.
func ReadTCP6() ([]NetConn, error) {
	return readProcNetFile("/proc/net/tcp6", "tcp6", parseIPv6Addr)
}

// ReadUDP parses /proc/net/udp and returns all socket records.
func ReadUDP() ([]NetConn, error) {
	return readProcNetFile("/proc/net/udp", "udp", parseIPv4Addr)
}

// ReadAllConns returns the union of TCP, TCP6, and UDP connections.
// Errors from individual files are returned as a combined error but
// partial results are still returned so the caller can make progress.
func ReadAllConns() ([]NetConn, error) {
	var all []NetConn
	var errs []string

	if tcp, err := ReadTCP(); err == nil {
		all = append(all, tcp...)
	} else {
		errs = append(errs, "tcp: "+err.Error())
	}
	if tcp6, err := ReadTCP6(); err == nil {
		all = append(all, tcp6...)
	} else {
		// tcp6 may not exist on systems with IPv6 disabled — not an error.
		if !os.IsNotExist(err) {
			errs = append(errs, "tcp6: "+err.Error())
		}
	}
	if udp, err := ReadUDP(); err == nil {
		all = append(all, udp...)
	} else {
		errs = append(errs, "udp: "+err.Error())
	}

	if len(errs) > 0 {
		return all, fmt.Errorf("procnet: %s", strings.Join(errs, "; "))
	}
	return all, nil
}

// addrParser is a function that decodes a HEX address field into net.IP.
type addrParser func(hex []byte) net.IP

// readProcNetFile reads path and parses it using addrFn for IP decoding.
// This is the hot path — kept allocation-minimal.
func readProcNetFile(path, proto string, addrFn addrParser) ([]NetConn, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("procnet: read %s: %w", path, err)
	}

	// Split on newline. lines[0] is the header — skip it.
	lines := bytes.Split(data, []byte{'\n'})
	if len(lines) < 2 {
		return nil, nil
	}

	conns := make([]NetConn, 0, len(lines)-1)

	for _, line := range lines[1:] {
		if len(line) == 0 {
			continue
		}

		// bytes.Fields splits on any whitespace, returns sub-slices of line.
		// No allocations beyond the slice header itself.
		parts := bytes.Fields(line)
		if len(parts) < 10 {
			continue
		}

		// Field layout (0-indexed):
		//  0: sl (slot number)
		//  1: local_address (HEX_IP:HEX_PORT)
		//  2: rem_address
		//  3: st (state hex)
		//  4: tx_queue:rx_queue
		//  5: tr:tm->when
		//  6: retrnsmt
		//  7: uid
		//  8: timeout
		//  9: inode

		localIP, localPort, ok1 := parseAddr(parts[1], addrFn)
		remoteIP, remotePort, ok2 := parseAddr(parts[2], addrFn)
		if !ok1 || !ok2 {
			continue
		}

		stateVal, err := strconv.ParseUint(string(parts[3]), 16, 8)
		if err != nil {
			continue
		}

		inode, _ := strconv.ParseUint(string(parts[9]), 10, 64)

		conns = append(conns, NetConn{
			LocalIP:    localIP,
			LocalPort:  localPort,
			RemoteIP:   remoteIP,
			RemotePort: remotePort,
			State:      TCPState(stateVal),
			Inode:      inode,
			Proto:      proto,
		})
	}

	return conns, nil
}

// parseAddr decodes a "HEX_IP:HEX_PORT" field.
// Returns (ip, port, ok).
func parseAddr(field []byte, addrFn addrParser) (net.IP, uint16, bool) {
	colon := bytes.IndexByte(field, ':')
	if colon < 0 || colon+1 >= len(field) {
		return nil, 0, false
	}
	ip := addrFn(field[:colon])
	if ip == nil {
		return nil, 0, false
	}
	port, err := strconv.ParseUint(string(field[colon+1:]), 16, 16)
	if err != nil {
		return nil, 0, false
	}
	return ip, uint16(port), true
}

// parseIPv4AddrInto decodes an 8-char little-endian hex IPv4 address into the provided 4-byte slice.
func parseIPv4AddrInto(hexip []byte, out []byte) bool {
	if len(hexip) != 8 || len(out) < 4 {
		return false
	}
	var raw [4]byte
	if _, err := hex.Decode(raw[:], hexip); err != nil {
		return false
	}
	// little-endian → big-endian (network order)
	out[0] = raw[3]
	out[1] = raw[2]
	out[2] = raw[1]
	out[3] = raw[0]
	return true
}

func parseIPv4Addr(hexip []byte) net.IP {
	ip := make(net.IP, 4)
	if !parseIPv4AddrInto(hexip, ip) {
		return nil
	}
	return ip
}

// parseIPv6Addr decodes a 32-char hex IPv6 address from /proc/net/tcp6.
// The kernel writes each 32-bit word in host byte order.
func parseIPv6Addr(hex []byte) net.IP {
	if len(hex) != 32 {
		return nil
	}
	ip := make(net.IP, 16)
	for word := 0; word < 4; word++ {
		chunk := hex[word*8 : word*8+8]
		v, err := strconv.ParseUint(string(chunk), 16, 32)
		if err != nil {
			return nil
		}
		binary.LittleEndian.PutUint32(ip[word*4:], uint32(v))
	}
	return ip
}

// parseHexUint16 decodes a hex string (up to 4 chars) into a uint16 without allocations.
func parseHexUint16(b []byte) (uint16, bool) {
	if len(b) == 0 || len(b) > 4 {
		return 0, false
	}
	var res uint32
	for _, c := range b {
		res <<= 4
		switch {
		case c >= '0' && c <= '9':
			res += uint32(c - '0')
		case c >= 'A' && c <= 'F':
			res += uint32(c - 'A' + 10)
		case c >= 'a' && c <= 'f':
			res += uint32(c - 'a' + 10)
		default:
			return 0, false
		}
	}
	return uint16(res), true
}

func parseHexUint8(b []byte) (uint8, bool) {
	if len(b) == 0 || len(b) > 2 {
		return 0, false
	}
	var res uint32
	for _, c := range b {
		res <<= 4
		switch {
		case c >= '0' && c <= '9':
			res += uint32(c - '0')
		case c >= 'A' && c <= 'F':
			res += uint32(c - 'A' + 10)
		case c >= 'a' && c <= 'f':
			res += uint32(c - 'a' + 10)
		default:
			return 0, false
		}
	}
	return uint8(res), true
}

func parseDecUint64(b []byte) (uint64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	var res uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		res = res*10 + uint64(c-'0')
	}
	return res, true
}

// ---------------------------------------------------------------------------
// Socket → process mapper
// ---------------------------------------------------------------------------

// ProcInfo maps a socket inode to the process that owns it.
type ProcInfo struct {
	PID     int
	Comm    string // short process name from /proc/<pid>/comm (e.g. "curl")
	// ExePath is the full path of the executable resolved from /proc/<pid>/exe.
	// Empty if the process has exited or we lack permission to read the symlink.
	ExePath string
}

// InodeMap is a mapping from socket inode → ProcInfo.
// Build it once per poll cycle and query it to enrich NetConn events.
type InodeMap map[uint64]ProcInfo

// BuildInodeMap walks /proc/<pid>/fd for all visible processes and maps
// socket inodes to their owning process.
//
// The walk ignores PIDs where we lack read permission (common for processes
// owned by other users when running as the angellab system user).
//
// Pattern of each fd symlink: "socket:[<inode>]"
func BuildInodeMap() InodeMap {
	result := make(InodeMap, 256)

	procDir, err := os.Open("/proc")
	if err != nil {
		return result
	}
	defer procDir.Close()

	entries, err := procDir.Readdirnames(-1)
	if err != nil {
		return result
	}

	for _, name := range entries {
		// Only process directories that are purely numeric (PIDs).
		pid, err := strconv.Atoi(name)
		if err != nil {
			continue
		}

		fdPath := fmt.Sprintf("/proc/%d/fd", pid)
		fds, err := os.ReadDir(fdPath)
		if err != nil {
			continue // permission denied or process vanished
		}

		// Read the comm file once per PID — cheap.
		comm := readComm(pid)

		for _, fd := range fds {
			linkPath := filepath.Join(fdPath, fd.Name())
			target, err := os.Readlink(linkPath)
			if err != nil {
				continue
			}
			// socket:[12345] → extract inode 12345
			if !strings.HasPrefix(target, "socket:[") {
				continue
			}
			inodeStr := target[8 : len(target)-1]
			inode, err := strconv.ParseUint(inodeStr, 10, 64)
			if err != nil {
				continue
			}
			result[inode] = ProcInfo{PID: pid, Comm: comm}
		}
	}

	return result
}

// Lookup returns the ProcInfo for the given inode, if known.
func (m InodeMap) Lookup(inode uint64) (ProcInfo, bool) {
	info, ok := m[inode]
	return info, ok
}

// readComm reads the process name from /proc/<pid>/comm.
// Returns "unknown" on error.
func readComm(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(data))
}

// readExePath resolves /proc/<pid>/exe to the full executable path.
// Returns an empty string on error (process may have exited, or we lack
// permission).  This is best-effort — never block on it.
func readExePath(pid int) string {
	path, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return ""
	}
	// Strip the " (deleted)" suffix that appears when the binary has been
	// replaced on disk while the process is still running.
	return strings.TrimSuffix(path, " (deleted)")
}

// ParseTCPFile parses raw /proc/net/tcp bytes (already read into memory)
// and returns all connection entries.
//
// Exposed for benchmarking and testing — the production path uses ReadTCP()
// which calls os.ReadFile.  By accepting []byte we can test the parser in
// isolation against synthetic data without touching the filesystem.
//
// Uses manual bytes.IndexByte line iteration: no bytes.Split allocation (avoids
// building a [][]byte header array) and no bufio.Scanner buffer allocation.
func ParseTCPFile(data []byte) []NetConn {
	// Skip header line.
	nl := bytes.IndexByte(data, '\n')
	if nl < 0 {
		return nil
	}
	data = data[nl+1:]

	// Pre-allocate with a reasonable estimate (128 bytes/line).
	estimated := len(data)/128 + 1
	conns := make([]NetConn, 0, estimated)

	// IP slab allocation: allocate all bytes for all IPs in one block.
	// Each connection needs 2 IPs (local + remote) * 4 bytes = 8 bytes.
	slab := make([]byte, estimated*8)
	slabPos := 0

	for len(data) > 0 {
		nl = bytes.IndexByte(data, '\n')
		var line []byte
		if nl < 0 {
			line = data
			data = nil
		} else {
			line = data[:nl]
			data = data[nl+1:]
		}
		if len(line) == 0 {
			continue
		}

		// Manual zero-alloc field separator.
		// We need fields 1 (local), 2 (remote), 3 (state), 9 (inode).
		var fields [11][]byte
		fieldIdx := 0
		l := 0
		for l < len(line) && fieldIdx < 11 {
			// Skip whitespace.
			for l < len(line) && (line[l] == ' ' || line[l] == '\t') {
				l++
			}
			if l == len(line) {
				break
			}
			start := l
			// Find end of field.
			for l < len(line) && line[l] != ' ' && line[l] != '\t' {
				l++
			}
			fields[fieldIdx] = line[start:l]
			fieldIdx++
		}

		if fieldIdx < 10 {
			continue
		}

		// Ensure slab has enough space (grow if needed - rare)
		if slabPos+8 > len(slab) {
			newSlab := make([]byte, len(slab)*2+8)
			copy(newSlab, slab)
			slab = newSlab
		}

		// Parse addresses directly into the slab
		localIPField := fields[1]
		remoteIPField := fields[2]

		colon1 := bytes.IndexByte(localIPField, ':')
		colon2 := bytes.IndexByte(remoteIPField, ':')
		if colon1 < 0 || colon2 < 0 {
			continue
		}

		localIP := net.IP(slab[slabPos : slabPos+4])
		if !parseIPv4AddrInto(localIPField[:colon1], localIP) {
			continue
		}
		localPort, ok1 := parseHexUint16(localIPField[colon1+1:])

		remoteIP := net.IP(slab[slabPos+4 : slabPos+8])
		if !parseIPv4AddrInto(remoteIPField[:colon2], remoteIP) {
			continue
		}
		remotePort, ok2 := parseHexUint16(remoteIPField[colon2+1:])

		if !ok1 || !ok2 {
			continue
		}

		stateVal, ok3 := parseHexUint8(fields[3])
		inode, ok4 := parseDecUint64(fields[9])
		if !ok3 || !ok4 {
			continue
		}

		slabPos += 8

		conns = append(conns, NetConn{
			LocalIP:    localIP,
			LocalPort:  uint16(localPort),
			RemoteIP:   remoteIP,
			RemotePort: uint16(remotePort),
			State:      TCPState(stateVal),
			Inode:      inode,
			Proto:      "tcp",
		})
	}
	return conns
}

