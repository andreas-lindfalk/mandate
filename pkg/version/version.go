// Package version is the release a binary was built from, the one value the
// server, the command line and the MCP server and client all report.
//
// A release build sets it:
//
//	go build -ldflags "-X github.com/andreas-lindfalk/pgmandate/pkg/version.Version=v0.1.0" ./cmd/pgmandate
//
// Otherwise it is what Go stamped into the build from git (a tag, or a
// pseudo-version with the commit, marked +dirty for uncommitted changes),
// and "dev" when there is nothing to go by, as with go run.
package version

import "runtime/debug"

// Version is set at link time for a release; empty otherwise.
var Version = ""

// String is the version to report.
func String() string {
	if Version != "" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	return fromBuild(info)
}

// fromBuild reads the version Go stamped, or the commit when there is no
// module version.
func fromBuild(info *debug.BuildInfo) string {
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	revision, modified := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision == "" {
		return "dev"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified {
		return "dev-" + revision + "+dirty"
	}
	return "dev-" + revision
}
