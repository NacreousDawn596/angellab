// Package lab — server.go
//
// The Server accepts all connections on lab.sock.
// The first action on every connection is the HELLO handshake (handled by
// ipc.AcceptHello), which identifies the peer's role:
//
//   - RoleAngel → long-lived angel worker connection
//   - RoleCLI   → short-lived CLI request/response (or long-lived event stream)
//
// Each connection is handled in its own goroutine.
package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/nacreousdawn596/angellab/internal/angels/guardian"
	"github.com/nacreousdawn596/angellab/pkg/ipc"
	"github.com/nacreousdawn596/angellab/pkg/logging"
	"github.com/nacreousdawn596/angellab/pkg/registry"
	"github.com/nacreousdawn596/angellab/pkg/version"
)

// Server listens on lab.sock and dispatches connections.
type Server struct {
	cfg       *Config
	log       *logging.Logger
	reg       *registry.Registry
	sup       *Supervisor
	bcast     *Broadcaster
	startedAt time.Time
}

// NewServer constructs a Server with all required dependencies.
func NewServer(cfg *Config, log *logging.Logger, reg *registry.Registry,
	sup *Supervisor, bcast *Broadcaster, startedAt time.Time) *Server {
	return &Server{
		cfg:       cfg,
		log:       log,
		reg:       reg,
		sup:       sup,
		bcast:     bcast,
		startedAt: startedAt,
	}
}

// Serve accepts connections until ctx is cancelled or the listener is closed.
func (s *Server) Serve(ctx context.Context, l *ipc.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				s.log.Warn("accept: %v", err)
				continue
			}
		}
		go s.handleConn(ctx, conn)
	}
}

// handleConn performs the HELLO handshake and routes to the right handler.
func (s *Server) handleConn(ctx context.Context, conn *ipc.Conn) {
	defer conn.Close()

	hello, err := ipc.AcceptHello(conn)
	if err != nil {
		s.log.Debug("hello from %s: %v", conn.RemoteAddr(), err)
		return
	}

	switch hello.Role {
	case ipc.RoleAngel:
		s.handleAngelConn(ctx, conn)
	case ipc.RoleCLI:
		s.handleCLIConn(ctx, conn)
	default:
		s.log.Warn("unknown role %q from %s", hello.Role, conn.RemoteAddr())
	}
}

// ---------------------------------------------------------------------------
// Angel connection handler
// ---------------------------------------------------------------------------

// handleAngelConn processes the long-lived angel IPC connection.
// After REGISTER it loops reading HEARTBEAT and EVENT frames.
func (s *Server) handleAngelConn(ctx context.Context, conn *ipc.Conn) {
	// First post-HELLO frame must be REGISTER.
	msg, err := conn.Recv()
	if err != nil {
		s.log.Warn("angel first frame: %v", err)
		return
	}
	if msg.Kind != ipc.KindRegister {
		s.log.Warn("angel: expected KindRegister, got kind %d", msg.Kind)
		return
	}

	var reg ipc.RegisterPayload
	if err := ipc.DecodePayload(msg.Payload, &reg); err != nil {
		s.log.Warn("angel: decode register: %v", err)
		return
	}

	if err := s.sup.RegisterConn(reg.AngelID, conn); err != nil {
		s.log.Warn("angel: register %s: %v", reg.AngelID, err)
		return
	}

	// Remove deadline for the long-lived connection.
	_ = conn.SetDeadline(time.Time{})

	// Main receive loop.
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Deadline = 2× heartbeat timeout so we detect completely silent angels.
		_ = conn.SetDeadline(time.Now().Add(s.cfg.Supervisor.HeartbeatTimeout.Duration * 2))

		msg, err := conn.Recv()
		if err != nil {
			s.log.Warn("angel %s disconnected: %v", reg.AngelID, err)
			// Open recovery window — the angel may reconnect.
			s.sup.MarkConnLost(reg.AngelID)
			return
		}

		switch msg.Kind {
		case ipc.KindHeartbeat:
			var hb ipc.HeartbeatPayload
			if err := ipc.DecodePayload(msg.Payload, &hb); err != nil {
				s.log.Warn("angel %s heartbeat decode: %v", reg.AngelID, err)
				continue
			}
			s.sup.HandleHeartbeat(&hb)

		case ipc.KindEvent:
			var ev ipc.EventPayload
			if err := ipc.DecodePayload(msg.Payload, &ev); err != nil {
				s.log.Warn("angel %s event decode: %v", reg.AngelID, err)
				continue
			}
			s.handleEvent(reg.AngelID, &ev)

		case ipc.KindCmdPing:
			// Angel is probing whether Lab is still alive — respond immediately.
			// The Ping/Pong exchange is a full round-trip: it detects half-open
			// sockets that a one-way heartbeat Send cannot catch.
			_ = conn.Send(&ipc.Message{
				Version:       ipc.ProtocolVersion,
				Kind:          ipc.KindCmdPong,
				CorrelationID: msg.CorrelationID,
			})

		default:
			s.log.Debug("angel %s sent unknown kind %d — ignoring", reg.AngelID, msg.Kind)
		}
	}
}

// handleEvent persists, logs, and broadcasts one EventPayload.
func (s *Server) handleEvent(angelID string, ev *ipc.EventPayload) {
	angelType := "Angel"
	if entry, ok := s.sup.GetEntry(angelID); ok {
		entry.mu.Lock()
		angelType = entry.AngelType
		entry.mu.Unlock()
	}

	// [Angel Lab] format that operators see in logs and event stream.
	s.log.AngelEvent(titleCase(angelType), angelID, ev.Message)

	metaJSON, _ := json.Marshal(ev.Meta)
	_ = s.reg.InsertEvent(&registry.Event{
		AngelID:    angelID,
		Severity:   ev.Severity.String(),
		Message:    ev.Message,
		MetaJSON:   string(metaJSON),
		OccurredAt: ev.Timestamp,
	})

	s.bcast.Publish(ev)
}

// ---------------------------------------------------------------------------
// CLI connection handler
// ---------------------------------------------------------------------------

// handleCLIConn handles a single CLI request, or long-running event stream.
func (s *Server) handleCLIConn(ctx context.Context, conn *ipc.Conn) {
	msg, err := conn.Recv()
	if err != nil {
		s.log.Debug("cli first frame from %s: %v", conn.RemoteAddr(), err)
		return
	}
	if msg.Kind != ipc.KindCLIRequest {
		s.log.Warn("cli: expected KindCLIRequest, got kind %d", msg.Kind)
		return
	}

	var req ipc.CLIRequest
	if err := ipc.DecodePayload(msg.Payload, &req); err != nil {
		s.sendError(conn, msg.CorrelationID, "decode: "+err.Error())
		return
	}

	switch req.Command {
	case ipc.CLICmdAngelCreate:
		s.cliAngelCreate(ctx, conn, msg.CorrelationID, req.Args)
	case ipc.CLICmdAngelList:
		s.cliAngelList(conn, msg.CorrelationID)
	case ipc.CLICmdAngelInspect:
		s.cliAngelInspect(conn, msg.CorrelationID, req.Args)
	case ipc.CLICmdAngelTerminate:
		s.cliAngelTerminate(conn, msg.CorrelationID, req.Args)
	case ipc.CLICmdAngelDiff:
		s.cliAngelDiff(conn, msg.CorrelationID, req.Args)
	case ipc.CLICmdLabStatus:
		s.cliLabStatus(conn, msg.CorrelationID)
	case ipc.CLICmdEventSubscribe:
		s.cliEventStream(ctx, conn, msg.CorrelationID)
	default:
		s.sendError(conn, msg.CorrelationID,
			fmt.Sprintf("unknown command: %q", req.Command))
	}
}

// ---------------------------------------------------------------------------
// CLI command implementations
// ---------------------------------------------------------------------------

func (s *Server) cliAngelCreate(ctx context.Context, conn *ipc.Conn, cid string, args map[string]string) {
	angelType := args["type"]
	if angelType == "" {
		s.sendError(conn, cid, "missing 'type' argument")
		return
	}
	cfg := &AngelConfig{
		Type:  angelType,
		ID:    args["id"],
		Paths: splitComma(args["paths"]),
	}
	if err := s.sup.SpawnAngel(ctx, cfg); err != nil {
		s.sendError(conn, cid, err.Error())
		return
	}
	s.sendOK(conn, cid, &ipc.AngelSummary{
		ID:        cfg.ID,
		AngelType: cfg.Type,
		State:     string(registry.StateCreated),
	})
}

func (s *Server) cliAngelList(conn *ipc.Conn, cid string) {
	entries := s.sup.ListEntries()
	summaries := make([]ipc.AngelSummary, 0, len(entries))
	for _, e := range entries {
		e.mu.Lock()
		sum := ipc.AngelSummary{
			ID:           e.ID,
			AngelType:    e.AngelType,
			State:        string(e.State),
			ConnState:    e.ConnTrack.State().String(),
			PID:          e.PID,
			RestartCount: e.RestartCount,
			LastSeen:     e.LastHeartbeat,
		}
		e.mu.Unlock()
		summaries = append(summaries, sum)
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].ID < summaries[j].ID
	})
	s.sendOK(conn, cid, summaries)
}

func (s *Server) cliAngelInspect(conn *ipc.Conn, cid string, args map[string]string) {
	id := args["id"]
	if id == "" {
		s.sendError(conn, cid, "missing 'id' argument")
		return
	}
	entry, ok := s.sup.GetEntry(id)
	if !ok {
		s.sendError(conn, cid, fmt.Sprintf("angel %q not found", id))
		return
	}
	a, err := s.reg.GetAngel(id)
	if err != nil {
		s.sendError(conn, cid, err.Error())
		return
	}
	events, _ := s.reg.ListEvents(id, 10)
	ipcEvents := make([]ipc.EventPayload, 0, len(events))
	for _, ev := range events {
		ipcEvents = append(ipcEvents, ipc.EventPayload{
			AngelID:   ev.AngelID,
			Severity:  parseSeverity(ev.Severity),
			Message:   ev.Message,
			Timestamp: ev.OccurredAt,
		})
	}
	entry.mu.Lock()
	detail := ipc.AngelDetail{
		AngelSummary: ipc.AngelSummary{
			ID:           entry.ID,
			AngelType:    entry.AngelType,
			State:        string(entry.State),
			ConnState:    entry.ConnTrack.State().String(),
			PID:          entry.PID,
			RestartCount: entry.RestartCount,
			LastSeen:     entry.LastHeartbeat,
		},
		CreatedAt:    a.CreatedAt,
		ConfigJSON:   a.ConfigJSON,
		Telemetry:    entry.Telemetry,
		RecentEvents: ipcEvents,
	}
	entry.mu.Unlock()
	s.sendOK(conn, cid, detail)
}

func (s *Server) cliAngelTerminate(conn *ipc.Conn, cid string, args map[string]string) {
	id := args["id"]
	if id == "" {
		s.sendError(conn, cid, "missing 'id' argument")
		return
	}
	if err := s.sup.TerminateAngel(id); err != nil {
		s.sendError(conn, cid, err.Error())
		return
	}
	s.sendOK(conn, cid, map[string]string{"id": id, "status": "terminated"})
}

// cliAngelDiff compares the watched files of a Guardian angel against their
// stored snapshots and returns a DiffResult for each changed/missing file.
func (s *Server) cliAngelDiff(conn *ipc.Conn, cid string, args map[string]string) {
	id := args["id"]
	if id == "" {
		s.sendError(conn, cid, "missing 'id' argument")
		return
	}

	entry, ok := s.sup.GetEntry(id)
	if !ok {
		s.sendError(conn, cid, fmt.Sprintf("angel %q not found", id))
		return
	}

	entry.mu.Lock()
	angelType := entry.AngelType
	var snapshotDir string
	var watchedPaths []string
	if entry.Config != nil {
		snapshotDir  = entry.Config.SnapshotDir
		watchedPaths = entry.Config.Paths
	}
	entry.mu.Unlock()

	if angelType != "guardian" {
		s.sendError(conn, cid, fmt.Sprintf("angel.diff only applies to guardian angels, %q is %q", id, angelType))
		return
	}
	if snapshotDir == "" {
		s.sendError(conn, cid, "angel has no snapshot_dir configured")
		return
	}

	diffs, err := guardian.DiffSnapshot(snapshotDir, watchedPaths)
	if err != nil {
		s.sendError(conn, cid, "diff: "+err.Error())
		return
	}

	// Convert guardian.DiffEntry to the msgpack-friendly wire type.
	type wireDiff struct {
		Path         string `msgpack:"path"`
		SnapshotHash string `msgpack:"snapshot_hash"`
		CurrentHash  string `msgpack:"current_hash"`
		Missing      bool   `msgpack:"current_missing"`
		SizeDelta    int64  `msgpack:"size_delta"`
		SnapshotAge  string `msgpack:"snapshot_age"`
	}
	wireDiffs := make([]wireDiff, len(diffs))
	for i, d := range diffs {
		wireDiffs[i] = wireDiff{
			Path:         d.Path,
			SnapshotHash: d.SnapshotHash,
			CurrentHash:  d.CurrentHash,
			Missing:      d.Missing,
			SizeDelta:    d.SizeDelta(),
			SnapshotAge:  d.SnapshotAge(),
		}
	}
	s.sendOK(conn, cid, struct {
		AngelID string      `msgpack:"angel_id"`
		Diffs   interface{} `msgpack:"diffs"`
	}{AngelID: id, Diffs: wireDiffs})
}

func (s *Server) cliLabStatus(conn *ipc.Conn, cid string) {
	entries := s.sup.ListEntries()
	angels := make([]ipc.AngelSummary, 0, len(entries))
	for _, e := range entries {
		e.mu.Lock()
		sum := ipc.AngelSummary{
			ID:           e.ID,
			AngelType:    e.AngelType,
			State:        string(e.State),
			ConnState:    e.ConnTrack.State().String(),
			PID:          e.PID,
			RestartCount: e.RestartCount,
			LastSeen:     e.LastHeartbeat,
		}
		e.mu.Unlock()
		angels = append(angels, sum)
	}
	sort.Slice(angels, func(i, j int) bool { return angels[i].ID < angels[j].ID })

	status := ipc.LabStatus{
		Version:   version.Version,
		PID:       os.Getpid(),
		Uptime:    int64(time.Since(s.startedAt).Seconds()),
		StartedAt: s.startedAt,
		Angels:    angels,
	}
	s.sendOK(conn, cid, status)
}

// cliEventStream subscribes the CLI connection to the event broadcaster.
// It holds the connection open and pushes KindEventStream frames until
// ctx is cancelled or the client disconnects.
func (s *Server) cliEventStream(ctx context.Context, conn *ipc.Conn, cid string) {
	// Acknowledge subscription.
	s.sendOK(conn, cid, map[string]string{"status": "streaming"})

	ch, cancel := s.bcast.Subscribe(128)
	defer cancel()

	// Remove deadline — streaming connections are indefinite.
	_ = conn.SetDeadline(time.Time{})

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			payload, err := ipc.EncodePayload(ev)
			if err != nil {
				continue
			}
			if err := conn.Send(&ipc.Message{
				Version: ipc.ProtocolVersion,
				Kind:    ipc.KindEventStream,
				Payload: payload,
			}); err != nil {
				return // client disconnected
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Response helpers
// ---------------------------------------------------------------------------

func (s *Server) sendOK(conn *ipc.Conn, cid string, data any) {
	payload, err := ipc.EncodePayload(data)
	if err != nil {
		s.sendError(conn, cid, "encode: "+err.Error())
		return
	}
	respPayload, _ := ipc.EncodePayload(&ipc.CLIResponse{OK: true, Data: payload})
	_ = conn.Send(&ipc.Message{
		Version:       ipc.ProtocolVersion,
		Kind:          ipc.KindCLIResponse,
		CorrelationID: cid,
		Payload:       respPayload,
	})
}

func (s *Server) sendError(conn *ipc.Conn, cid, errMsg string) {
	payload, _ := ipc.EncodePayload(&ipc.CLIResponse{OK: false, Error: errMsg})
	_ = conn.Send(&ipc.Message{
		Version:       ipc.ProtocolVersion,
		Kind:          ipc.KindCLIResponse,
		CorrelationID: cid,
		Payload:       payload,
	})
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

func splitComma(s string) []string {
	if s == "" {
		return nil
	}
	parts := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

func parseSeverity(s string) ipc.Severity {
	switch s {
	case "WARN":
		return ipc.SeverityWarn
	case "CRIT":
		return ipc.SeverityCritical
	default:
		return ipc.SeverityInfo
	}
}
