// Package version holds build-time metadata injected via -ldflags.
//
// The Makefile sets these with:
//
//	-X github.com/nacreousdawn596/angellab/pkg/version.Version=$(git describe …)
//	-X github.com/nacreousdawn596/angellab/pkg/version.BuildTime=…
//	-X github.com/nacreousdawn596/angellab/pkg/version.Commit=…
package version

import "fmt"

// These variables are overwritten at link time by the Makefile.
// Default values are used when building outside the repo (e.g. go run).
var (
	Version   = "dev"
	BuildTime = "unknown"
	Commit    = "unknown"
)

// String returns a single-line version string suitable for --version output.
func String() string {
	return fmt.Sprintf("AngelLab %s (commit %s, built %s)", Version, Commit, BuildTime)
}
