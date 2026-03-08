// Angel — the AngelLab worker binary.
//
// All angel types are compiled into this single binary and selected via
// subcommand.  Lab spawns angels as:
//
//	angel guardian --id A-01 --lab /run/angellab/lab.sock
//	angel sentinel --id A-02 --lab /run/angellab/lab.sock
//	angel memory   --id A-03 --lab /run/angellab/lab.sock
//
// Config is passed as a msgpack blob on stdin immediately after the process
// starts.  The angel reads it, connects to Lab, sends REGISTER, then enters
// its monitoring loop.
//
// Every angel implementation satisfies the Angel interface defined in
// internal/angels/base.go.
package main

import (
	"fmt"
	"os"

	"github.com/nacreousdawn596/angellab/internal/angels/guardian"
	"github.com/nacreousdawn596/angellab/internal/angels/memory"
	"github.com/nacreousdawn596/angellab/internal/angels/process"
	"github.com/nacreousdawn596/angellab/internal/angels/sentinel"
)

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: angel <type> --id <ID> --lab <socket>

Angel types:
  guardian   file integrity monitor (inotify + snapshots)
  sentinel   network anomaly detector (/proc/net + netlink)
  memory     process memory monitor (/proc + cgroup v2)
  scheduler  periodic task runner

Flags (common to all types):
  --id <ID>      angel identifier, e.g. A-01
  --lab <path>   path to lab.sock, e.g. /run/angellab/lab.sock

Config is read as a msgpack-encoded blob from stdin on startup.
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	angelType := os.Args[1]
	// Shift so that each sub-handler sees its own flags starting at os.Args[1].
	os.Args = append([]string{os.Args[0]}, os.Args[2:]...)

	switch angelType {
	case "guardian":
		guardian.Run()
	case "sentinel":
		sentinel.Run()
	case "memory":
		memory.Run()
	case "process":
		process.Run()
	default:
		fmt.Fprintf(os.Stderr, "angel: unknown type %q\n", angelType)
		usage()
	}
}
