package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
		// The majority version is presumed the intended target; everything
		// else is a straggler that needs bumping to match it.
		var versions []string
		for v := range versionToFiles {
			versions = append(versions, v)
		}
		sort.Slice(versions, func(i, j int) bool {
			return len(versionToFiles[versions[i]]) > len(versionToFiles[versions[j]])
		})
		majority := versions[0]

		var problems []string
		for _, v := range versions[1:] {
			files := append([]string(nil), versionToFiles[v]...)
			sort.Strings(files)
			problems = append(problems, "golang:"+v+" ("+strings.Join(files, ", ")+")")
		}
		sort.Strings(problems)

		t.Fatalf("Dockerfiles disagree on the golang base image — %d version(s) in use across %d files:\n\n"+
			"  majority: golang:%s (%d file(s))\n"+
			"  stragglers:\n    %s\n\n"+
			"Bump the straggler Dockerfile(s) above to golang:%s so every image builds from the same base. "+
			"This is exactly how SEC-M4 happened: a base-image bump moved 3 of 22 Dockerfiles and left the "+
			"rest behind, and nothing compared them to each other.",
			len(versionToFiles), dockerfileCount, majority, len(versionToFiles[majority]),
			strings.Join(problems, "\n    "), majority)
	}
}
