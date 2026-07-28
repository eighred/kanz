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
			"the exposure is RECORDED, not removed — see issue #55.",
			len(still), strings.Join(still, ", "))
	}
}

// baseImagesPendingMirror is a DEFAULT-DENY allow-list: any base image not under
// mirrorPrefix fails unless it is named here with the issue that will remove it.
//
// It is keyed on the EXACT image reference, tag included, and that is the point.
// Keying on the repository (`golang`) would let a version bump to an unmirrored
// tag pass silently, which is the same class of hole as exempting a whole
// registry. Two entries cover 50 FROMs across 25 Dockerfiles precisely because
// the estate is disciplined about using one base — so #55 retires two lines
// here, not fifty.
var baseImagesPendingMirror = map[string]string{
	"golang:1.26.5": "issue #55: the Docker Hub builder base, used by all 25 Go Dockerfiles. " +
		"THIS IS THE ONE THAT ALREADY COST US — seven registry-1.docker.io timeouts on " +
		"2026-07-27, three on consecutive PRs each needing a manual re-run. Mirrored to " +
		"ghcr.io/eighred/base/golang:1.26.5, this entry goes.",

	"gcr.io/distroless/static:nonroot": "issue #55: the runtime base for all 25 images. Not " +
		"Docker Hub, so it did not appear in the 2026-07-27 outage, but it is equally a registry " +
		"the estate does not own and cannot serve if Google withdraws or rate-limits it. #55's " +
		"acceptance grep counts it as non-mirrored for that reason.",

	"python:3.12-slim": "issue #55: kanz-py's base, for the ghcr.io/eighred/inference image. " +
		"OUTSIDE #55's STATED SCOPE — its acceptance grep is `grep -rE '^FROM ' kanz`, which " +
		"never reaches kanz-py/Dockerfile. Recorded here so widening the mirror to cover it is a " +
		"decision somebody takes rather than an omission nobody notices; the deployed inference " +
		"service has exactly the same Docker Hub exposure as the Go images did.",
}

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
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}

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
				file:  filepath.ToSlash(rel),
				line:  i + 1,
				image: image,
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
