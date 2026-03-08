// lab — AngelLab CLI client.
//
// lab talks to the labd daemon via /run/angellab/lab.sock and renders
// responses for the operator.  It never talks to angels directly.
//
// Usage:
//
//	lab status                        show daemon and angel summary
//	lab tui                           live TUI dashboard (refreshes every 2s)
//	lab angel list                    list all angels
//	lab angel create <type> [flags]   spawn a new angel
//	lab angel inspect <id>            detailed view of one angel
//	lab angel diff <id>               show file diff vs last guardian snapshot
//	lab angel terminate <id>          gracefully stop an angel
//	lab events [--filter <str>]       stream live events (Ctrl-C to stop)
//	lab doctor                        check system prerequisites
//	lab version                       print version
//
// All commands (except tui and events) are synchronous request/response.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/nacreousdawn596/angellab/pkg/ipc"
	"github.com/nacreousdawn596/angellab/pkg/version"
)

// defaultSocket is the canonical lab.sock path.
// Override with LAB_SOCKET environment variable.
func socketPath() string {
	if s := os.Getenv("LAB_SOCKET"); s != "" {
		return s
	}
	return "/run/angellab/lab.sock"
}

// ---------------------------------------------------------------------------
// Entry point and command dispatch
// ---------------------------------------------------------------------------

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "status":
		cmdStatus()
	case "tui":
		cmdTUI()
	case "angel":
		cmdAngel(os.Args[2:])
	case "events":
		cmdEvents(os.Args[2:])
	case "doctor":
		cmdDoctor()
	case "version", "--version", "-v":
		fmt.Println(version.String())
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "lab: unknown command %q\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`Usage: lab <command> [arguments]

Commands:
  status                         show daemon and angel summary
  tui                            live TUI dashboard (Ctrl-C to exit)
  angel list                     list all angels
  angel create <type> [flags]    spawn a new angel
  angel inspect <id>             detailed view of one angel
  angel diff <id>                diff watched file vs guardian snapshot
  angel terminate <id>           gracefully stop an angel
  events [--filter <str>]        stream live events (Ctrl-C to stop)
  doctor                         check system prerequisites
  version                        print version

Angel create flags:
  --id <id>                      assign a specific ID (e.g. A-05)
  --paths <p1,p2,...>            watched paths (guardian only)

Events flags:
  --filter <str>                 only show events whose message contains str
  --since <duration>             only show events newer than duration (e.g. 5m)

Environment:
  LAB_SOCKET   override socket path (default: /run/angellab/lab.sock)
`)
}

// ---------------------------------------------------------------------------
// lab status
// ---------------------------------------------------------------------------

func cmdStatus() {
	c := dial()
	defer c.Close()

	resp, err := c.Request(ipc.CLICmdLabStatus, nil)
	die(err)
	if !resp.OK {
		fatalf("lab: %s", resp.Error)
	}

	var status ipc.LabStatus
	die(ipc.DecodeAs(resp.Data, &status))

	// Counts
	var active, unstable, contained int
	for _, a := range status.Angels {
		switch a.State {
		case "ACTIVE":
			active++
		case "UNSTABLE":
			unstable++
		case "CONTAINED":
			contained++
		}
	}

	uptime := time.Duration(status.Uptime) * time.Second

	sep := strings.Repeat("─", 64)
	fmt.Printf("\n[Angel Lab]  %s\n", sep[:40])
	fmt.Printf("  PID %-6d  Uptime %-12s  Started %s\n",
		status.PID,
		fmtDuration(uptime),
		status.StartedAt.Local().Format("2006-01-02 15:04:05"),
	)
	fmt.Printf("  Angels %-3d  Active %-3d  Unstable %-3d  Contained %d\n",
		len(status.Angels), active, unstable, contained)
	fmt.Printf("%s\n\n", sep)

	if len(status.Angels) == 0 {
		fmt.Println("  No angels registered. Try: lab angel create guardian")
		fmt.Println()
		return
	}

	// Sort by ID for stable output.
	angels := status.Angels
	sort.Slice(angels, func(i, j int) bool { return angels[i].ID < angels[j].ID })

	// Header
	fmt.Printf("  %-6s  %-10s  %-10s  %-10s  %6s  %8s  %4s  %s\n",
		"ID", "TYPE", "STATE", "CONN", "CPU", "RSS", "FD", "RESTARTS")
	fmt.Printf("  %s\n", strings.Repeat("·", 72))

	for _, a := range angels {
		cpu, rss, fd := "—", "—", "—"
		// TODO: telemetry is embedded in the summary via lab.status in a future pass.
		// For now pull from inspect if needed.
		_ = cpu
		_ = rss
		_ = fd

		stateColor := stateStr(a.State)
		fmt.Printf("  %-6s  %-10s  %-10s  %-10s  %6s  %8s  %4s  %d\n",
			a.ID,
			a.AngelType,
			stateColor,
			a.ConnState,
			"—",
			"—",
			"—",
			a.RestartCount,
		)
	}
	fmt.Println()
}

// ---------------------------------------------------------------------------
// lab angel *
// ---------------------------------------------------------------------------

func cmdAngel(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: lab angel <list|create|inspect|terminate>")
		os.Exit(1)
	}
	switch args[0] {
	case "list":
		cmdAngelList()
	case "create":
		cmdAngelCreate(args[1:])
	case "inspect":
		if len(args) < 2 {
			fatalf("Usage: lab angel inspect <id>")
		}
		cmdAngelInspect(args[1])
	case "diff":
		if len(args) < 2 {
			fatalf("Usage: lab angel diff <id>")
		}
		cmdAngelDiff(args[1])
	case "terminate":
		if len(args) < 2 {
			fatalf("Usage: lab angel terminate <id>")
		}
		cmdAngelTerminate(args[1])
	default:
		fatalf("lab angel: unknown subcommand %q", args[0])
	}
}

func cmdAngelList() {
	c := dial()
	defer c.Close()

	resp, err := c.Request(ipc.CLICmdAngelList, nil)
	die(err)
	if !resp.OK {
		fatalf("lab: %s", resp.Error)
	}

	var list []ipc.AngelSummary
	die(ipc.DecodeAs(resp.Data, &list))

	if len(list) == 0 {
		fmt.Println("No angels registered.")
		return
	}

	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })

	fmt.Printf("\n  %-6s  %-10s  %-11s  %-12s  %5s  %s\n",
		"ID", "TYPE", "STATE", "CONN", "PID", "LAST SEEN")
	fmt.Printf("  %s\n", strings.Repeat("·", 62))
	for _, a := range list {
		lastSeen := "never"
		if !a.LastSeen.IsZero() {
			lastSeen = fmtAgo(time.Since(a.LastSeen))
		}
		fmt.Printf("  %-6s  %-10s  %-11s  %-12s  %5d  %s\n",
			a.ID, a.AngelType, a.State, a.ConnState, a.PID, lastSeen)
	}
	fmt.Println()
}

func cmdAngelCreate(args []string) {
	if len(args) == 0 {
		fatalf("Usage: lab angel create <type> [--id <id>] [--paths <p1,p2>]")
	}
	angelType := args[0]
	reqArgs := map[string]string{"type": angelType}

	// Parse optional flags.
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--id":
			if i+1 >= len(args) {
				fatalf("--id requires a value")
			}
			i++
			reqArgs["id"] = args[i]
		case "--paths":
			if i+1 >= len(args) {
				fatalf("--paths requires a value")
			}
			i++
			reqArgs["paths"] = args[i]
		default:
			fatalf("unknown flag: %s", args[i])
		}
	}

	c := dial()
	defer c.Close()

	resp, err := c.Request(ipc.CLICmdAngelCreate, reqArgs)
	die(err)
	if !resp.OK {
		fatalf("lab: %s", resp.Error)
	}

	var summary ipc.AngelSummary
	die(ipc.DecodeAs(resp.Data, &summary))

	fmt.Printf("\n[Angel Lab]\n\n")
	fmt.Printf("  Created  %s  (%s)  %s\n\n", summary.ID, summary.AngelType, summary.State)
}

func cmdAngelInspect(id string) {
	c := dial()
	defer c.Close()

	resp, err := c.Request(ipc.CLICmdAngelInspect, map[string]string{"id": id})
	die(err)
	if !resp.OK {
		fatalf("lab: %s", resp.Error)
	}

	var detail ipc.AngelDetail
	die(ipc.DecodeAs(resp.Data, &detail))

	sep := strings.Repeat("─", 48)

	fmt.Printf("\nAngel %s  (%s)\n%s\n", detail.ID, detail.AngelType, sep)
	fmt.Printf("  %-14s %s\n", "State:", detail.State)
	fmt.Printf("  %-14s %s\n", "Connection:", detail.ConnState)
	fmt.Printf("  %-14s %d\n", "PID:", detail.PID)
	fmt.Printf("  %-14s %d\n", "Restarts:", detail.RestartCount)
	fmt.Printf("  %-14s %s\n", "Created:", detail.CreatedAt.Local().Format(time.RFC3339))
	if !detail.LastSeen.IsZero() {
		fmt.Printf("  %-14s %s  (%s ago)\n",
			"Last Seen:",
			detail.LastSeen.Local().Format(time.RFC3339),
			fmtAgo(time.Since(detail.LastSeen)),
		)
	}

	if t := detail.Telemetry; t != nil {
		fmt.Printf("\nTelemetry\n%s\n", sep[:24])
		fmt.Printf("  %-14s %.1f%%\n", "CPU:", t.CPUPercent)
		fmt.Printf("  %-14s %s\n", "RSS:", fmtBytes(t.RSSBytes))
		fmt.Printf("  %-14s %d\n", "FD Count:", t.FDCount)
		fmt.Printf("  %-14s %d\n", "Goroutines:", t.Goroutines)
		fmt.Printf("  %-14s %s\n", "Uptime:", fmtDuration(time.Duration(t.Uptime)*time.Second))
		if len(t.AngelMeta) > 0 {
			for k, v := range t.AngelMeta {
				fmt.Printf("  %-14s %s\n", k+":", v)
			}
		}
	}

	if len(detail.RecentEvents) > 0 {
		fmt.Printf("\nRecent Events\n%s\n", sep[:24])
		for _, ev := range detail.RecentEvents {
			fmt.Printf("  %s  %-4s  %s\n",
				ev.Timestamp.Local().Format("2006-01-02T15:04:05"),
				ev.Severity.String(),
				ev.Message,
			)
		}
	}
	fmt.Println()
}

func cmdAngelTerminate(id string) {
	c := dial()
	defer c.Close()

	resp, err := c.Request(ipc.CLICmdAngelTerminate, map[string]string{"id": id})
	die(err)
	if !resp.OK {
		fatalf("lab: %s", resp.Error)
	}
	fmt.Printf("\n[Angel Lab]  Angel %s terminated.\n\n", id)
}

// ---------------------------------------------------------------------------
// lab events
// ---------------------------------------------------------------------------

func cmdEvents(args []string) {
	// Parse --filter and --since flags.
	var filterStr string
	var since time.Duration
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--filter":
			if i+1 >= len(args) {
				fatalf("--filter requires a value")
			}
			i++
			filterStr = args[i]
		case "--since":
			if i+1 >= len(args) {
				fatalf("--since requires a value")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				fatalf("--since: invalid duration %q", args[i])
			}
			since = d
		}
	}

	c := dial()
	// Do not defer c.Close() here — we want it open for the full stream.

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ch, err := c.EventStream(ctx)
	die(err)

	filterDesc := ""
	if filterStr != "" {
		filterDesc = fmt.Sprintf(" [filter: %q]", filterStr)
	}
	fmt.Printf("\n[Angel Lab]  following events%s — Ctrl-C to stop\n\n", filterDesc)

	sinceTime := time.Time{}
	if since > 0 {
		sinceTime = time.Now().Add(-since)
	}

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				fmt.Println("\n[connection closed]")
				return
			}
			// Apply --since filter.
			if !sinceTime.IsZero() && ev.Timestamp.Before(sinceTime) {
				continue
			}
			// Apply --filter.
			if filterStr != "" && !strings.Contains(ev.Message, filterStr) {
				continue
			}
			printEvent(ev)
		case <-ctx.Done():
			fmt.Println()
			return
		}
	}
}

func printEvent(ev *ipc.EventPayload) {
	ts := ev.Timestamp.Local().Format("2006-01-02T15:04:05")
	fmt.Printf("%s  %-4s  [%s]  %s\n",
		ts,
		ev.Severity.String(),
		ev.AngelID,
		ev.Message,
	)
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

func stateStr(state string) string {
	return state
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func fmtAgo(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func fmtBytes(b uint64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
	)
	switch {
	case b >= GiB:
		return fmt.Sprintf("%.1f GiB", float64(b)/GiB)
	case b >= MiB:
		return fmt.Sprintf("%.1f MiB", float64(b)/MiB)
	case b >= KiB:
		return fmt.Sprintf("%.1f KiB", float64(b)/KiB)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// ---------------------------------------------------------------------------
// Error helpers
// ---------------------------------------------------------------------------

func dial() *ipc.Client {
	c, err := ipc.NewClient(socketPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab: cannot connect to daemon: %v\n", err)
		os.Exit(1)
	}
	return c
}

func die(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab: %v\n", err)
		os.Exit(1)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "lab: "+format+"\n", args...)
	os.Exit(1)
}
