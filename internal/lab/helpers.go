package lab

import "strings"

// newConfigReader wraps a JSON config string as an io.Reader for cmd.Stdin.
// This avoids exposing config on the command line (/proc/<pid>/cmdline).
func newConfigReader(configJSON string) *strings.Reader {
	return strings.NewReader(configJSON)
}
