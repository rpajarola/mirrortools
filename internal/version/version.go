// Package version reports the version of the mirror command.
package version

import (
	"runtime/debug"
	"strings"
)

// Version is set by packaging builds with
// -ldflags "-X github.com/rpajarola/mirrortools/internal/version.Version=1.2.3".
var Version = ""

// String returns the version: the one set at build time, the module
// version for go install, or "devel".
func String() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}
	return "devel"
}
