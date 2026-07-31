package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// THE VERSION A BINARY REPORTS IS FORENSIC DATA, NOT A LABEL (#143).
//
// It reaches two places that outlive the process: the OTel resource on every span
// (observability.New's ServiceVersion) and envelope field 14, producer_version —
// which the schema marks REQUIRED and describes as "the first thing needed to
// debug a bad event". A wrong value is written into the durable archive.
//
// There were 28 separate `func version() string`, in three implementations that
// disagreed. Twelve read debug.ReadBuildInfo's vcs.revision and were correct on a
// developer's machine and WRONG in everything shipped: .dockerignore excludes
// .git, so a container build carries no VCS metadata, the lookup misses, and each
// one silently returned "dev" — from images built at a signed release tag.
//
// Two rules, because fixing either alone leaves the defect reachable:
//
//	1. one implementation      — internal/version, no local copies
//	2. every image stamps it   — or the one implementation falls back to
//	                             "unstamped" in exactly the artifact that matters
//
// Rule 2 is the one that would rot quietly. A new service's Dockerfile copied
// from an old one before this change compiles, runs, passes every test, ships,
// and reports "unstamped" forever with nothing failing.

// versionStampFlag is the linker flag that sets internal/version.stamped. The
// package path is spelled out rather than derived, because the point of the
// check is that the Dockerfile and the Go package agree — deriving one from the
// other would make them agree by construction and prove nothing.
const versionStampFlag = "-X github.com/eighred/kanz/internal/version.stamped="

// TestNoBinaryDefinesItsOwnVersion keeps the 28 copies from coming back one at a
// time, which is exactly how they arrived: #140 added the 25th and 26th to stay
// consistent with the surrounding convention, and both said so in a comment.
func TestNoBinaryDefinesItsOwnVersion(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	var offenders []string
	scanned := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", ".gotmp", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++

		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		// internal/version is where the one implementation lives.
		rel := filepath.ToSlash(mustRel(root, path))
		if strings.HasPrefix(rel, "internal/version/") {
			return nil
		}
		for _, decl := range f.Decls {
			fn, isFn := decl.(*ast.FuncDecl)
			if !isFn || fn.Recv != nil || fn.Name.Name != "version" {
				continue
			}
			if fn.Type.Params != nil && len(fn.Type.Params.List) > 0 {
				continue // some other `version(x)` helper, not the nullary copy
			}
			offenders = append(offenders, fmt.Sprintf("%s:%d", rel, fset.Position(fn.Pos()).Line))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned == 0 {
		t.Fatal("scanned zero Go files — the walk is broken, and this guard can only pass")
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("these binaries define their own version():\n  %s\n\n"+
			"There is one implementation, internal/version, and it is the only one that knows "+
			"a container build has no .git to fall back on. A local copy reintroduces the exact "+
			"defect #143 closed: it reads correct on a developer's machine and reports a "+
			"plausible-looking constant from the shipped image. Call version.String().",
			strings.Join(offenders, "\n  "))
	}
}

// TestEveryGoImageStampsItsVersion is the half that would rot silently. An
// unstamped image does not fail to build, fail to start, or fail a test — it just
// tells everyone downstream that it does not know what it is.
func TestEveryGoImageStampsItsVersion(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))

	var unstamped []string
	goBuilders := 0

	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", ".gotmp", "gen":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasPrefix(d.Name(), "Dockerfile") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		body := string(b)
		// Only Go images are in scope — kanz-py builds no Go binary and has no
		// linker to pass a flag to.
		if !strings.Contains(body, "go build") {
			return nil
		}
		goBuilders++

		rel := filepath.ToSlash(mustRel(repoRoot, path))
		switch {
		case !strings.Contains(body, versionStampFlag):
			unstamped = append(unstamped, rel+": go build does not pass "+versionStampFlag)
		case !strings.Contains(body, "ARG VERSION"):
			// The flag without the ARG interpolates to nothing and the image is
			// unstamped anyway — a stamp that looks present and is not.
			unstamped = append(unstamped, rel+": passes the stamp flag but never declares ARG VERSION, "+
				"so ${VERSION} expands to empty")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", repoRoot, err)
	}

	// NON-VACUITY. A walk that finds no Go Dockerfiles passes no matter how many
	// images ship unstamped.
	if goBuilders == 0 {
		t.Fatalf("found zero Dockerfiles running `go build` under %s — the scanner is broken, "+
			"not the estate", repoRoot)
	}

	if len(unstamped) > 0 {
		sort.Strings(unstamped)
		t.Fatalf("these images ship a Go binary that cannot say which code it is:\n  %s\n\n"+
			"A container build has no .git (it is the first line of .dockerignore), so "+
			"debug.ReadBuildInfo finds no revision and internal/version falls back to "+
			"%q. That value is stamped on every span the image emits AND into envelope "+
			"field 14 of every FACT it publishes, which the schema marks REQUIRED and calls "+
			"the first thing needed to debug a bad event.\n\n"+
			"Add both, next to the build step:\n"+
			"  ARG VERSION=\n"+
			"  ... -ldflags=\"-s -w %s${VERSION}\"\n"+
			"and pass --build-arg VERSION=<tag-or-sha> from the workflow that builds it.",
			strings.Join(unstamped, "\n  "), "unstamped", versionStampFlag)
	}
}

// TestTheWorkflowsPassAVersion closes the last gap: a Dockerfile that declares
// ARG VERSION and a workflow that never sets it produce an unstamped image with
// every file looking correct in isolation.
func TestTheWorkflowsPassAVersion(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))
	dir := filepath.Join(repoRoot, ".github", "workflows")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	var missing []string
	buildersFound := 0

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		body := string(b)
		if !strings.Contains(body, "docker/build-push-action") {
			continue
		}
		buildersFound++
		if !strings.Contains(body, "VERSION=") {
			missing = append(missing, e.Name())
		}
	}

	if buildersFound == 0 {
		t.Fatal("no workflow uses docker/build-push-action — the scanner is broken, not the estate")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("these workflows build images without passing a version:\n  %s\n\n"+
			"The Dockerfiles declare ARG VERSION; a workflow that never sets it leaves "+
			"${VERSION} empty and the image reports \"unstamped\". Add:\n"+
			"  build-args: |\n"+
			"    VERSION=${{ github.ref_name }}   # a release tag, or github.sha otherwise",
			strings.Join(missing, "\n  "))
	}
}
