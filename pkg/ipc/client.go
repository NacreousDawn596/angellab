// Package ipc — client.go
//
// Client wraps a Conn with a higher-level request/response API designed
// for CLI use.  Every CLI command uses one of:
//
//	client.Request(cmd, args)                 → *CLIResponse
//	client.EventStream(ctx)                   → <-chan *EventPayload
//
// Usage:
//
//	c, err := ipc.NewClient("/run/angellab/lab.sock")
//	resp, err := c.Request(ipc.CLICmdAngelList, nil)
package ipc

import (
	"context"
	"fmt"
	"math/rand"
	"time"
)

// Client is a high-level IPC client for CLI use.
// It is not safe for concurrent use from multiple goroutines.
type Client struct {
	conn *Conn
}

// NewClient dials socketPath, performs the HELLO handshake as RoleCLI,
// and returns a ready-to-use Client.
func NewClient(socketPath string) (*Client, error) {
	conn, err := Dial(socketPath, RoleCLI)
	if err != nil {
		return nil, fmt.Errorf("client: connect to %s: %w\n\n"+
			"Hint: is labd running?  Try: sudo systemctl start angellab", socketPath, err)
	}
	return &Client{conn: conn}, nil
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Request sends a CLIRequest and waits for the corresponding CLIResponse.
// It is a synchronous, single-round-trip call suitable for all non-streaming commands.
func (c *Client) Request(cmd CLICommand, args map[string]string) (*CLIResponse, error) {
	cid := shortID()

	payload, err := EncodePayload(&CLIRequest{Command: cmd, Args: args})
	if err != nil {
		return nil, fmt.Errorf("client: encode request: %w", err)
	}

	if err := c.conn.Send(&Message{
		Version:       ProtocolVersion,
		Kind:          KindCLIRequest,
		CorrelationID: cid,
		Payload:       payload,
	}); err != nil {
		return nil, fmt.Errorf("client: send request: %w", err)
	}

	// Wait for the matching response.  We set a generous deadline because
	// lab.status may need to query several angels before responding.
	_ = c.conn.SetDeadline(time.Now().Add(15 * time.Second))
	msg, err := c.conn.Recv()
	if err != nil {
		return nil, fmt.Errorf("client: recv response: %w", err)
	}
	_ = c.conn.SetDeadline(time.Time{})

	if msg.Kind != KindCLIResponse {
		return nil, fmt.Errorf("client: expected CLIResponse, got kind %d", msg.Kind)
	}
	if msg.CorrelationID != cid {
		return nil, fmt.Errorf("client: correlation ID mismatch (sent %s, got %s)", cid, msg.CorrelationID)
	}

	var resp CLIResponse
	if err := DecodePayload(msg.Payload, &resp); err != nil {
		return nil, fmt.Errorf("client: decode response: %w", err)
	}
	return &resp, nil
}

// EventStream sends an event.subscribe request and returns a channel on which
// the Lab pushes EventPayload values until ctx is cancelled or the connection
// drops.  The returned channel is closed when streaming ends.
//
// The caller must NOT call any other Client method after calling EventStream.
func (c *Client) EventStream(ctx context.Context) (<-chan *EventPayload, error) {
	cid := shortID()
	payload, err := EncodePayload(&CLIRequest{Command: CLICmdEventSubscribe})
	if err != nil {
		return nil, err
	}
	if err := c.conn.Send(&Message{
		Version:       ProtocolVersion,
		Kind:          KindCLIRequest,
		CorrelationID: cid,
		Payload:       payload,
	}); err != nil {
		return nil, fmt.Errorf("client: subscribe: %w", err)
	}

	// Consume the initial CLIResponse (acknowledge frame).
	_ = c.conn.SetDeadline(time.Now().Add(5 * time.Second))
	ack, err := c.conn.Recv()
	if err != nil {
		return nil, fmt.Errorf("client: subscribe ack: %w", err)
	}
	_ = c.conn.SetDeadline(time.Time{})
	if ack.Kind != KindCLIResponse {
		return nil, fmt.Errorf("client: unexpected ack kind %d", ack.Kind)
	}

	ch := make(chan *EventPayload, 64)

	go func() {
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			// No deadline on streaming connection — Lab pushes at event time.
			msg, err := c.conn.Recv()
			if err != nil {
				return
			}
			if msg.Kind != KindEventStream {
				continue
			}
			var ev EventPayload
			if err := DecodePayload(msg.Payload, &ev); err != nil {
				continue
			}
			select {
			case ch <- &ev:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

// DecodeAs is a generic helper for decoding CLIResponse.Data into a typed value.
//
//	var list []ipc.AngelSummary
//	if err := ipc.DecodeAs(resp.Data, &list); err != nil { … }
func DecodeAs(data []byte, v any) error {
	return DecodePayload(data, v)
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// shortID generates an 8-character hex correlation ID.
func shortID() string {
	var buf [4]byte
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	r.Read(buf[:])
	return fmt.Sprintf("%08x", buf)
}
