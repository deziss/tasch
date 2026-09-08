// Package version records what build this binary is.
//
// Nothing identified the build before: there was no --version flag, no commit, and the only
// machine-readable version numbers were duplicated across the Makefile and nfpm.yaml, both
// still saying 0.1.0 while the project shipped v0.8.0. That made "am I running the patched
// build?" unanswerable on a deployed node.
package version

import (
	"fmt"
	"runtime"
)

// Set via -ldflags at build time; see the Makefile.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// String returns a one-line description of this build.
func String() string {
	return fmt.Sprintf("tasch %s (commit %s, built %s, %s %s/%s)",
		Version, Commit, BuildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
