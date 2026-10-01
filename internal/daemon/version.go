package daemon

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/salehsayyadi/tuunel/internal/session"
)

// Version is set at build time (-X ...daemon.Version=<tag>).
var Version = "dev"

// Commit is set at build time (-X ...daemon.Commit=<sha>); when empty the Go
// toolchain's VCS stamp is used if present.
var Commit = ""

// BuildInfo returns version, commit and toolchain for `tuunel version`,
// the API and the tuunel_build_info metric.
func BuildInfo() (version, commit, goVersion string) {
	commit = Commit
	if commit == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" {
					commit = s.Value
				}
			}
		}
	}
	if commit == "" {
		commit = "unknown"
	}
	return Version, commit, runtime.Version()
}

// VersionString is the human-readable `tuunel version` output.
func VersionString() string {
	v, c, g := BuildInfo()
	return fmt.Sprintf("tuunel %s\ncommit:   %s\ngo:       %s\nplatform: %s/%s\nprotocol: %d", v, c, g, runtime.GOOS, runtime.GOARCH, session.ProtocolVersion)
}
