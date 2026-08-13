package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// NO WORKFLOW MAY RUN A CONTAINER IMAGE AT `:latest` (#155).
//
// `latest` is not a version. It is a promise that a gate can change behaviour
// with no commit — the reasoning already written into kanz-ci.yml when
// golangci-lint was pinned in #141, and then applied to one workflow while two
// others were left.
//
// THE SECRET GATE IS THE CASE THAT MATTERS. security.yml's gitleaks job is the
// one job whose silent success is indistinguishable from a real pass. An upstream
// release that removes a subcommand turns it RED — recoverable and loud. One that
// changes default rules, allowlist semantics or exit codes turns it quietly
// PERMISSIVE, and the repository's own self-test does not cover that: planting a
// secret proves the scanner caught it AT THE TIME IT RAN, not that tomorrow's
// `latest` still honours the flags this repo passes.
//
// It was not hypothetical. gitleaks reorganised its CLI (`detect` → `git`) and k6
// crossed a MAJOR while both were floating. #155 pinned them —
// gitleaks:v8.30.1 and k6:2.1.0 — and its body asked for exactly this guard so
// the next one cannot arrive silently. Nothing was built, and the estate has been
// relying on nobody typing `:latest` again.
//
// COMMENTS ARE STRIPPED BEFORE MATCHING, and that is load-bearing rather than
// tidy. Every `:latest` in .github/workflows today sits in PROSE describing the
// defect that was fixed — release.yml explaining what used to happen, latency.yml
// recording why k6 is pinned. A guard that matched those would fail on the very
// comments that document the repair, and the obvious way to "fix" that is to
// delete the explanations. Exactly the trap #444's guard hit by matching prose.
//
// WHAT THIS CANNOT CHECK: that a pinned VERSION is a good one, or that a
// third-party tag has not been re-pointed at different bytes. Digest pinning is
// the answer to the second and is tracked separately — base-image-mirror.yml is
// the path, and repointing consumers must land after the mirror is populated.

var (
	// latestTagRe finds an image reference whose TAG is `latest`.
	//
	// THE COLON MUST BE ADJACENT, and that is the whole precision of this regex.
	// A bare `\blatest\b` matches `runs-on: ubuntu-latest` — the standard GitHub
	// runner label, present in every workflow in this repository — so the first
	// draft of this guard fired on all eleven and proved nothing. A guard that
	// fails everywhere gets deleted, not obeyed.
	latestTagRe = regexp.MustCompile(`(?i)[a-z0-9._/-]+:latest\b`)
	// imageKeyRe captures the value of an `image:` key — the services:/container:
	// form, where the reference is unambiguous.
	imageKeyRe = regexp.MustCompile(`(?m)^\s*image:\s*(\S+)\s*$`)
	// imageRefRe recognises a value that is actually an image reference rather
	// than a YAML anchor, an expression, or a job output name.
	imageRefRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9._/-]*[a-z0-9])?(:[\w.-]+)?(@sha256:[a-f0-9]{64})?$`)
)

// workflowLatestExempt maps "<workflow>: <reason>" for a file permitted to name a
// floating tag outside a comment, and what retires the entry.
//
// EMPTY, AND THAT IS THE POINT. There is no CI gate for which "whatever upstream
// published this morning" is the right answer. An entry here has to argue that a
// job's behaviour may change without a commit.
var workflowLatestExempt = map[string]string{}

func TestNoWorkflowRunsAFloatingImageTag(t *testing.T) {
	// The repo root, not the module root: workflows live a level above kanz/.
	// workflowFiles is the existing discovery — it already handles BOTH
	// .github/workflows trees (root and kanz-schemas) and skips nested worktree
	// checkouts, which a fresh glob here would have re-implemented wrongly.
	files := workflowFiles(t, filepath.Dir(moduleRoot(t)))
	// NON-VACUITY, the directory half: a moved workflow tree finds nothing and
	// this guard passes having read no CI definition at all.
	if len(files) == 0 {
		t.Fatal("found zero workflows — the scanner is broken, not the estate")
	}

	var offenders []string
	seenExempt := map[string]bool{}
	imagesSeen, untagged := 0, []string{}

	for _, p := range files {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		base := filepath.Base(p)
		body := stripYAMLComments(string(raw))

		if latestTagRe.MatchString(body) {
			if reason, ok := workflowLatestExempt[base]; ok {
				seenExempt[base] = true
				t.Logf("%s: exempt — %s", base, reason)
			} else {
				offenders = append(offenders, base+" (names a floating `latest` outside a comment)")
			}
		}

		// THE SECOND SHAPE: an `image:` with no tag at all. Docker resolves a
		// bare name to :latest, so omitting the tag is the same defect written
		// more quietly — and it does not contain the word this guard greps for.
		for _, m := range imageKeyRe.FindAllStringSubmatch(body, -1) {
			ref := m[1]
			// Skip YAML that is not an image: expressions, anchors, empty keys.
			if strings.Contains(ref, "${{") || !imageRefRe.MatchString(ref) {
				continue
			}
			imagesSeen++
			if !strings.Contains(ref, ":") && !strings.Contains(ref, "@sha256:") {
				untagged = append(untagged, base+": image "+ref+" has no tag (docker resolves it to :latest)")
			}
		}
	}

	// NON-VACUITY, the match half: these workflows definitely declare service
	// containers. Finding none means the key spelling changed and the untagged
	// arm is asserting nothing.
	if imagesSeen < 2 {
		t.Fatalf("found %d `image:` reference(s) across the workflows — expected at least 2 (kanz-ci's "+
			"postgres and redis services). The scan is broken and half this guard is inert",
			imagesSeen)
	}

	problems := append(offenders, untagged...)
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("%d workflow image reference(s) are not pinned: %v.\n"+
			"`latest` is not a version — it is a promise that a CI gate can change behaviour with "+
			"no commit. The secret scanner is the case that matters: an upstream release removing a "+
			"subcommand turns that job RED and loud, but one that changes default rules or exit "+
			"codes turns it quietly PERMISSIVE, and the planted-secret self-test only proves the "+
			"scanner worked at the moment it ran. Pin to an explicit version (gitleaks:v8.30.1 and "+
			"k6:2.1.0 are the precedent from #155), or add an argued entry to workflowLatestExempt.",
			len(problems), problems)
	}

	// DEAD-ENTRY ARM: an exemption for a workflow that no longer floats has
	// outlived its repair and would wave the next one through.
	for base, reason := range workflowLatestExempt {
		if !seenExempt[base] {
			t.Errorf("exemption for %q (%s) matches no workflow naming a floating tag — delete it",
				base, reason)
		}
	}
}
