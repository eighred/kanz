package main

// A release that leaves one of our images on a mutable tag must FAIL (#771).
//
// Result.Unpinned was reported and not acted on, which is the hole this closes:
// the dead-exemption arm in test/arch/supplychain_test.go fires only when an
// exemption OUTLIVES its repair. It cannot fire the other way — a release that
// silently skipped a manifest left the exemption covering it forever, the guard
// green, and nothing anywhere saying the image was still on a mutable tag.
//
// These run the BUILT BINARY rather than calling Pin, because the defect is an
// exit code. Pin already returned everything needed to detect this; what was
// missing was main deciding to act on it, and main is exactly what a unit test
// of Pin cannot reach.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pinBinary builds the tool once for this package's tests.
func pinBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "kanz-pin-digests")
	if os.PathSeparator == '\\' {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// pinTree writes a manifest tree and a digest set, then runs the tool over them.
func pinTree(t *testing.T, manifests map[string]string, digests map[string]string) (string, error) {
	t.Helper()
	infra := t.TempDir()
	for name, body := range manifests {
		full := filepath.Join(infra, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dig := t.TempDir()
	for svc, d := range digests {
		if err := os.WriteFile(filepath.Join(dig, svc), []byte(d), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command(pinBinary(t), "-digests", dig, "-infra", infra).CombinedOutput()
	return string(out), err
}

const someDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// A SERVICE THE RELEASE DID NOT PUBLISH FAILS THE RUN.
//
// This is the whole issue: before #771 the run below printed "LEFT ON A MUTABLE
// TAG" and exited 0, so the release completed, the pin PR opened, and the image
// stayed on :main with an exemption still covering it.
func TestAServiceWithNoPublishedDigestRefusesTheRelease(t *testing.T) {
	out, err := pinTree(t,
		map[string]string{
			"a-deploy.yaml": "spec:\n  containers:\n  - image: ghcr.io/eighred/pinned:main\n",
			"b-deploy.yaml": "spec:\n  containers:\n  - image: ghcr.io/eighred/forgotten:main\n",
		},
		map[string]string{"pinned": someDigest}, // no digest for `forgotten`
	)
	if err == nil {
		t.Fatalf("the tool exited 0 with a service left on a mutable tag:\n%s\n\n"+
			"A release that publishes no digest for an image the estate deploys has either lost the "+
			"service from its build matrix or is pinning a manifest that names something that does "+
			"not exist. Exiting 0 is how a temporary exemption becomes permanent (#771).", out)
	}
	if !strings.Contains(out, "forgotten") {
		t.Errorf("the refusal does not name the service that was left behind:\n%s", out)
	}
	if !strings.Contains(out, "build matrix") {
		t.Errorf("the refusal does not name the two causes an operator has to choose between:\n%s", out)
	}
}

// A CLEAN RELEASE STILL SUCCEEDS. Without this the test above passes on a tool
// that refuses everything, which would turn every release red and get the check
// reverted rather than fixed.
func TestAReleaseThatPinsEverythingSucceeds(t *testing.T) {
	out, err := pinTree(t,
		map[string]string{
			"a-deploy.yaml": "spec:\n  containers:\n  - image: ghcr.io/eighred/pinned:main\n",
		},
		map[string]string{"pinned": someDigest},
	)
	if err != nil {
		t.Fatalf("a release that pinned every reference failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "pinned 1 image reference") {
		t.Errorf("the report does not say what was pinned:\n%s", out)
	}
}

// A THIRD-PARTY IMAGE IS NOT OUR PROBLEM. The pattern requires our own org, so a
// postgres or nats tag must not drag a release down — the refusal is about images
// this release was supposed to publish.
func TestAThirdPartyImageDoesNotRefuseTheRelease(t *testing.T) {
	out, err := pinTree(t,
		map[string]string{
			"a-deploy.yaml": "spec:\n  containers:\n" +
				"  - image: ghcr.io/eighred/pinned:main\n" +
				"  - image: docker.io/library/postgres:16-alpine\n",
		},
		map[string]string{"pinned": someDigest},
	)
	if err != nil {
		t.Fatalf("a third-party image refused the release: %v\n%s", err, out)
	}
}

// AN EMPTY DIGEST SET STILL FAILS FOR ITS OWN REASON, and the two refusals must
// stay distinguishable: "the release published nothing" sends an operator to the
// build job, "one service is missing" sends them to the matrix entry.
func TestAnEmptyDigestSetFailsWithItsOwnMessage(t *testing.T) {
	out, err := pinTree(t,
		map[string]string{"a-deploy.yaml": "spec:\n  containers:\n  - image: ghcr.io/eighred/pinned:main\n"},
		map[string]string{},
	)
	if err == nil {
		t.Fatalf("an empty digest set exited 0:\n%s", out)
	}
	if !strings.Contains(out, "no digests found") {
		t.Errorf("the empty-set refusal lost its own message and now reads as the #771 one:\n%s", out)
	}
}

// A RUN THAT REWROTE NOTHING FAILS FOR ITS OWN REASON.
//
// Distinct from both refusals above: the digest set is populated and every
// manifest is ALREADY pinned to those digests, so nothing changed. That is how a
// rewriter whose pattern stopped matching reports success — it walks the tree,
// matches nothing, and exits 0 having pinned nothing. The check predates #771 and
// had no test; it protects the same property from the other side, so it gets one
// here rather than being left as the only unexercised refusal in the tool.
func TestARunThatRewroteNothingFails(t *testing.T) {
	out, err := pinTree(t,
		// The digest set names `pinned`; no manifest references it, so the walk
		// matches nothing and rewrites nothing.
		map[string]string{"a-deploy.yaml": "spec:\n  containers:\n  - image: docker.io/library/postgres:16-alpine\n"},
		map[string]string{"pinned": someDigest},
	)
	if err == nil {
		t.Fatalf("a run that rewrote no reference exited 0:\n%s\n\nA rewriter whose pattern stops "+
			"matching walks the whole tree, changes nothing and reports success — which ships every "+
			"manifest on a mutable tag while the release looks clean.", out)
	}
}
