package app

import (
	"errors"
	"regexp"
	"runtime/debug"
)

// version is the release this binary was cut from, stamped by the linker at
// release time (-X github.com/Chi-teck/da-transcript/internal/app.version=…,
// see .goreleaser.yml). Every other build leaves it empty, and buildVersion
// falls back to what the toolchain recorded.
var version string

// errVersionPrinted, like pflag.ErrHelp, is not a failure: the command line
// asked for something that has already gone to stdout.
var errVersionPrinted = errors.New("version printed")

// buildVersion names the build as well as it can be named. A release says its
// tag, and so does a clean `go build` of the tagged commit; any other
// `go build` inside a checkout says the commit it came from, marked +dirty
// when the tree had uncommitted changes; a `go install …@version` says the
// module version it was served as; a build from an unpacked source tree, with
// neither to read, can only say "unknown".
func buildVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	return versionFromBuildInfo(info)
}

// versionFromBuildInfo reads the build back out of what the toolchain recorded.
// A checkout build records both a commit and a Main.Version taken from it. On
// a tagged commit in a clean tree that version is the tag, the name a release
// and `go install …@tag` give the same code, so it wins. Anywhere else it is a
// pseudo-version (or the tag with +dirty) built around that same commit, and
// the commit comes first because it is the narrower answer. Only an install by
// module path — `go install …@latest` — has a version and no VCS to have taken
// it from.
func versionFromBuildInfo(info *debug.BuildInfo) string {
	var revision, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	if revision == "" {
		// "(devel)" is what a build with nothing to stamp reports; it names
		// the build no better than "unknown" does.
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
		return "unknown"
	}
	if v := info.Main.Version; dirty == "" && v != "" && v != "(devel)" && !pseudoVersion.MatchString(v) {
		return v
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	return revision + dirty
}

// pseudoVersion is golang.org/x/mod/module's pattern for the three shapes the
// toolchain gives a commit no tag names, +dirty or other build metadata
// allowed after any of them:
//
//	vX.0.0-yyyymmddhhmmss-abcdefabcdef          no tag below it
//	vX.Y.Z-pre.0.yyyymmddhhmmss-abcdefabcdef    past the pre-release vX.Y.Z-pre
//	vX.Y.(Z+1)-0.yyyymmddhhmmss-abcdefabcdef    past the release vX.Y.Z
var pseudoVersion = regexp.MustCompile(
	`^v[0-9]+\.(0\.0-|\d+\.\d+-([^+]*\.)?0\.)\d{14}-[A-Za-z0-9]+(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
