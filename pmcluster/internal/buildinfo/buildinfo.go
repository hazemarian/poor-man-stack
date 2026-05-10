// Package buildinfo holds build-time variables (version, commit, date) that
// are populated via -ldflags by the Makefile. Living in a leaf package
// rather than internal/cli avoids import cycles when other packages need to
// report version (e.g. internal/api/health.go).
package buildinfo

import "runtime/debug"

// Build-time variables, populated via -ldflags by the Makefile.
// Defaults make `go run` and `go install` work without ldflags.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// Resolve prefers ldflags-injected values; if those are still the defaults
// (e.g. `go install` without ldflags), falls back to the VCS info embedded
// by the Go toolchain.
func Resolve() (version, commit, date string) {
	version, commit, date = Version, Commit, Date
	if version != "dev" {
		return
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if commit == "unknown" {
				commit = s.Value
			}
		case "vcs.time":
			if date == "unknown" {
				date = s.Value
			}
		}
	}
	return
}
