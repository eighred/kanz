package arch

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A RELEASE MUST NOT LEAVE ONE OF OUR IMAGES ON A MUTABLE TAG (#771).
//
// mutableTagExempt's dead-exemption arm fires in ONE DIRECTION ONLY: when an
// exemption outlives its repair — the manifest is pinned and the line is still
// there. It cannot fire the other way. If a release ran and kanz-pin-digests
// silently skipped a manifest, the exemption covering it kept covering it
// forever, the guard stayed green, and nothing anywhere said the image was still
// mutable. A temporary exemption became permanent without anyone deciding it
// should — the failure shape this repository has been bitten by before (#610 for
// a citation, #759 for a premise).
//
// The tool already computed the signal: Result.Unpinned, "services referenced by
// a manifest but absent from the digest set". It printed it and exited 0. What
// #771 changed is that it now REFUSES, which puts the check at the moment the
// promise is kept or broken rather than inferring it afterwards from git history
// — no release tag to read, no join date to maintain, no shallow-clone caveat.
//
// This guard holds two things a unit test cannot: that the refusal is still
// wired, and that the calibration it rests on is still true.

const (
	pinToolRel  = "../../cmd/kanz-pin-digests/main.go"
	releaseYML  = "../../../.github/workflows/release.yml"
	infraRoot   = "../../infra"
	orgInImages = "eighred"
)

// TestTheReleaseRefusesAnUnpinnedService holds the refusal itself.
func TestTheReleaseRefusesAnUnpinnedService(t *testing.T) {
	src := readStripped(t, pinToolRel)
	body := funcBody(t, src, "func main()")

	// MATCHED AS THE WHOLE CONDITION, not as a substring of it. `strings.Contains`
	// on "len(res.Unpinned) > 0" is satisfied by "len(res.Unpinned) > 0 && false",
	// which disables the refusal in place while leaving every token this guard
	// looks for — a mutation survived exactly that way before this line was
	// written, and it is the same failure as a guard matching its own prose.
	if !strings.Contains(body, "if len(res.Unpinned) > 0 {") {
		t.Fatal("kanz-pin-digests's refusal is no longer an unconditional test of Result.Unpinned. " +
			"A guarded or narrowed condition — `&& false`, an extra flag, a length threshold — " +
			"restores the pre-#771 behaviour with every symbol this guard reads still present, " +
			"and the release goes green with an image left on a mutable tag.")
	}
	idx := strings.Index(body, "len(res.Unpinned) > 0")
	if idx < 0 {
		t.Fatal("kanz-pin-digests no longer acts on Result.Unpinned. That field is the ONLY signal " +
			"that a release left one of our images on a mutable tag, and reporting it while exiting " +
			"0 is exactly how a temporary exemption in mutableTagExempt becomes permanent: the " +
			"dead-exemption arm fires when an exemption outlives its repair, never when the repair " +
			"silently did not happen (#771).")
	}
	if !strings.Contains(body[idx:], "os.Exit(1)") {
		t.Fatal("kanz-pin-digests inspects Result.Unpinned and does not exit non-zero on it. A " +
			"release that publishes no digest for an image the estate deploys must FAIL rather than " +
			"print a line into a workflow log nobody reads afterwards (#771).")
	}
}

// TestEveryDeployedImageIsBuiltByTheRelease holds the calibration.
//
// The refusal needs no exemption list, and that is a MEASURED property rather
// than an assumption: every eighred image a manifest references through an
// image:/value: key is built by release.yml's matrix, so a service in Unpinned is
// always a defect. If a manifest ever references a service the release does not
// build, the refusal starts failing every release for a legitimate reason — and
// the right response is to add the service to the matrix, not to soften the
// refusal. This arm says so before that happens.
func TestEveryDeployedImageIsBuiltByTheRelease(t *testing.T) {
	built := releaseMatrixServices(t)
	if len(built) == 0 {
		t.Fatal("parsed NO services from release.yml's build matrix — the guard would pass vacuously")
	}
	referenced := referencedOwnImages(t)
	if len(referenced) == 0 {
		t.Fatal("found NO eighred image references in infra/ — the guard would pass vacuously")
	}

	var missing []string
	for svc := range referenced {
		if !built[svc] {
			missing = append(missing, svc)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("manifests deploy %d eighred image(s) that release.yml does not build:\n\n  %s\n\n"+
			"kanz-pin-digests refuses a release that publishes no digest for a referenced image "+
			"(#771), so each of these turns the NEXT release red. That refusal is correct — the "+
			"estate is deploying an image no release produced. Add the service to release.yml's "+
			"build matrix rather than relaxing the refusal, which would restore the silence #771 "+
			"closed.", len(missing), strings.Join(missing, "\n  "))
	}
}

// TestTheStandingExemptionIsUnreachableByTheRewriter.
//
// The one entry left in mutableTagExempt is infra/gitops/preview-applicationset.yaml,
// and it is legitimate for a reason the refusal must not disturb: kustomize
// carries its images as a plural `images:` LIST of bare quoted strings, which the
// rewriter's pattern cannot match, so per-PR preview tags never reach Unpinned.
// If that file ever gained an `image:` key, the refusal would start failing every
// release on an image that is ephemeral BY DESIGN — a per-PR build with no
// release digest to pin to.
func TestTheStandingExemptionIsUnreachableByTheRewriter(t *testing.T) {
	const preview = "infra/gitops/preview-applicationset.yaml"
	body, err := os.ReadFile("../../" + preview)
	if err != nil {
		t.Skipf("%s is gone; if the preview environment was removed, drop its mutableTagExempt entry too", preview)
	}
	if rewriterPattern().Match(body) {
		t.Fatalf("%s now carries an image:/value: reference that kanz-pin-digests can match. Its "+
			"images are per-PR builds (:pr-N) with no release digest to pin to, so the rewriter "+
			"will leave them in Result.Unpinned and the #771 refusal will fail every release. "+
			"Either keep them in the kustomize images: list form the pattern cannot see, or give "+
			"the refusal an explicit exemption and argue it here.", preview)
	}
}

// releaseMatrixServices reads the service names release.yml builds.
func releaseMatrixServices(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(releaseYML)
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*-\s+service:\s*([A-Za-z0-9._-]+)`).FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = true
	}
	return out
}

// referencedOwnImages returns every eighred service a manifest deploys through a
// key the rewriter can see.
func referencedOwnImages(t *testing.T) map[string]bool {
	t.Helper()
	pat := rewriterPattern()
	out := map[string]bool{}
	walkYAML(t, infraRoot, func(_, body string) {
		for _, m := range pat.FindAllStringSubmatch(body, -1) {
			out[m[3]] = true
		}
	})
	return out
}

// rewriterPattern mirrors cmd/kanz-pin-digests's own reference pattern.
//
// A COPY, AND THE COPY IS CHECKED. TestTheRewriterPatternHasNotDrifted below
// fails if the tool's pattern changes, because a guard measuring a DIFFERENT set
// of references from the tool it is calibrating would report a clean estate
// while the tool refused every release.
// rewriterKeyPattern is the KEY-MATCHING half of the tool's pattern, held as a
// const so the drift arm can assert the tool still contains this exact text.
//
// COMPARING THE TWO, NOT JUST CHECKING THE TOOL. An earlier version of the drift
// arm read the tool's source for a shape and never looked at this copy, so
// narrowing the copy — dropping `value:`, say — left the arm green while the
// calibration silently measured a smaller set of images than the refusal acts on.
// A mutation survived exactly that way. Deriving the regexp from the same const
// the arm compares makes the two impossible to separate.
const rewriterKeyPattern = `(?m)^(\s*(?:-\s+)?(?:image|value):\s*)`

func rewriterPattern() *regexp.Regexp {
	return regexp.MustCompile(rewriterKeyPattern + `([A-Za-z0-9.\-]+/` +
		regexp.QuoteMeta(orgInImages) + `/([A-Za-z0-9._-]+))(?::[^\s]+|@sha256:[0-9a-f]{64})`)
}

// TestTheRewriterPatternHasNotDrifted keeps the copy honest.
func TestTheRewriterPatternHasNotDrifted(t *testing.T) {
	src := readStripped(t, pinToolRel)
	// The tool must contain THIS FILE'S key pattern verbatim. Narrowing either
	// side then fails: narrow the tool and the substring is gone; narrow this
	// copy and the tool no longer contains it.
	if !strings.Contains(src, strings.TrimPrefix(rewriterKeyPattern, `(?m)^`)) {
		t.Fatalf("cmd/kanz-pin-digests's key pattern no longer matches this guard's copy "+
			"(guard: %s). The copy calibrates which images the #771 refusal can see; when the two "+
			"diverge the calibration measures a different set from the tool, and a clean report here "+
			"can sit beside a release that fails on every run (or the reverse). Update both in the "+
			"same change.", rewriterKeyPattern)
	}
	if !strings.Contains(src, `@sha256:[0-9a-f]{64}`) {
		t.Fatal("cmd/kanz-pin-digests's reference pattern has changed shape. This guard carries a " +
			"copy of it to calibrate which images the refusal can see; a copy that has drifted " +
			"measures a different set from the tool and would report a clean estate while every " +
			"release failed (#771). Update rewriterPattern here in the same change.")
	}
}
