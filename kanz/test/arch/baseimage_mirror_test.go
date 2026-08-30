package arch

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// mirrorPrefix is where every base image this estate builds on must come from.
//
// It is a PREFIX, not a list of exact images, because the mirror is a namespace
// the estate controls: adding golang:1.27 to it later must not require editing
// this test. What the prefix pins is the REGISTRY and the ownership — that the
// image is one we mirrored and can serve, not one a third party can withdraw,
// rate-limit or fail to answer.
const mirrorPrefix = "ghcr.io/eighred/base/"

// EVERY DOCKERFILE FROM MUST RESOLVE INSIDE THE MIRROR.
//
// The estate builds 25 images and every one of them pulls its base from a
// registry it does not own. On 2026-07-27 that produced seven
// registry-1.docker.io timeouts, three of them on consecutive PRs each needing a
// manual re-run. A build system that cannot build without a third party
// answering is not a build system; it is a dependency with a queue in front of
// it.
//
// The mirror (#55) is the fix. This guard is what keeps it fixed: nothing
// otherwise stops the twenty-sixth Dockerfile reintroducing a Docker Hub FROM,
// and it would look exactly like the twenty-five that came before it.
//
// NOT THE SAME QUESTION AS baseimage_test.go, though both read FROM lines.
// That one asks whether every golang FROM agrees on the same VERSION — it
// exists because a Dependabot bump once moved 3 Dockerfiles and left 18 behind.
// This one asks where the image comes FROM at all. A repository can pass either
// while failing the other: 25 Dockerfiles pinned in perfect agreement to a
// Docker Hub tag satisfy that test and fail this one, which is exactly today's
// state. Keep them separate; merging them would force one invariant to be
// weakened to express the other.
//
// SCOPE IS THE WHOLE REPOSITORY, not just the Go module. #55's own acceptance
// grep is scoped to `kanz`, which misses kanz-py/Dockerfile — and that image
// (ghcr.io/eighred/inference) is built and deployed like any other, on
// python:3.12-slim from the same Docker Hub. Inheriting that blind spot would
// leave the guard asserting less than its title claims, so this walks from the
// repository root and the gap is recorded in the exemption below instead.
func TestEveryDockerfileBaseImageComesFromTheMirror(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))

	froms := dockerfileFroms(t, repoRoot)
	// NON-VACUITY. A walk that finds no Dockerfiles passes every assertion below
	// no matter how many Docker Hub bases exist — the scanner would be broken,
	// not the estate, and it would look green.
	if len(froms) == 0 {
		t.Fatalf("found zero Dockerfiles under %s — the scanner is broken, not the estate", repoRoot)
	}

	seenExempt := map[string]bool{}

	for _, f := range froms {
		if strings.HasPrefix(f.image, mirrorPrefix) {
			continue
		}
		if _, ok := baseImagesPendingMirror[f.image]; ok {
			seenExempt[f.image] = true
			continue
		}
		t.Errorf("%s:%d pulls its base image from outside the mirror: %s\n\n"+
			"Every base must come from %s — an image the estate owns and can serve. A FROM "+
			"outside it makes this build depend on a third party answering: Docker Hub timeouts "+
			"already cost seven failed runs and three manual re-runs in a single day. Mirror the "+
			"image and point this FROM at it, or add it to baseImagesPendingMirror with the issue "+
			"that will remove it.",
			f.file, f.line, f.image, mirrorPrefix)
	}

	// DEAD-ENTRY CHECK. An exemption naming an image no Dockerfile pulls any
	// more is stale — the mirror landed, or the image was dropped. Left in place
	// it protects nothing and reads as load-bearing, and the next Dockerfile
	// reintroducing that exact image would pass unnoticed. Same shape as
	// metricSurfacesPendingRepair and retryCertifiedConsumers.
	var dead []string
	for image := range baseImagesPendingMirror {
		if !seenExempt[image] {
			dead = append(dead, image)
		}
	}
	if len(dead) > 0 {
		sort.Strings(dead)
		t.Errorf("baseImagesPendingMirror exempts %d image(s) no Dockerfile pulls any more: %s\n\n"+
			"The mirror landed for these — delete the entry. A stale exemption re-opens the hole "+
			"it was documenting, and the next FROM naming that image would pass.",
			len(dead), strings.Join(dead, ", "))
	}

	// Say the remaining exposure out loud. These pass because they are recorded,
	// not because they are safe: every one of them is still a third-party pull
	// on the critical path of a build.
	if len(seenExempt) > 0 {
		var still []string
		for image := range seenExempt {
			still = append(still, image)
		}
		sort.Strings(still)
		t.Logf("BASE-IMAGE SPOF: %d base image(s) still pulled from outside the mirror: %s. "+
			"Every build depends on a registry this estate does not own. This test passing means "+
			"the exposure is RECORDED, not removed — mirror the image, point the FROM at it, and "+
			"delete the entry. This map was empty as of #161; anything here is a regression.",
			len(still), strings.Join(still, ", "))
	}
}

// baseImagesPendingMirror is a DEFAULT-DENY allow-list: any base image not under
// mirrorPrefix fails unless it is named here with the issue that will remove it.
//
// It is keyed on the EXACT image reference, tag included, and that is the point.
// Keying on the repository (`golang`) would let a version bump to an unmirrored
// tag pass silently, which is the same class of hole as exempting a whole
// registry.
//
// IT IS EMPTY, AND THAT IS THE POINT (#161). Every FROM in the repository now
// resolves under mirrorPrefix: 52 of them across 26 Dockerfiles. The three
// entries that used to live here — golang:1.26.5, gcr.io/distroless/static
// :nonroot and python:3.12-slim — are gone because the thing they documented is
// gone, not because anyone decided to stop tracking it.
//
// The dead-entry check above is what keeps this map honest in the other
// direction: an entry added here for an image no Dockerfile pulls fails the
// build, so an exemption cannot outlive its repair.
//
// TWO THINGS THIS MAP LEARNED THE HARD WAY, worth keeping written down:
//
//   - These entries used to name #55, and #55 was CLOSED with the rewrite
//     unshipped. Default-deny still held, but nothing would ever retire them,
//     and the dead-entry check cannot detect that — it fires when an exempt
//     image stops being PULLED, never when the issue meant to remove it stops
//     EXISTING. The exposure went on being "recorded" indefinitely, which reads
//     at a glance exactly like being managed. If an entry is ever added back,
//     name an OPEN issue and check it is still open.
//   - The mirrored name is not always the source name. distroless/static is
//     mirrored as distroless-static, so the cutover was not a find-and-replace
//     on the registry prefix — doing it that way would have produced a tag that
//     does not exist and failed 25 builds identically.
var baseImagesPendingMirror = map[string]string{}

// dockerfileFrom is one FROM instruction: where it is, and what it pulls.
type dockerfileFrom struct {
	file  string // repo-relative, forward slashes
	line  int
	image string
}

var fromRe = regexp.MustCompile(`^\s*FROM\s+(?:--\S+\s+)*(\S+)`)

// dockerfileFroms returns every FROM in every Dockerfile under root, excluding
// references to earlier build stages.
//
// A multi-stage `FROM build` names a stage declared by an earlier `AS build`, not
// an image, and flagging it would be a false positive that gets the guard
// weakened rather than the Dockerfile fixed. Stage names are collected per file
// as the file is read, so a FROM can only match a stage declared ABOVE it —
// which is also Docker's own rule.
func dockerfileFroms(t *testing.T, root string) []dockerfileFrom {
	t.Helper()

	var out []dockerfileFrom
	for _, df := range dockerfilePaths(t, root) {
		b, rerr := os.ReadFile(df.abs)
		if rerr != nil {
			t.Fatalf("read %s: %v", df.abs, rerr)
		}
		rel := df.rel

		stages := map[string]bool{}
		for i, line := range strings.Split(string(b), "\n") {
			m := fromRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			image := m[1]
			// Record the stage name BEFORE deciding, so `FROM x AS build`
			// registers build for the FROMs below it.
			if fields := strings.Fields(line); len(fields) >= 4 && strings.EqualFold(fields[2], "AS") {
				stages[fields[3]] = true
			}
			if stages[image] {
				continue // a stage reference, not an image
			}
			out = append(out, dockerfileFrom{
				file:  rel,
				line:  i + 1,
				image: image,
			})
		}
	}
	return out
}

// dockerfileRef is one Dockerfile the walk found: its absolute path, and its
// path relative to the walk root in forward slashes (the form every failure
// message and every exemption key uses).
type dockerfileRef struct {
	abs string
	rel string
}

// dockerfilePaths is THE walk for "every Dockerfile in this repository",
// shared by the FROM guard above and the pip-install guard in
// python_dependency_pinning_test.go. One walk, not two: the set of files a
// Dockerfile guard is allowed to be blind to is itself an invariant, and two
// copies of it drift the moment one of them learns about a directory the other
// does not.
//
// SKIP NESTED CHECKOUTS. A git worktree carries its own copy of every
// Dockerfile, so a recursive walk from the repo root reads OTHER checkouts'
// copies as if they were ours and reports failures against paths this branch
// cannot fix — agent worktrees under .claude/ already did exactly this to
// TestEveryWorkflowGoTestIsSerialised, which is why workflowFiles() skips them.
// The Dockerfile walk inherited none of that: it predates the pip-install
// guard, and it survived only because every worktree's FROM lines happened to
// agree. A guard that fires on whether a sibling worktree has been rebased is
// noise, and it would have been red locally and green in CI — the shape that
// teaches people to ignore a guard.
//
// Keyed on the presence of a .git entry (a worktree's is a FILE pointing at the
// parent, not a directory), so any nested checkout is excluded, not just
// today's tooling.
func dockerfilePaths(t *testing.T, root string) []dockerfileRef {
	t.Helper()

	var out []dockerfileRef
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// OTHER CHECKOUTS FIRST (#848). dockerfileFroms is called with the REPO
			// ROOT, so this walk can descend into .claude/worktrees/ — another
			// checkout of this same tree, whose Dockerfiles are not the estate's.
			// The shared set is the one every repo-root walker consults; "gen"
			// below is this guard's own speed hint on top of it.
			if skipWalkDir(d) {
				return filepath.SkipDir
			}
			switch d.Name() {
			case "gen":
				return filepath.SkipDir
			}
			if path != root {
				if _, serr := os.Stat(filepath.Join(path, ".git")); serr == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasPrefix(d.Name(), "Dockerfile") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		out = append(out, dockerfileRef{abs: path, rel: filepath.ToSlash(rel)})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out
}
