// lab tui — live terminal dashboard for AngelLab.
//
// Renders a full-screen dashboard that refreshes every 2 seconds.
// Requires a terminal that supports ANSI escape codes (all modern terminals).
// Zero external dependencies — every escape sequence is written by hand.
//
// Layout:
//
//	┌──────────────────────────────────────────────────────────────┐
//	│  AngelLab  v1.2.0   pid 1234   uptime 4h23m   angels 3/3    │
//	├──────────────────────────────────────────────────────────────┤
//	│  ID      TYPE       STATE      CONN       CPU    RSS    FD   │
//	│  A-01    guardian   ACTIVE     ACTIVE     0.1%   8MiB   12   │
//	│  A-02    sentinel   ACTIVE     ACTIVE     0.3%   7MiB   15   │
//	│  A-03    process    TRAINING   CONNECTING —      —      —    │
//	├──────────────────────────────────────────────────────────────┤
//	│  Recent events                                               │
//	│  14:22:01  WARN  [A-02]  suspicious outbound → 185.x.x.x    │
//	│  14:21:55  INFO  [A-03]  baseline learning (12 procs)        │
//	└──────────────────────────────────────────────────────────────┘
//	  Ctrl-C to exit · refreshes every 2s · lab angel inspect A-01
//
// The TUI uses two goroutines:
//   - Poller: calls lab.status every 2s and updates shared state.
//   - Streamer: holds an event.subscribe connection and appends to the
//     event ring buffer.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nacreousdawn596/angellab/pkg/ipc"
)

// ---------------------------------------------------------------------------
// ANSI helpers
// ---------------------------------------------------------------------------

const (
	ansiReset     = "\033[0m"
	ansiBold      = "\033[1m"
	ansiDim       = "\033[2m"
	ansiRed       = "\033[31m"
	ansiGreen     = "\033[32m"
	ansiYellow    = "\033[33m"
	ansiBlue      = "\033[34m"
	ansiMagenta   = "\033[35m"
	ansiCyan      = "\033[36m"
	ansiWhite     = "\033[37m"
	ansiBoldGreen  = "\033[1;32m"
	ansiBoldYellow = "\033[1;33m"
	ansiBoldRed    = "\033[1;31m"
	ansiBoldCyan   = "\033[1;36m"
	ansiBoldWhite  = "\033[1;37m"

	clearScreen  = "\033[2J"
	cursorHome   = "\033[H"
	hideCursor   = "\033[?25l"
	showCursor   = "\033[?25h"
	clearLine    = "\033[2K"
	eraseToeEOL  = "\033[0K"
)

func moveTo(row, col int) string { return fmt.Sprintf("\033[%d;%dH", row, col) }

// colorState returns ANSI colour codes for an angel state string.
func colorState(state string) string {
	switch state {
	case "ACTIVE":
		return ansiBoldGreen + state + ansiReset
	case "TRAINING":
		return ansiBoldCyan + state + ansiReset
	case "UNSTABLE":
		return ansiBoldYellow + state + ansiReset
	case "CONTAINED", "TERMINATED":
		return ansiBoldRed + state + ansiReset
	default:
		return ansiDim + state + ansiReset
	}
}

// colorConn returns colour for a connection state string.
func colorConn(conn string) string {
	switch conn {
	case "ACTIVE":
		return ansiGreen + conn + ansiReset
	case "REGISTERED", "CONNECTING":
		return ansiCyan + conn + ansiReset
	case "RECOVERING", "LOST":
		return ansiYellow + conn + ansiReset
	default:
		return ansiDim + conn + ansiReset
	}
}

// colorSeverity returns colour + label for an event severity.
func colorSeverity(s ipc.Severity) string {
	switch s {
	case ipc.SeverityInfo:
		return ansiCyan + "INFO" + ansiReset
	case ipc.SeverityWarn:
		return ansiYellow + "WARN" + ansiReset
	case ipc.SeverityCritical:
		return ansiBoldRed + "CRIT" + ansiReset
	default:
		return "????"
	}
}

// truncate shortens s to max runes, appending "…" if truncated.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}

// stripANSI approximates visible length by ignoring escape sequences.
// Used for column padding when colourised strings are mixed in.
func visLen(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if r == '\033' {
			inEsc = true
			continue
		}
		n++
	}
	return n
}

// padRight pads s with spaces to width, respecting ANSI escape codes.
func padRight(s string, width int) string {
	vis := visLen(s)
	if vis >= width {
		return s
	}
	return s + strings.Repeat(" ", width-vis)
}

// ---------------------------------------------------------------------------
// TUI state
// ---------------------------------------------------------------------------

const (
	tuiRefresh   = 2 * time.Second
	eventBufSize = 20 // number of recent events to display
)

// tuiState is shared between the poller and streamer goroutines.
type tuiState struct {
	mu      sync.RWMutex
	status  *ipc.LabStatus
	events  []*ipc.EventPayload // ring buffer, newest first
	lastErr error
	lastPoll time.Time
}

func (t *tuiState) setStatus(s *ipc.LabStatus) {
	t.mu.Lock()
	t.status = s
	t.lastPoll = time.Now()
	t.mu.Unlock()
}

func (t *tuiState) pushEvent(ev *ipc.EventPayload) {
	t.mu.Lock()
	t.events = append([]*ipc.EventPayload{ev}, t.events...)
	if len(t.events) > eventBufSize {
		t.events = t.events[:eventBufSize]
	}
	t.mu.Unlock()
}

// ---------------------------------------------------------------------------
// cmdTUI entry point
// ---------------------------------------------------------------------------

func cmdTUI() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	state := &tuiState{}

	// Hide cursor, clear screen on entry.
	fmt.Print(hideCursor + clearScreen)
	defer func() {
		// Restore terminal on exit.
		fmt.Print(showCursor + cursorHome + clearScreen)
	}()

	// --- Poller: fetches lab.status every tuiRefresh ---
	go func() {
		tick := time.NewTicker(tuiRefresh)
		defer tick.Stop()
		// Immediate first fetch.
		tuiFetchStatus(state)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				tuiFetchStatus(state)
			}
		}
	}()

	// --- Streamer: holds event.subscribe connection ---
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			c, err := ipc.NewClient(socketPath())
			if err != nil {
				time.Sleep(3 * time.Second)
				continue
			}
			ch, err := c.EventStream(ctx)
			if err != nil {
				c.Close()
				time.Sleep(3 * time.Second)
				continue
			}
			for ev := range ch {
				state.pushEvent(ev)
			}
			c.Close()
			// If ctx cancelled, exit. Otherwise reconnect.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()

	// --- Render loop ---
	renderTick := time.NewTicker(tuiRefresh)
	defer renderTick.Stop()

	for {
		tuiRender(state)
		select {
		case <-ctx.Done():
			return
		case <-renderTick.C:
		}
	}
}

// tuiFetchStatus dials, fetches lab.status, and updates state.
func tuiFetchStatus(state *tuiState) {
	c, err := ipc.NewClient(socketPath())
	if err != nil {
		state.mu.Lock()
		state.lastErr = err
		state.mu.Unlock()
		return
	}
	defer c.Close()

	resp, err := c.Request(ipc.CLICmdLabStatus, nil)
	if err != nil || !resp.OK {
		state.mu.Lock()
		if err != nil {
			state.lastErr = err
		}
		state.mu.Unlock()
		return
	}

	var s ipc.LabStatus
	if err := ipc.DecodeAs(resp.Data, &s); err != nil {
		return
	}
	state.setStatus(&s)
}

// ---------------------------------------------------------------------------
// Renderer
// ---------------------------------------------------------------------------

func tuiRender(state *tuiState) {
	state.mu.RLock()
	status  := state.status
	events  := make([]*ipc.EventPayload, len(state.events))
	copy(events, state.events)
	lastErr := state.lastErr
	lastPoll := state.lastPoll
	state.mu.RUnlock()

	var sb strings.Builder

	// Move to top-left.
	sb.WriteString(cursorHome)

	// Determine terminal width (default 80 if unknown).
	cols := termWidth()
	if cols < 60 {
		cols = 80
	}
	line := strings.Repeat("─", cols-2)

	// -----------------------------------------------------------------------
	// Header
	// -----------------------------------------------------------------------
	sb.WriteString(ansiBoldWhite)
	if status != nil {
		active := 0
		for _, a := range status.Angels {
			if a.State == "ACTIVE" {
				active++
			}
		}
		uptime := fmtDuration(time.Duration(status.Uptime) * time.Second)
		header := fmt.Sprintf("  AngelLab  %s   pid %d   uptime %s   angels %d/%d",
			ansiBoldCyan+status.Version+ansiBoldWhite,
			status.PID, uptime, active, len(status.Angels))
		sb.WriteString(padRight(header, cols))
	} else if lastErr != nil {
		sb.WriteString(padRight(fmt.Sprintf("  AngelLab  %sCannot connect to daemon%s", ansiRed, ansiBoldWhite), cols))
	} else {
		sb.WriteString(padRight("  AngelLab  connecting…", cols))
	}
	sb.WriteString(ansiReset + "\n")
	sb.WriteString(ansiDim + "  ┌" + line + "┐" + ansiReset + "\n")

	// -----------------------------------------------------------------------
	// Angels table header
	// -----------------------------------------------------------------------
	hdr := fmt.Sprintf("  │ %s%-6s  %-10s  %-11s  %-12s  %5s  %7s  %4s  %s│",
		ansiBold,
		"ID", "TYPE", "STATE", "CONN", "CPU", "RSS", "FD",
		ansiReset)
	sb.WriteString(padRight(hdr, cols+len(ansiBold)+len(ansiReset)) + "\n")
	sb.WriteString(ansiDim + "  │" + strings.Repeat("·", cols-2) + "│" + ansiReset + "\n")

	// -----------------------------------------------------------------------
	// Angel rows
	// -----------------------------------------------------------------------
	if status != nil {
		angels := status.Angels
		sort.Slice(angels, func(i, j int) bool { return angels[i].ID < angels[j].ID })

		for _, a := range angels {
			cpu := "—"
			rss := "—"
			fd  := "—"
			// Telemetry comes from heartbeat — shown if recently updated.
			// In the status response the live values aren't yet embedded
			// (inspect is needed for that), so we show the known summary.

			row := fmt.Sprintf("  │ %-6s  %-10s  %-22s  %-21s  %5s  %7s  %4s  │",
				ansiBoldWhite+a.ID+ansiReset,
				a.AngelType,
				colorState(a.State),
				colorConn(a.ConnState),
				cpu, rss, fd)
			sb.WriteString(padRight(row, cols+40) + "\n")
		}
	} else {
		sb.WriteString(fmt.Sprintf("  │  %swaiting for daemon...%s%-*s│\n",
			ansiDim, ansiReset, cols-24, ""))
	}

	sb.WriteString(ansiDim + "  ├" + line + "┤" + ansiReset + "\n")

	// -----------------------------------------------------------------------
	// Event feed
	// -----------------------------------------------------------------------
	feedHdr := fmt.Sprintf("  │ %sRecent events%s%-*s│",
		ansiBold, ansiReset, cols-17, "")
	sb.WriteString(padRight(feedHdr, cols+len(ansiBold)+len(ansiReset)) + "\n")

	maxEvents := 8
	if len(events) == 0 {
		sb.WriteString(fmt.Sprintf("  │  %sno events yet%s%-*s│\n",
			ansiDim, ansiReset, cols-17, ""))
	} else {
		shown := events
		if len(shown) > maxEvents {
			shown = shown[:maxEvents]
		}
		for _, ev := range shown {
			ts  := ev.Timestamp.Local().Format("15:04:05")
			sev := colorSeverity(ev.Severity)
			id  := ev.AngelID
			msg := truncate(ev.Message, cols-30)
			row := fmt.Sprintf("  │  %s  %s  [%s]  %s",
				ansiDim+ts+ansiReset,
				sev,
				ansiBoldWhite+id+ansiReset,
				msg)
			sb.WriteString(padRight(row, cols+40) + "│\n")
		}
	}

	sb.WriteString(ansiDim + "  └" + line + "┘" + ansiReset + "\n")

	// -----------------------------------------------------------------------
	// Footer
	// -----------------------------------------------------------------------
	pollDesc := ""
	if !lastPoll.IsZero() {
		pollDesc = fmt.Sprintf(" · polled %s ago", fmtAgo(time.Since(lastPoll)))
	}
	footer := fmt.Sprintf("  %sCtrl-C to exit · refreshes every 2s%s%s",
		ansiDim, pollDesc, ansiReset)
	sb.WriteString(footer + "\n")

	// Write entire frame at once (minimises flicker).
	fmt.Print(sb.String())
}

// termWidth tries to read the terminal width via tput or falls back to 80.
func termWidth() int {
	// Try $COLUMNS env var first (set by most shells).
	if cols := os.Getenv("COLUMNS"); cols != "" {
		var n int
		if _, err := fmt.Sscanf(cols, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 80
}

// ---------------------------------------------------------------------------
// lab angel diff <id>
// ---------------------------------------------------------------------------

func cmdAngelDiff(id string) {
	c := dial()
	defer c.Close()

	resp, err := c.Request(ipc.CLICmdAngelDiff, map[string]string{"id": id})
	die(err)
	if !resp.OK {
		fatalf("lab: %s", resp.Error)
	}

	var result AngelDiffResult
	die(ipc.DecodeAs(resp.Data, &result))

	if len(result.Diffs) == 0 {
		fmt.Printf("\n[Angel Lab]  Guardian %s — all watched files match snapshots\n\n", id)
		return
	}

	fmt.Printf("\n[Angel Lab]  Guardian %s — %d file(s) differ from snapshot\n\n", id, len(result.Diffs))

	for _, d := range result.Diffs {
		fmt.Printf("  %s  %s\n", ansiBoldWhite+d.Path+ansiReset, ansiDim+"("+d.SnapshotAge+")"+ansiReset)
		if d.CurrentMissing {
			fmt.Printf("    %sFILE MISSING — not present on disk%s\n", ansiRed, ansiReset)
		} else {
			fmt.Printf("    snapshot sha256: %s\n", ansiDim+d.SnapshotHash[:16]+"…"+ansiReset)
			fmt.Printf("    current  sha256: %s\n", ansiYellow+d.CurrentHash[:16]+"…"+ansiReset)
			if d.SizeDelta != 0 {
				sign := "+"
				if d.SizeDelta < 0 {
					sign = ""
				}
				fmt.Printf("    size delta:  %s%d bytes%s\n", ansiYellow, d.SizeDelta, ansiReset)
				_ = sign
			}
		}
		fmt.Println()
	}
}

// AngelDiffResult is the response body for the angel.diff CLI command.
type AngelDiffResult struct {
	AngelID string      `msgpack:"angel_id"`
	Diffs   []FileDiff  `msgpack:"diffs"`
}

// FileDiff describes one file that differs from its snapshot.
type FileDiff struct {
	Path           string `msgpack:"path"`
	SnapshotHash   string `msgpack:"snapshot_hash"`
	CurrentHash    string `msgpack:"current_hash"`
	CurrentMissing bool   `msgpack:"current_missing"`
	SizeDelta      int64  `msgpack:"size_delta"`
	SnapshotAge    string `msgpack:"snapshot_age"` // human-readable time since snapshot
}
