package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// SEC-M4: every Go Dockerfile must pin the SAME golang base image.
//
// The defect this guards against was never that the base image was old. It
// was that a Dependabot bump moved 3 of the 22 golang:* Dockerfiles to a newer
// patch and left the other 19 (including this repo's own) untouched — and
// nothing noticed, because nothing compared them to each other. Eighteen
// images sat on a version with 13 vulnerabilities the code actually reached,
// silently, because "did the bump run" and "did the bump run EVERYWHERE" were
// never the same question to any check in this repository.
//
// This test asks the second question. It does not care what the version IS —
// pinning a specific value here would mean editing this test on every
// legitimate bump, and a test that must be edited to let a change through
// gets edited without thought, which is exactly how 19 stragglers survive the
// next one too. The invariant is AGREEMENT: every FROM golang: line in the
// module must name the same version, or the build fails and says which files
// disagree with which.
var fromGolangLine = regexp.MustCompile(`(?m)^FROM\s+golang:(\S+)`)

var leadingDigits = regexp.MustCompile(`^\d+`)

// leadingInt returns the integer formed by the leading digits of a dotted
// version component ("5" -> 5, "5-alpine" -> 5, "alpine" -> 0). A component
// with no leading digits sorts as 0 rather than panicking or failing the
// test — an unparsed suffix should degrade the comparison, not crash the
// guard that exists to catch drift.
func leadingInt(component string) int {
	m := leadingDigits.FindString(component)
	if m == "" {
		return 0
	}
	n, err := strconv.Atoi(m)
	if err != nil {
		return 0
	}
	return n
}

// compareVersions orders two dotted version tags ("1.26.5", "1.26.10",
// "1.26.5-alpine") oldest to newest, comparing components numerically
// left-to-right with a missing trailing component treated as 0. Only the
// leading digits of each component participate, so "1.26.5-alpine" and
// "1.26.5-bookworm" compare equal — this guard cares about version drift
// between Dockerfiles, not packaging variant.
//
// A lexical string compare would rank "1.26.10" below "1.26.9" (since '1' <
// '9' byte-wise), which is exactly the class of bug this test exists to
// catch in the base images themselves — it must not exist in the test.
func compareVersions(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var av, bv int
		if i < len(as) {
			av = leadingInt(as[i])
		}
		if i < len(bs) {
			bv = leadingInt(bs[i])
		}
		if av != bv {
			return av - bv
		}
	}
	return 0
}

func TestAllDockerfilesPinTheSameGolangVersion(t *testing.T) {
	root := moduleRoot(t)

	versionToFiles := map[string][]string{}
	dockerfileCount := 0

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "Dockerfile" {
			return nil
		}
		dockerfileCount++

		b, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("read %s: %v", path, rerr)
		}
		// NORMALIZE CRLF before matching, same reason as deployability_test.go:
		// git on Windows (core.autocrlf=true) can hand Dockerfiles over with
		// CRLF line endings, and an anchored regex match against "\n" alone
		// would then silently see zero FROM lines in every file on a Windows
		// checkout — a false pass, not a real one.
		content := strings.ReplaceAll(string(b), "\r\n", "\n")

		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		for _, m := range fromGolangLine.FindAllStringSubmatch(content, -1) {
			version := m[1]
			versionToFiles[version] = append(versionToFiles[version], rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s for Dockerfiles: %v", root, err)
	}

	// NON-VACUOUS: zero Dockerfiles found means the walk broke (wrong root,
	// filesystem error), not that there is nothing to check. This module has
	// 22 Go Dockerfiles as of SEC-M4; a walk that finds none is a broken test,
	// not a passing one.
	if dockerfileCount == 0 {
		t.Fatal("found zero Dockerfiles under the module root — the walk is broken, not the fleet")
	}
	if len(versionToFiles) == 0 {
		t.Fatalf("found %d Dockerfile(s) but zero \"FROM golang:<version>\" lines — the format changed "+
			"(e.g. a different keyword, casing, or missing tag) and this guard no longer sees any base image",
			dockerfileCount)
	}

	if len(versionToFiles) > 1 {
		// The HIGHEST version is presumed the intended target, never the
		// most common one. A partial base-image bump always starts as a
		// minority on the newer version — that is what SEC-M4 was: 18 of 22
		// Dockerfiles sat on the old, vulnerable golang:1.26.1 and 4 had
		// already moved to the patched golang:1.26.5. Picking the majority
		// as "correct" would tell an engineer to bump those 4 fixed images
		// backward to the vulnerable one. Ranking by version — newest
		// first, ties broken on the version string — also makes the
		// ordering deterministic on an even split (e.g. 11/11), which a
		// count-based tie-break is not.
		var versions []string
		for v := range versionToFiles {
			versions = append(versions, v)
		}
		sort.Slice(versions, func(i, j int) bool {
			if c := compareVersions(versions[i], versions[j]); c != 0 {
				return c > 0
			}
			return versions[i] < versions[j]
		})
		newest := versions[0]

		var problems []string
		for _, v := range versions[1:] {
			files := append([]string(nil), versionToFiles[v]...)
			sort.Strings(files)
			problems = append(problems, "golang:"+v+" ("+strings.Join(files, ", ")+")")
		}
		sort.Strings(problems)

		t.Fatalf("Dockerfiles disagree on the golang base image — %d version(s) in use across %d files:\n\n"+
			"  newest (bump everything up to this): golang:%s (%d file(s))\n"+
			"  older, to bump up:\n    %s\n\n"+
			"Bump the older Dockerfile(s) above to golang:%s so every image builds from the same, most-current "+
			"base — never the other direction. This is exactly how SEC-M4 happened: a base-image bump moved 3 "+
			"of 22 Dockerfiles to a newer, patched version and left the other 19 (a majority) on an old one "+
			"with 13 reachable vulnerabilities, and nothing compared them to each other. The minority is "+
			"presumed the fix, not the outlier.",
			len(versionToFiles), dockerfileCount, newest, len(versionToFiles[newest]),
			strings.Join(problems, "\n    "), newest)
	}
}
