// Package version is the platform's single answer to "which code is this?".
//
// The value it returns is not decoration. It reaches two places that outlive the
// process:
//
//	observability.New(ServiceVersion:) — the OTel resource on every span
//	bus.ProducerConfig{ProducerVersion:} — envelope field 14, which the schema
//	                                       marks REQUIRED and describes as "the
//	                                       first thing needed to debug a bad event"
//
// So a wrong version is not a cosmetic label on a dashboard; it is stamped into
// durable event data, and it is what someone reads first at the worst moment.
//
// # Why this package exists (#143)
//
// There were 28 separate `func version() string` — one per binary — in three
// mutually inconsistent implementations:
//
//	14 returned the literal "dev"
//	12 read debug.ReadBuildInfo()'s vcs.revision, falling back to "dev"
//	 1 read $KANZ_VERSION, falling back to "dev"
//	 1 returned the literal "0.1.0", forever
//
// The twelve that read vcs.revision look correct, and are correct on a
// developer's machine. They are NOT correct in anything shipped: .dockerignore
// excludes .git (line 1), so a container build has no VCS metadata,
// ReadBuildInfo finds no vcs.revision, and every one of them silently returns
// "dev" — from an image built at a signed release tag.
//
// That is the failure this repository names specifically: "nothing configured"
// and "checked, and fine" looked identical, and the difference was invisible
// precisely where it mattered and visible precisely where it did not.
//
// # The fallback is deliberately not "dev"
//
// "dev" is a plausible value. It reads as a deliberate choice by whoever built
// the thing, which is why nobody noticed 26 binaries reporting it from
// production images. "unstamped" is not a version anyone would choose on
// purpose, so it survives being read by a human without being believed.
package version

import (
	"runtime/debug"
	"sync"
)

// stamped is set at link time by the release build:
//
//	-ldflags "-X github.com/eighred/kanz/internal/version.stamped=v0.2.0"
//
// It is a var, unexported, and written by exactly one mechanism. Do not assign
// it from code: a version the process can change is a version that cannot be
// trusted to identify the binary.
var stamped string

// Unstamped is what String reports when the build carried no version and no VCS
// metadata — the container-build case above. It is exported so a guard, a test
// or a startup check can name the state rather than matching a string literal
// that might be edited on one side only.
const Unstamped = "unstamped"

var (
	once     sync.Once
	resolved string
)

// String returns the version of this binary, resolved once.
//
// Precedence, most trustworthy first:
//
//	the link-time stamp        a release build, so the release tag
//	VCS revision from the build a local build from a git tree, so the SHA —
//	                           suffixed "-dirty" when the tree had uncommitted
//	                           changes, because a binary built from a dirty tree
//	                           cannot be reproduced from the SHA it claims
//	Unstamped                  neither, which is a build configuration defect,
//	                           and says so rather than guessing
func String() string {
	once.Do(func() { resolved = resolve() })
	return resolved
}

// IsStamped reports whether this binary carries a link-time version. A service
// can use it to say loudly at startup that its telemetry and every FACT it
// publishes will be attributed to a build nobody can identify.
func IsStamped() bool { return stamped != "" }

func resolve() string {
	rev, ok := fromBuildInfo()
	return choose(stamped, rev, ok)
}

// choose is the precedence itself, separated from where the two inputs come
// from. A test cannot un-stamp its own test binary or strip its VCS metadata, so
// a version of this logic that read the process's real build state could only
// ever be exercised in whichever state the test happened to run in — and the
// case that matters (no stamp, no VCS) is exactly the one a developer's machine
// cannot produce.
func choose(stamp, rev string, revOK bool) string {
	switch {
	case stamp != "":
		return stamp
	case revOK && rev != "":
		return rev
	default:
		return Unstamped
	}
}

// fromBuildInfo reads the revision Go records when building inside a VCS
// checkout. ok is false when the build carried no VCS metadata at all — which is
// every container build here, since .dockerignore excludes .git.
func fromBuildInfo() (string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "", false
	}
	if dirty {
		return rev + "-dirty", true
	}
	return rev, true
}
