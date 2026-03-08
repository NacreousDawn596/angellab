// Package integration contains end-to-end tests for the AngelLab IPC pipeline.
//
// These tests spin up a real Unix socket, a real ipc.Listener, and a real
// ipc.Client.  They do NOT exec angel subprocesses — instead they simulate
// angel behaviour by dialing the socket directly.  This validates the full
// HELLO→REGISTER→HEARTBEAT→EVENT→CLI_REQUEST→CLI_RESPONSE pipeline without
// any process-spawning plumbing.
//
// Running:
//
//	go test ./test/integration/... -v -count=1
//
// Tests run on Linux only (they use /tmp Unix sockets).
package integration

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nacreousdawn596/angellab/pkg/ipc"
	"github.com/nacreousdawn596/angellab/pkg/logging"
)

// ---------------------------------------------------------------------------
// Minimal in-process Lab stand-in
// ---------------------------------------------------------------------------

// miniLab is a lightweight stand-in for the full Lab daemon.
// It exercises the ipc.Listener / ipc.AcceptHello / transport pipeline
// without importing internal/lab (which requires CGO for SQLite).
type miniLab struct {
	listener *ipc.Listener
	log      *logging.Logger

	mu     sync.Mutex
	angels map[string]*angelRecord
	events []ipc.EventPayload
}

type angelRecord struct {
	id        string
	angelType string
	conn      *ipc.Conn
	lastHB    *ipc.HeartbeatPayload
}

func newMiniLab(t *testing.T, socketPath string) *miniLab {
	t.Helper()
	l, err := ipc.Listen(socketPath)
	if err != nil {
		t.Fatalf("miniLab: listen %s: %v", socketPath, err)
	}
	lab := &miniLab{
		listener: l,
		log:      logging.NewDefault("MiniLab"),
		angels:   make(map[string]*angelRecord),
	}
	return lab
}

func (lab *miniLab) serve(ctx context.Context) {
	for {
		conn, err := lab.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				return
			}
		}
		go lab.handleConn(ctx, conn)
	}
}

func (lab *miniLab) handleConn(ctx context.Context, conn *ipc.Conn) {
	hello, err := ipc.AcceptHello(conn)
	if err != nil {
		return
	}

	switch hello.Role {
	case ipc.RoleAngel:
		lab.handleAngel(ctx, conn)
	case ipc.RoleCLI:
		lab.handleCLI(ctx, conn)
	}
}

func (lab *miniLab) handleAngel(_ context.Context, conn *ipc.Conn) {
	// Expect REGISTER.
	msg, err := conn.Recv()
	if err != nil || msg.Kind != ipc.KindRegister {
		return
	}
	var reg ipc.RegisterPayload
	if err := ipc.DecodePayload(msg.Payload, &reg); err != nil {
		return
	}

	rec := &angelRecord{id: reg.AngelID, angelType: reg.AngelType, conn: conn}
	lab.mu.Lock()
	lab.angels[reg.AngelID] = rec
	lab.mu.Unlock()

	// Read subsequent frames.
	for {
		msg, err := conn.Recv()
		if err != nil {
			return
		}
		switch msg.Kind {
		case ipc.KindCmdPing:
			// Respond with KindCmdPong — mirrors what the real Lab server does.
			_ = conn.Send(&ipc.Message{
				Version:       ipc.ProtocolVersion,
				Kind:          ipc.KindCmdPong,
				CorrelationID: msg.CorrelationID,
			})
		case ipc.KindHeartbeat:
			var hb ipc.HeartbeatPayload
			if ipc.DecodePayload(msg.Payload, &hb) == nil {
				lab.mu.Lock()
				rec.lastHB = &hb
				lab.mu.Unlock()
			}
		case ipc.KindEvent:
			var ev ipc.EventPayload
			if ipc.DecodePayload(msg.Payload, &ev) == nil {
				lab.mu.Lock()
				lab.events = append(lab.events, ev)
				lab.mu.Unlock()
			}
		}
	}
}

func (lab *miniLab) handleCLI(_ context.Context, conn *ipc.Conn) {
	msg, err := conn.Recv()
	if err != nil || msg.Kind != ipc.KindCLIRequest {
		return
	}
	var req ipc.CLIRequest
	if err := ipc.DecodePayload(msg.Payload, &req); err != nil {
		return
	}

	var responseData []byte
	switch req.Command {
	case ipc.CLICmdAngelList:
		lab.mu.Lock()
		list := make([]ipc.AngelSummary, 0, len(lab.angels))
		for _, a := range lab.angels {
			list = append(list, ipc.AngelSummary{ID: a.id, AngelType: a.angelType})
		}
		lab.mu.Unlock()
		responseData, _ = ipc.EncodePayload(list)

	case ipc.CLICmdLabStatus:
		responseData, _ = ipc.EncodePayload(ipc.LabStatus{
			Version: "test", PID: os.Getpid(),
		})

	default:
		responseData, _ = ipc.EncodePayload(map[string]string{"echo": string(req.Command)})
	}

	respPayload, _ := ipc.EncodePayload(&ipc.CLIResponse{OK: true, Data: responseData})
	_ = conn.Send(&ipc.Message{
		Version:       ipc.ProtocolVersion,
		Kind:          ipc.KindCLIResponse,
		CorrelationID: msg.CorrelationID,
		Payload:       respPayload,
	})
}

// ---------------------------------------------------------------------------
// Simulated angel helper
// ---------------------------------------------------------------------------

// simAngel connects to the lab socket as an angel, sends REGISTER + heartbeats.
type simAngel struct {
	conn      *ipc.Conn
	angelID   string
	angelType string
}

func newSimAngel(t *testing.T, socketPath, id, typ string) *simAngel {
	t.Helper()
	conn, err := ipc.Dial(socketPath, ipc.RoleAngel)
	if err != nil {
		t.Fatalf("simAngel: dial: %v", err)
	}

	// Send REGISTER.
	payload, _ := ipc.EncodePayload(&ipc.RegisterPayload{
		AngelID: id, AngelType: typ, PID: os.Getpid(),
	})
	if err := conn.Send(&ipc.Message{
		Version: ipc.ProtocolVersion,
		Kind:    ipc.KindRegister,
		Payload: payload,
	}); err != nil {
		t.Fatalf("simAngel: register: %v", err)
	}

	return &simAngel{conn: conn, angelID: id, angelType: typ}
}

func (a *simAngel) sendHeartbeat(t *testing.T) {
	t.Helper()
	payload, _ := ipc.EncodePayload(&ipc.HeartbeatPayload{
		AngelID:    a.angelID,
		State:      "ACTIVE",
		CPUPercent: 0.1,
		RSSBytes:   8 << 20,
		Goroutines: 4,
		FDCount:    10,
		Uptime:     5,
		AngelMeta:  map[string]string{"test": "true"},
	})
	if err := a.conn.Send(&ipc.Message{
		Version: ipc.ProtocolVersion,
		Kind:    ipc.KindHeartbeat,
		Payload: payload,
	}); err != nil {
		t.Fatalf("simAngel: heartbeat: %v", err)
	}
}

func (a *simAngel) sendEvent(t *testing.T, msg string) {
	t.Helper()
	payload, _ := ipc.EncodePayload(&ipc.EventPayload{
		AngelID:   a.angelID,
		Severity:  ipc.SeverityWarn,
		Message:   msg,
		Timestamp: time.Now(),
		Meta:      map[string]string{"source": "integration_test"},
	})
	if err := a.conn.Send(&ipc.Message{
		Version: ipc.ProtocolVersion,
		Kind:    ipc.KindEvent,
		Payload: payload,
	}); err != nil {
		t.Fatalf("simAngel: event: %v", err)
	}
}

func (a *simAngel) close() { _ = a.conn.Close() }

// ---------------------------------------------------------------------------
// Helper
// ---------------------------------------------------------------------------

// tmpSocket returns a unique socket path under t.TempDir().
func tmpSocket(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "lab.sock")
}

// waitFor polls cond until it returns true or timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestHelloHandshake verifies that both RoleAngel and RoleCLI connections
// complete the HELLO handshake and are accepted by the server.
func TestHelloHandshake(t *testing.T) {
	sock := tmpSocket(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lab := newMiniLab(t, sock)
	go lab.serve(ctx)
	defer lab.listener.Close()

	// Angel connection.
	angel := newSimAngel(t, sock, "A-01", "guardian")
	defer angel.close()

	// CLI connection via Client.
	cli, err := ipc.NewClient(sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cli.Close()

	t.Log("TestHelloHandshake: PASS — both roles connected successfully")
}

// TestVersionMismatch verifies that a client with a wrong protocol version
// is rejected after the HELLO exchange.
func TestVersionMismatch(t *testing.T) {
	sock := tmpSocket(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lab := newMiniLab(t, sock)
	go lab.serve(ctx)
	defer lab.listener.Close()

	// Manually dial and send a HELLO with wrong version.
	raw, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()

	conn := ipc.Wrap(raw)

	// Build a HELLO payload with wrong version.
	wrongPayload, _ := ipc.EncodePayload(&ipc.HelloPayload{
		Version: 255, // definitely not ProtocolVersion
		Role:    ipc.RoleAngel,
	})
	if err := conn.Send(&ipc.Message{
		Version: 255,
		Kind:    ipc.KindHello,
		Payload: wrongPayload,
	}); err != nil {
		t.Fatalf("send bad hello: %v", err)
	}

	// The server should close the connection or send a reject response.
	// Either way, a subsequent Recv should fail.
	_, recvErr := conn.Recv()
	if recvErr == nil {
		t.Error("expected connection to be closed after version mismatch, but Recv succeeded")
	} else {
		t.Logf("TestVersionMismatch: connection closed as expected: %v", recvErr)
	}
}

// TestRegisterAndHeartbeat verifies that the lab correctly receives
// a REGISTER frame followed by HEARTBEAT frames.
func TestRegisterAndHeartbeat(t *testing.T) {
	sock := tmpSocket(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lab := newMiniLab(t, sock)
	go lab.serve(ctx)
	defer lab.listener.Close()

	angel := newSimAngel(t, sock, "A-01", "guardian")
	defer angel.close()

	angel.sendHeartbeat(t)
	angel.sendHeartbeat(t)

	// Wait for lab to receive both heartbeats.
	ok := waitFor(t, 2*time.Second, func() bool {
		lab.mu.Lock()
		defer lab.mu.Unlock()
		rec, exists := lab.angels["A-01"]
		return exists && rec.lastHB != nil && rec.lastHB.FDCount >= 0
	})
	if !ok {
		t.Fatal("timed out waiting for heartbeat to be received by lab")
	}

	lab.mu.Lock()
	hb := lab.angels["A-01"].lastHB
	lab.mu.Unlock()

	if hb.AngelID != "A-01" {
		t.Errorf("heartbeat angel_id = %q, want A-01", hb.AngelID)
	}
	if hb.RSSBytes != 8<<20 {
		t.Errorf("heartbeat rss_bytes = %d, want %d", hb.RSSBytes, 8<<20)
	}
	t.Logf("TestRegisterAndHeartbeat: received heartbeat CPU=%.1f%% RSS=%dB FD=%d",
		hb.CPUPercent, hb.RSSBytes, hb.FDCount)
}

// TestEventPipeline verifies that EventPayload frames emitted by an angel
// are received and stored by the lab.
func TestEventPipeline(t *testing.T) {
	sock := tmpSocket(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lab := newMiniLab(t, sock)
	go lab.serve(ctx)
	defer lab.listener.Close()

	angel := newSimAngel(t, sock, "A-02", "sentinel")
	defer angel.close()

	const eventMsg = "anomalous outbound → 185.199.108.153:443 (curl, pid 4211)"
	angel.sendEvent(t, eventMsg)

	ok := waitFor(t, 2*time.Second, func() bool {
		lab.mu.Lock()
		defer lab.mu.Unlock()
		for _, ev := range lab.events {
			if ev.Message == eventMsg {
				return true
			}
		}
		return false
	})
	if !ok {
		t.Fatal("timed out waiting for event to arrive at lab")
	}

	lab.mu.Lock()
	var got *ipc.EventPayload
	for i := range lab.events {
		if lab.events[i].Message == eventMsg {
			got = &lab.events[i]
			break
		}
	}
	lab.mu.Unlock()

	if got.AngelID != "A-02" {
		t.Errorf("event angel_id = %q, want A-02", got.AngelID)
	}
	if got.Severity != ipc.SeverityWarn {
		t.Errorf("event severity = %v, want WARN", got.Severity)
	}
	t.Logf("TestEventPipeline: event received: [%s] %s", got.Severity, got.Message)
}

// TestCLIAngelList verifies that the CLI can request angel.list and
// receive a well-formed response.
func TestCLIAngelList(t *testing.T) {
	sock := tmpSocket(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lab := newMiniLab(t, sock)
	go lab.serve(ctx)
	defer lab.listener.Close()

	// Register two angels.
	a1 := newSimAngel(t, sock, "A-01", "guardian")
	defer a1.close()
	a2 := newSimAngel(t, sock, "A-02", "sentinel")
	defer a2.close()

	// Wait for both to register.
	ok := waitFor(t, 2*time.Second, func() bool {
		lab.mu.Lock()
		defer lab.mu.Unlock()
		return len(lab.angels) == 2
	})
	if !ok {
		t.Fatal("timed out waiting for both angels to register")
	}

	// Send CLI request.
	cli, err := ipc.NewClient(sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cli.Close()

	resp, err := cli.Request(ipc.CLICmdAngelList, nil)
	if err != nil {
		t.Fatalf("CLI request angel.list: %v", err)
	}
	if !resp.OK {
		t.Fatalf("angel.list response not OK: %s", resp.Error)
	}

	var list []ipc.AngelSummary
	if err := ipc.DecodeAs(resp.Data, &list); err != nil {
		t.Fatalf("decode angel list: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("angel list len = %d, want 2", len(list))
	}

	ids := make(map[string]string)
	for _, a := range list {
		ids[a.ID] = a.AngelType
	}
	if ids["A-01"] != "guardian" {
		t.Errorf("A-01 type = %q, want guardian", ids["A-01"])
	}
	if ids["A-02"] != "sentinel" {
		t.Errorf("A-02 type = %q, want sentinel", ids["A-02"])
	}
	t.Logf("TestCLIAngelList: received %d angels: %v", len(list), ids)
}

// TestMultipleAngelsHeartbeats exercises concurrent heartbeats from multiple
// angels to verify the server's concurrent read path has no races.
// Run with: go test -race ./test/integration/...
func TestMultipleAngelsHeartbeats(t *testing.T) {
	sock := tmpSocket(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	lab := newMiniLab(t, sock)
	go lab.serve(ctx)
	defer lab.listener.Close()

	const numAngels = 5
	angels := make([]*simAngel, numAngels)
	for i := range angels {
		id := "A-" + string(rune('0'+i+1))
		angels[i] = newSimAngel(t, sock, id, "guardian")
		defer angels[i].close()
	}

	// Send 3 heartbeats from each angel concurrently.
	var wg sync.WaitGroup
	for _, a := range angels {
		a := a
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 3; i++ {
				a.sendHeartbeat(t)
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}
	wg.Wait()

	// All angels should have registered and sent at least one heartbeat.
	ok := waitFor(t, 3*time.Second, func() bool {
		lab.mu.Lock()
		defer lab.mu.Unlock()
		for _, rec := range lab.angels {
			if rec.lastHB == nil {
				return false
			}
		}
		return len(lab.angels) == numAngels
	})
	if !ok {
		lab.mu.Lock()
		t.Errorf("only %d/%d angels delivered heartbeats", len(lab.angels), numAngels)
		lab.mu.Unlock()
	} else {
		t.Logf("TestMultipleAngelsHeartbeats: all %d angels registered and heartbeat received", numAngels)
	}
}

// TestCorrelationID verifies that CLI responses carry the same correlation ID
// as the request — critical for the client's request/response matching.
func TestCorrelationID(t *testing.T) {
	sock := tmpSocket(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lab := newMiniLab(t, sock)
	go lab.serve(ctx)
	defer lab.listener.Close()

	cli, err := ipc.NewClient(sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cli.Close()

	// Send lab.status — any command that gets a response.
	resp, err := cli.Request(ipc.CLICmdLabStatus, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if !resp.OK {
		t.Fatalf("response not OK: %s", resp.Error)
	}

	// Decode the status.
	var status ipc.LabStatus
	if err := ipc.DecodeAs(resp.Data, &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.PID != os.Getpid() {
		t.Errorf("status PID = %d, want %d", status.PID, os.Getpid())
	}
	t.Logf("TestCorrelationID: status PID=%d version=%q", status.PID, status.Version)
}

// TestDeduplicatorIntegration tests the Sentinel deduplicator properties
// without needing a network connection.
func TestDeduplicatorIntegration(t *testing.T) {
	// Verify that dedup JSON serialisation round-trips cleanly.
	// (This is a property test, not a network test.)
	event := ipc.EventPayload{
		AngelID:   "A-02",
		Severity:  ipc.SeverityCritical,
		Message:   "anomalous outbound → 185.199.108.153:443 (curl [/usr/bin/curl], pid 4211) — score 7 (new IP + high port + very high port)",
		Timestamp: time.Now().UTC().Truncate(time.Millisecond),
		Meta: map[string]string{
			"remote_ip":   "185.199.108.153",
			"remote_port": "443",
			"score":       "7",
			"process":     "curl",
			"exe":         "/usr/bin/curl",
		},
	}

	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ipc.EventPayload
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Meta["exe"] != "/usr/bin/curl" {
		t.Errorf("meta[exe] = %q, want /usr/bin/curl", got.Meta["exe"])
	}
	if !strings.Contains(got.Message, "185.199.108.153") {
		t.Errorf("message missing IP: %q", got.Message)
	}
	t.Logf("TestDeduplicatorIntegration: event round-trips correctly")
}
