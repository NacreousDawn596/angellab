// Package ipc — transport.go
//
// Framed msgpack I/O with HELLO handshake.
//
// Every connection — whether from an angel worker or a CLI client — must
// complete the HELLO exchange before sending any other messages:
//
//	Dialer:   send KindHello{Version:1, Role:"angel"|"cli"}
//	Listener: send KindHello{Version:1, Role:"lab"}
//	Both:     validate that the remote Version == ProtocolVersion.
//
// If versions mismatch, the listener closes the connection immediately.
// This protects against stale angel binaries connecting to a new Lab.
package ipc

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

const (
	frameHeaderSize    = 4
	DefaultDialTimeout = 5 * time.Second
	DefaultRWTimeout   = 10 * time.Second
	// helloTimeout is deliberately tight — a well-behaved peer sends HELLO
	// immediately and we do not want to hold a slot open for slow clients.
	helloTimeout = 3 * time.Second
)

// ---------------------------------------------------------------------------
// Conn
// ---------------------------------------------------------------------------

// Conn wraps a net.Conn with framed msgpack I/O.
// It is safe for use by one sender goroutine and one receiver goroutine
// concurrently, but not by multiple senders or multiple receivers.
type Conn struct {
	raw net.Conn
}

// Wrap promotes a plain net.Conn to an IPC Conn.
// The caller is responsible for performing the HELLO handshake separately
// if Wrap is used instead of Dial.
func Wrap(c net.Conn) *Conn {
	return &Conn{raw: c}
}

// Dial connects to socketPath, performs the HELLO handshake as role,
// and returns a ready-to-use Conn.
func Dial(socketPath string, role Role) (*Conn, error) {
	raw, err := net.DialTimeout("unix", socketPath, DefaultDialTimeout)
	if err != nil {
		return nil, fmt.Errorf("ipc: dial %s: %w", socketPath, err)
	}
	c := &Conn{raw: raw}
	if err := c.sendHello(role); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("ipc: send hello: %w", err)
	}
	if err := c.recvHelloAck(); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("ipc: hello ack: %w", err)
	}
	return c, nil
}

// Close closes the underlying connection.
func (c *Conn) Close() error { return c.raw.Close() }

// Ping sends a KindCmdPing and waits up to timeout for a KindCmdPong reply.
// It is used by angels to verify the Lab connection is alive without relying
// solely on the heartbeat send succeeding (which only detects half-open sockets
// once the OS send buffer drains — Ping forces a full round-trip).
//
// On success returns nil.  On timeout or any error, returns a non-nil error
// that the caller should treat as a dead connection.
func (c *Conn) Ping(timeout time.Duration) error {
	_ = c.raw.SetDeadline(time.Now().Add(timeout))
	defer func() { _ = c.raw.SetDeadline(time.Time{}) }()

	if err := c.Send(&Message{
		Version: ProtocolVersion,
		Kind:    KindCmdPing,
	}); err != nil {
		return fmt.Errorf("ping: send: %w", err)
	}

	msg, err := c.Recv()
	if err != nil {
		return fmt.Errorf("ping: recv: %w", err)
	}
	if msg.Kind != KindCmdPong {
		return fmt.Errorf("ping: unexpected reply kind %d", msg.Kind)
	}
	return nil
}

// RemoteAddr returns the remote address string for logging.
func (c *Conn) RemoteAddr() string { return c.raw.RemoteAddr().String() }

// SetDeadline sets a combined read+write deadline. Pass time.Time{} to clear.
func (c *Conn) SetDeadline(t time.Time) error { return c.raw.SetDeadline(t) }

// ---------------------------------------------------------------------------
// Send / Recv
// ---------------------------------------------------------------------------

// Send encodes msg as a length-prefixed msgpack frame and writes it.
func (c *Conn) Send(msg *Message) error {
	payload, err := msgpack.Marshal(msg)
	if err != nil {
		return fmt.Errorf("ipc: marshal: %w", err)
	}
	if len(payload) > MaxFrameSize {
		return fmt.Errorf("ipc: frame too large (%d B)", len(payload))
	}
	var header [frameHeaderSize]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))

	_ = c.raw.SetWriteDeadline(time.Now().Add(DefaultRWTimeout))
	if _, err := c.raw.Write(header[:]); err != nil {
		return fmt.Errorf("ipc: write header: %w", err)
	}
	if _, err := c.raw.Write(payload); err != nil {
		return fmt.Errorf("ipc: write payload: %w", err)
	}
	return nil
}

// Recv reads the next length-prefixed frame and decodes it.
// Blocks until a complete frame is available or an error occurs.
func (c *Conn) Recv() (*Message, error) {
	var header [frameHeaderSize]byte
	_ = c.raw.SetReadDeadline(time.Now().Add(DefaultRWTimeout))
	if _, err := io.ReadFull(c.raw, header[:]); err != nil {
		return nil, fmt.Errorf("ipc: read header: %w", err)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 {
		return nil, fmt.Errorf("ipc: zero-length frame")
	}
	if size > MaxFrameSize {
		return nil, fmt.Errorf("ipc: frame too large (%d B)", size)
	}
	buf := make([]byte, size)
	_ = c.raw.SetReadDeadline(time.Now().Add(DefaultRWTimeout))
	if _, err := io.ReadFull(c.raw, buf); err != nil {
		return nil, fmt.Errorf("ipc: read payload: %w", err)
	}
	var msg Message
	if err := msgpack.Unmarshal(buf, &msg); err != nil {
		return nil, fmt.Errorf("ipc: unmarshal: %w", err)
	}
	return &msg, nil
}

// ---------------------------------------------------------------------------
// HELLO helpers
// ---------------------------------------------------------------------------

// sendHello sends a KindHello frame identifying this peer's role.
func (c *Conn) sendHello(role Role) error {
	payload, err := EncodePayload(&HelloPayload{
		Version: ProtocolVersion,
		Role:    role,
		Binary:  os.Args[0],
	})
	if err != nil {
		return err
	}
	_ = c.raw.SetDeadline(time.Now().Add(helloTimeout))
	return c.Send(&Message{
		Version: ProtocolVersion,
		Kind:    KindHello,
		Payload: payload,
	})
}

// recvHelloAck reads the HELLO response from the server and validates version.
func (c *Conn) recvHelloAck() error {
	_ = c.raw.SetDeadline(time.Now().Add(helloTimeout))
	msg, err := c.Recv()
	if err != nil {
		return fmt.Errorf("recv hello ack: %w", err)
	}
	if msg.Kind != KindHello {
		return fmt.Errorf("expected KindHello ack, got kind %d", msg.Kind)
	}
	var hello HelloPayload
	if err := DecodePayload(msg.Payload, &hello); err != nil {
		return err
	}
	if hello.Version != ProtocolVersion {
		return fmt.Errorf("version mismatch: lab=%d, client=%d",
			hello.Version, ProtocolVersion)
	}
	// Clear deadline after successful handshake.
	_ = c.raw.SetDeadline(time.Time{})
	return nil
}

// AcceptHello reads the initial KindHello from a new inbound connection,
// validates the protocol version, and sends the server's own KindHello ack.
// Returns the peer's HelloPayload for the server to inspect (role, binary).
// On version mismatch the connection is closed and an error returned.
func AcceptHello(c *Conn) (*HelloPayload, error) {
    _ = c.raw.SetDeadline(time.Now().Add(helloTimeout))

    msg, err := c.Recv()
    if err != nil {
        return nil, fmt.Errorf("ipc: accept hello: %w", err)
    }

    if msg.Kind != KindHello {
        return nil, fmt.Errorf("ipc: expected KindHello, got kind %d", msg.Kind)
    }

    var hello HelloPayload
    if err := DecodePayload(msg.Payload, &hello); err != nil {
        return nil, fmt.Errorf("ipc: decode hello: %w", err)
    }

    if hello.Version != ProtocolVersion {
        _ = c.raw.Close()
        return nil, fmt.Errorf("ipc: version mismatch (peer=%d, us=%d)",
            hello.Version, ProtocolVersion)
    }

    // Ack with our own HELLO.
    if err := c.sendHello(RoleLab); err != nil {
        return nil, fmt.Errorf("ipc: send hello ack: %w", err)
    }

    _ = c.raw.SetDeadline(time.Time{})
    return &hello, nil
}

// ---------------------------------------------------------------------------
// Payload helpers
// ---------------------------------------------------------------------------

// EncodePayload serialises v into a []byte for Message.Payload.
func EncodePayload(v any) ([]byte, error) {
	b, err := msgpack.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("ipc: encode payload: %w", err)
	}
	return b, nil
}

// DecodePayload deserialises Message.Payload into v.
func DecodePayload(payload []byte, v any) error {
	if err := msgpack.Unmarshal(payload, v); err != nil {
		return fmt.Errorf("ipc: decode payload: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Listener
// ---------------------------------------------------------------------------

// Listener wraps a net.UnixListener and produces *Conn values from Accept.
type Listener struct {
	raw net.Listener
}

// Listen creates a Unix domain socket listener at path.
// A stale socket file from a previous run is removed first.
func Listen(path string) (*Listener, error) {
	_ = removeSocket(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("ipc: listen %s: %w", path, err)
	}
	// 0660 — readable by the angellab group so CLI can connect without sudo.
	_ = os.Chmod(path, 0660)
	return &Listener{raw: l}, nil
}

// Accept blocks until a new connection arrives.
// It does NOT perform the HELLO handshake — that is left to the server so
// it can route the connection based on the peer's declared role.
func (l *Listener) Accept() (*Conn, error) {
	c, err := l.raw.Accept()
	if err != nil {
		return nil, err
	}
	return Wrap(c), nil
}

// Close closes the listener.
func (l *Listener) Close() error { return l.raw.Close() }

// Addr returns the listener's bound address.
func (l *Listener) Addr() net.Addr { return l.raw.Addr() }
