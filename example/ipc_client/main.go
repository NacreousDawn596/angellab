// example/ipc_client — programmatic IPC client for AngelLab.
//
// This example demonstrates how to talk to a running Lab daemon from Go code:
//
//   - Fetch daemon status (version, uptime, angel list)
//   - Inspect a specific angel
//   - Subscribe to the live event stream
//   - Create and terminate an angel programmatically
//
// This is the same path taken by the `lab` CLI binary.  Any Go program
// can embed this pattern to build custom dashboards, alerting integrations,
// or automation scripts on top of AngelLab.
//
// Prerequisites:
//   - A running labd daemon:  sudo systemctl start angellab
//   - Or the basic_guardian example in another terminal
//
// Run:
//
//	go run ./example/ipc_client [--socket /path/to/lab.sock]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nacreousdawn596/angellab/pkg/ipc"
)

func main() {
	socketPath := flag.String("socket", "/run/angellab/lab.sock", "Lab socket path")
	flag.Parse()

	fmt.Printf("AngelLab IPC client example\n")
	fmt.Printf("connecting to %s\n\n", *socketPath)

	// -----------------------------------------------------------------------
	// 1. Fetch daemon status
	// -----------------------------------------------------------------------

	fmt.Println("── Daemon status ─────────────────────────────────────────")

	c, err := ipc.NewClient(*socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial: %v\nIs the daemon running? Try: sudo systemctl start angellab\n", err)
		os.Exit(1)
	}

	resp, err := c.Request(ipc.CLICmdLabStatus, nil)
	mustOK(err, resp, "lab.status")
	c.Close()

	var status ipc.LabStatus
	mustDecode(ipc.DecodeAs(resp.Data, &status))

	fmt.Printf("  Version  : %s\n", status.Version)
	fmt.Printf("  PID      : %d\n", status.PID)
	fmt.Printf("  Uptime   : %s\n", fmtDuration(time.Duration(status.Uptime)*time.Second))
	fmt.Printf("  Angels   : %d\n\n", len(status.Angels))

	// -----------------------------------------------------------------------
	// 2. List angels
	// -----------------------------------------------------------------------

	fmt.Println("── Angel list ─────────────────────────────────────────────")

	c2, _ := ipc.NewClient(*socketPath)
	resp2, err := c2.Request(ipc.CLICmdAngelList, nil)
	mustOK(err, resp2, "angel.list")
	c2.Close()

	var list []ipc.AngelSummary
	mustDecode(ipc.DecodeAs(resp2.Data, &list))

	for _, a := range list {
		fmt.Printf("  %-8s  %-10s  %-11s  pid %-6d  restarts %d\n",
			a.ID, a.AngelType, a.State, a.PID, a.RestartCount)
	}
	fmt.Println()

	// -----------------------------------------------------------------------
	// 3. Inspect the first angel (if any)
	// -----------------------------------------------------------------------

	if len(list) > 0 {
		fmt.Printf("── Inspect %s ──────────────────────────────────────────\n", list[0].ID)

		c3, _ := ipc.NewClient(*socketPath)
		resp3, err := c3.Request(ipc.CLICmdAngelInspect, map[string]string{"id": list[0].ID})
		mustOK(err, resp3, "angel.inspect")
		c3.Close()

		var detail ipc.AngelDetail
		mustDecode(ipc.DecodeAs(resp3.Data, &detail))

		fmt.Printf("  ID         : %s\n", detail.ID)
		fmt.Printf("  Type       : %s\n", detail.AngelType)
		fmt.Printf("  State      : %s\n", detail.State)
		fmt.Printf("  ConnState  : %s\n", detail.ConnState)
		fmt.Printf("  PID        : %d\n", detail.PID)
		fmt.Printf("  Restarts   : %d\n", detail.RestartCount)
		fmt.Printf("  Created    : %s\n", detail.CreatedAt.Local().Format(time.RFC3339))
		if detail.Telemetry != nil {
			t := detail.Telemetry
			fmt.Printf("  RSS        : %s\n", fmtBytes(t.RSSBytes))
			fmt.Printf("  CPU%%       : %.1f%%\n", t.CPUPercent)
			fmt.Printf("  Goroutines : %d\n", t.Goroutines)
			fmt.Printf("  FDs        : %d\n", t.FDCount)
		}
		fmt.Println()
	}

	// -----------------------------------------------------------------------
	// 4. Subscribe to the event stream (10 seconds)
	// -----------------------------------------------------------------------

	fmt.Println("── Event stream (10s — press Ctrl-C to stop early) ───────")

	ctx, cancel := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT, syscall.SIGTERM,
	)
	defer cancel()

	// Auto-cancel after 10 seconds so the example terminates.
	streamCtx, streamCancel := context.WithTimeout(ctx, 10*time.Second)
	defer streamCancel()

	cStream, err := ipc.NewClient(*socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stream dial: %v\n", err)
		os.Exit(1)
	}

	ch, err := cStream.EventStream(streamCtx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "event stream: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("  (waiting for events — trigger one by modifying a watched file)")
	fmt.Println()

	count := 0
	for ev := range ch {
		fmt.Printf("  %s  %-4s  [%s]  %s\n",
			ev.Timestamp.Local().Format("15:04:05"),
			ev.Severity.String(),
			ev.AngelID,
			ev.Message,
		)
		count++
	}

	if count == 0 {
		fmt.Println("  (no events received — system is quiet)")
	}
	fmt.Println()
	fmt.Printf("received %d event(s)\n", count)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustOK(err error, resp *ipc.CLIResponse, cmd string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		os.Exit(1)
	}
	if !resp.OK {
		fmt.Fprintf(os.Stderr, "%s failed: %s\n", cmd, resp.Error)
		os.Exit(1)
	}
}

func mustDecode(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "decode: %v\n", err)
		os.Exit(1)
	}
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func fmtBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
