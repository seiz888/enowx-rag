// Package buildinfo reports which build of enowx-rag is running.
//
// Why this exists: until now the server answered "dev" to every question about
// its own identity -- `enowx-rag version`, the MCP handshake, the dashboard --
// while production ran a binary compiled from an uncommitted working tree. A
// deployment that cannot be identified cannot be fenced, rolled back, or
// reconciled against a source revision, which the shared-memory gateway work
// requires before it can fence legacy writers.
//
// The data comes from Go's own VCS stamping (debug.ReadBuildInfo), so an
// ordinary `go build` already carries it -- no build wrapper, no generated
// file, nothing to forget. Release builds may additionally inject the ldflags
// variables below; when they do, the injected values win, because a release
// tag is more meaningful than a raw commit hash.
//
// Nothing here invents a version. A build from a dirty tree reports
// dirty_tree=true and keeps the commit it was based on, rather than pretending
// to be that commit.
package buildinfo

import (
	"runtime/debug"
	"sync"
)

// Values injected at link time, e.g.
//
//	-ldflags "-X github.com/enowdev/enowx-rag/pkg/buildinfo.version=v0.4.0"
//
// They are optional: an unstamped build falls back to VCS build info.
var (
	version   = ""
	commitSHA = ""
	buildTime = ""
)

// Info describes the running binary.
type Info struct {
	// Version is the release version when one was injected, otherwise the
	// short commit SHA, otherwise "dev". A dirty build is suffixed "-dirty".
	Version string `json:"version"`
	// CommitSHA is the full revision the build was based on, "" when unknown.
	CommitSHA string `json:"commit_sha"`
	// BuildTime is the VCS commit time (RFC 3339), or the injected build time.
	BuildTime string `json:"build_time"`
	// DirtyTree reports that the working tree had uncommitted changes at build
	// time, so CommitSHA identifies the base revision, not the exact source.
	DirtyTree bool `json:"dirty_tree"`
	// GoVersion is the toolchain that produced the binary.
	GoVersion string `json:"go_version"`
}

var (
	once   sync.Once
	cached Info
)

// Get returns the build identity of the running binary. The result is computed
// once and reused; it cannot change while the process lives.
func Get() Info {
	once.Do(func() { cached = read() })
	return cached
}

// String returns the short human-readable form used by `enowx-rag version`,
// the MCP handshake and the startup log.
func String() string {
	return Get().Version
}

func read() Info {
	info := Info{
		Version:   version,
		CommitSHA: commitSHA,
		BuildTime: buildTime,
	}

	bi, ok := debug.ReadBuildInfo()
	if ok {
		info.GoVersion = bi.GoVersion
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if info.CommitSHA == "" {
					info.CommitSHA = s.Value
				}
			case "vcs.time":
				if info.BuildTime == "" {
					info.BuildTime = s.Value
				}
			case "vcs.modified":
				info.DirtyTree = s.Value == "true"
			}
		}
		// A module installed with `go install module@version` records a real
		// version here; a build of the main module records "(devel)", which
		// says nothing and is discarded.
		if info.Version == "" {
			if v := bi.Main.Version; v != "" && v != "(devel)" {
				info.Version = v
			}
		}
	}

	if info.Version == "" {
		if info.CommitSHA != "" {
			info.Version = shortSHA(info.CommitSHA)
		} else {
			info.Version = "dev"
		}
	}
	// The suffix is part of the version string on purpose: it travels through
	// the MCP handshake and the logs, where a bare SHA would otherwise imply a
	// clean build of that commit.
	if info.DirtyTree {
		info.Version += "-dirty"
	}
	return info
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
