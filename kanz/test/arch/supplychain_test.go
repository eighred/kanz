package arch

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A PIN NOTHING ENFORCES IS NOT A PIN.
//
// release.yml grants packages: write and id-token: write — a compromised action
// running there can push an image AND mint a genuine cosign signature that
// verifies. Every action in every workflow was moved from a floating tag to an
// immutable commit SHA (SUPPLY-M1 task 1) for exactly that reason: the value of
// pinning is that MOVING an action becomes a reviewable commit, and that only
// holds if a tag-pinned action fails the build (KANZ_BRAIN.md: "latest is not a
// version; it is a promise that the gate can change without a commit. Pin every
// tool that can fail a build, and make moving it a reviewable change.").
//
// This test is the enforcement: every `uses:` reference across every workflow
// file must resolve to a 40-character commit SHA, or be named below with a
// reason. Without it, the pinning from task 1 is a snapshot that erodes the
// next time someone edits a workflow and reaches for a tag out of habit.
func TestWorkflowActionsArePinnedToSHA(t *testing.T) {
	root := moduleRoot(t)
	repoRoot := filepath.Dir(root)

	files := workflowFiles(t, repoRoot)
	if len(files) == 0 {
		t.Fatal("no workflow files found under .github/workflows — has the layout changed? " +
			"(this test would otherwise pass vacuously)")
	}

	usesRe := regexp.MustCompile(`(?m)^\s*(?:-\s*)?uses:\s*(\S+)(.*)$`)
	shaRe := regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	trailerRe := regexp.MustCompile(`#\s*\S+`)

	var problems []string
	seen := map[string]bool{} // action name -> referenced anywhere (for dead-exemption check)
	total := 0

	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		// Normalize CRLF: git on Windows (core.autocrlf=true) hands these files
		// over with CRLF, and a naive line-anchored regex would otherwise behave
		// differently on a Windows checkout than on CI (see deployability_test.go).
		body := strings.ReplaceAll(string(raw), "\r\n", "\n")
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		for _, m := range usesRe.FindAllStringSubmatch(body, -1) {
			actionRef := m[1]
			trailer := m[2]
			if strings.HasPrefix(actionRef, "./") {
				// A local composite action is in-repo, not a supply-chain edge —
				// there is nothing external to pin.
				continue
			}
			at := strings.LastIndex(actionRef, "@")
			if at <= 0 || at == len(actionRef)-1 {
				problems = append(problems, rel+": malformed uses reference "+actionRef+" (expected owner/repo@ref)")
				continue
			}
			total++
			name, ref := actionRef[:at], actionRef[at+1:]
			seen[name] = true

			_, exempt := pinExempt[name]
			isSHA := shaRe.MatchString(ref)

			if exempt {
				if isSHA {
					problems = append(problems, name+" ("+rel+"): now SHA-pinned ("+ref+") — remove it from "+
						"pinExempt, the exemption is stale")
				}
				continue
			}
			if !isSHA {
				problems = append(problems, rel+": "+name+"@"+ref+" is pinned to a tag/branch, not a commit SHA — "+
					"pin it to the commit SHA "+ref+" currently resolves to, keeping \""+ref+"\" as a trailing comment "+
					"(e.g. uses: "+name+"@<40-char-sha> # "+ref+")")
			} else if !trailerRe.MatchString(trailer) {
				problems = append(problems, rel+": "+name+"@"+ref+" is SHA-pinned but has no trailing \"# <ref>\" "+
					"comment — Dependabot reads that comment to know what tag/version the SHA corresponds to; a bare "+
					"SHA with no comment is invisible to it and will never receive a security bump (add e.g. \"# v4\" "+
					"after the SHA)")
			}
		}
	}

	if total == 0 {
		t.Fatal("zero `uses:` lines found across all workflow files — the regex or the workflow layout is broken " +
			"(this test would otherwise pass vacuously)")
	}

	// Anti-rot, direction two: an exemption for an action no longer referenced by
	// any workflow is dead weight — the next reader would believe it load-bearing.
	for name, reason := range pinExempt {
		if strings.TrimSpace(reason) == "" {
			problems = append(problems, name+": exempted in pinExempt with no reason")
			continue
		}
		if !seen[name] {
			problems = append(problems, name+": in pinExempt but not referenced by any workflow — dead exemption, remove it")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("unenforced supply-chain pins:\n\n  %s\n\n"+
			"Every `uses:` action across every workflow must resolve to a 40-character commit SHA, or be named in "+
			"pinExempt with a written reason. A tag-pinned action can be silently repointed by its maintainer — "+
			"moving a SHA-pinned action is a reviewable commit instead.", strings.Join(problems, "\n  "))
	}
}

// pinExempt lists actions deliberately held at a tag instead of a commit SHA,
// each with the reason. An action is here because somebody DECIDED it, not
// because somebody forgot to pin it.
var pinExempt = map[string]string{}

// workflowFiles walks the repo root for every *.yml / *.yaml under a
// .github/workflows directory — both the root .github/workflows and
// kanz-schemas/.github/workflows, and any future one, without hardcoding the
// path list. A ninth workflow file, or a workflows directory in a new
// sub-repo, is picked up automatically; only .git is skipped (irrelevant and
// large).
func workflowFiles(t *testing.T, repoRoot string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skipWalkDir(d) {
			return filepath.SkipDir
		}
		// SKIP NESTED CHECKOUTS. A git worktree carries its own .github/workflows,
		// so a recursive walk from the repo root reads OTHER checkouts' copies as if
		// they were ours — and reports failures against paths that are not in this
		// repository. Agent worktrees under .claude/ made TestEveryWorkflowGoTestIsSerialised
		// fail naming four invocations in two sibling checkouts, none of which this
		// branch can fix.
		//
		// It would have passed in CI, where no worktree exists — a guard that is red
		// locally and green in CI teaches people to ignore it, which is worse than not
		// having it. Keyed on the presence of a .git entry (a worktree's is a FILE
		// pointing at the parent, not a directory) rather than on ".claude", so any
		// nested checkout is excluded, not just today's tooling.
		if d.IsDir() && path != repoRoot {
			if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
				return filepath.SkipDir
			}
		}
		if d.IsDir() || filepath.Base(filepath.Dir(path)) != "workflows" {
			return nil
		}
		if filepath.Base(filepath.Dir(filepath.Dir(path))) != ".github" {
			return nil
		}
		if ext := filepath.Ext(path); ext == ".yml" || ext == ".yaml" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s for workflow files: %v", repoRoot, err)
	}
	sort.Strings(files)
	return files
}

// A PRIVATE REGISTRY WITH NO CREDENTIAL IS A NODE THAT CANNOT RUN ANYTHING.
//
// Every platform image is ghcr.io/eighred/*, and those packages are private
// (anonymous pulls 403). A node joined through the TUI holds none of those
// images, so a workload scheduled there dies in ErrImagePull. That was observed
// twice during OPS-M2e — a probe Job and the operator's own rollout — and was
// worked around by hand with ctr export/scp/ctr import across ten images.
//
// OPS-M2f-b attaches a `ghcr-pull` dockerconfigjson Secret at the ServiceAccount
// rather than the pod spec, so the two Go-built provisioning Jobs inherit it via
// ServiceAccountName with no code change. This test is the enforcement: remove
// the attachment from any ServiceAccount that owns a pod running a private image
// and the build fails, instead of the removal surfacing weeks later as an
// ErrImagePull nobody connects back to this decision.
//
// NOTE ON automountServiceAccountToken. kanz-node-provisioner sets it false
// (TestProvisionerServiceAccountHasNoToken). That governs the projected API
// token only; kubelet reads imagePullSecrets off the ServiceAccount regardless.
// The two are not in conflict.
const pullSecretName = "ghcr-pull"

// privateImagePrefix is the registry path that requires the credential. It is
// DERIVED from canonicalImageOrg rather than written out again: when the org
// changed under REL-P0a, a second hardcoded copy of it here went stale and this
// guard failed with "found 0 pod-bearing manifests" — correctly, because its
// floor caught the drift, but the two constants should never have been able to
// disagree in the first place.
const privateImagePrefix = "ghcr.io/" + canonicalImageOrg + "/"

// workloadFloor is a backstop, not the primary defense. The structural filter
// below (a pod template carrying a private image) is what selects workloads; if
// a manifest is reshaped so it stops matching, the count drops and this floor
// catches it. 25 is the number of pod-bearing manifests at the time of writing.
const workloadFloor = 25

type pullSecretDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	// ServiceAccount carries imagePullSecrets at the top level.
	ImagePullSecrets []struct {
		Name string `yaml:"name"`
	} `yaml:"imagePullSecrets"`
	// Deployment / Job / Rollout / StatefulSet / DaemonSet all nest the pod
	// template at spec.template.spec, so one shape reads all of them.
	Spec struct {
		Template struct {
			Spec struct {
				ServiceAccountName string `yaml:"serviceAccountName"`
				Containers         []struct {
					Image string `yaml:"image"`
				} `yaml:"containers"`
				InitContainers []struct {
					Image string `yaml:"image"`
				} `yaml:"initContainers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func TestPrivateImagesHavePullSecrets(t *testing.T) {
	root := moduleRoot(t)
	infra := filepath.Join(root, "infra")

	// key: namespace/name
	saHasPull := map[string]bool{}
	saSeen := map[string]bool{}
	type workload struct{ file, ns, sa, kind, name string }
	var workloads []workload
	var parseErrors []string

	err := filepath.WalkDir(infra, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || (!strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml")) {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		relSlash := filepath.ToSlash(rel)
		dec := yaml.NewDecoder(strings.NewReader(string(body)))
		for {
			var doc pullSecretDoc
			if err := dec.Decode(&doc); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				// FAIL LOUD, never skip. A decoder that silently stops on the first
				// unreadable document would stop reading the REST of that file too,
				// and this test would then pass by having looked at less than it
				// claims. If some file legitimately cannot decode into this shape,
				// add it to a named exemption with a stated reason — do not widen
				// this catch.
				parseErrors = append(parseErrors, fmt.Sprintf("%s: %v", relSlash, err))
				break
			}
			if doc.Kind == "ServiceAccount" {
				key := doc.Metadata.Namespace + "/" + doc.Metadata.Name
				saSeen[key] = true
				for _, s := range doc.ImagePullSecrets {
					if s.Name == pullSecretName {
						saHasPull[key] = true
					}
				}
				continue
			}
			ps := doc.Spec.Template.Spec
			private := false
			for _, c := range ps.Containers {
				if strings.Contains(c.Image, privateImagePrefix) {
					private = true
				}
			}
			for _, c := range ps.InitContainers {
				if strings.Contains(c.Image, privateImagePrefix) {
					private = true
				}
			}
			if !private {
				continue
			}
			workloads = append(workloads, workload{
				file: relSlash, ns: doc.Metadata.Namespace,
				sa: ps.ServiceAccountName, kind: doc.Kind, name: doc.Metadata.Name,
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking infra/: %v", err)
	}
	if len(parseErrors) > 0 {
		sort.Strings(parseErrors)
		t.Fatalf("manifests under infra/ failed to decode (%d) — this test cannot assert "+
			"anything about them:\n  %s", len(parseErrors), strings.Join(parseErrors, "\n  "))
	}

	if len(workloads) < workloadFloor {
		t.Fatalf("found %d pod-bearing manifests carrying a %s image, want at least %d — "+
			"the manifest shape changed and this test is now asserting almost nothing",
			len(workloads), privateImagePrefix, workloadFloor)
	}

	var problems []string
	for _, w := range workloads {
		if w.sa == "" {
			problems = append(problems, fmt.Sprintf(
				"%s: %s/%s runs a private image with NO serviceAccountName — it would use "+
					"the namespace default SA, which carries no pull secret", w.file, w.kind, w.name))
			continue
		}
		key := w.ns + "/" + w.sa
		if !saSeen[key] {
			problems = append(problems, fmt.Sprintf(
				"%s: %s/%s names ServiceAccount %q in namespace %q, but no such ServiceAccount "+
					"is declared anywhere under infra/", w.file, w.kind, w.name, w.sa, w.ns))
			continue
		}
		if !saHasPull[key] {
			problems = append(problems, fmt.Sprintf(
				"%s: %s/%s runs a private image under ServiceAccount %q (namespace %q), which does "+
					"not carry imagePullSecrets: [{name: %s}] — this pod cannot pull on any node that "+
					"has not pre-loaded the image by hand", w.file, w.kind, w.name, w.sa, w.ns, pullSecretName))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("private images without a pull credential (%d):\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// CI PUBLISHED TO ONE NAMESPACE AND THE CLUSTER PULLED FROM ANOTHER.
//
// Every manifest, preview.yml, and the sigstore ClusterImagePolicy (both its
// `images[].glob` and its keyless `subjectRegExp`) name ghcr.io/eighred. The
// two steps that actually PUSH images computed their target from
// ${{ github.repository_owner }}, which for this repository resolves to
// "eighred". So a green release.yml would have published images the estate
// could never pull, under an identity the admission policy would never
// accept — and nothing would have said so until someone tried.
//
// The owner's decision (REL-P0a, 2026-07-27) is that eighred is canonical — see
// canonicalImageOrg for why eighred was never available. This test is
// what keeps the two halves from drifting apart again: publish targets, pull
// references, and the signing identity are the same string or the build fails.
//
// WHY IT READS POSITIONS, NOT TEXT. A comment at infra/deploy/operator-deploy.yaml
// quotes the error string "403 Forbidden from ghcr.io/token". A guard that grepped
// free text would flag that comment as an image reference under an org called
// "token". So this walks YAML keys that actually carry image references.
//
// FOUR INDEPENDENT CHECKS, FOUR INDEPENDENT FLOORS. Workflow-side image
// references, infra-side image references, the ClusterImagePolicy's `glob`,
// and the signing identity (`subjectRegExp`) are four unrelated populations —
// an `image: ghcr.io/org/svc` field, a `glob: "ghcr.io/org/**"` list entry,
// and a `subjectRegExp: "^https://github.com/org/repo/..."` string share no
// key name and no value format, and the workflow and infra populations, while
// sharing the same `imageRefKeys` keys, live in files with utterly different
// counts. Counting any of these into one pooled total lets a coverage
// regression in the smallest population hide behind padding from the
// largest: infra `image:` references matched by imageRefKeys clear 40
// comfortably (53 ghcr matches — 39 eighred + 14 spiffe — across 45 files at
// time of writing), while the three workflow-side publish targets (build.yml's
// `images`, preview.yml's `tags`, release.yml's `IMAGE`) total exactly 3, and
// `glob`/`subjectRegExp` each have exactly ONE occurrence in this repo. Pool
// any of the small ones into the 53-strong infra count and it can vanish
// entirely — three publish targets rewritten as `${{ env.REGISTRY }}/...`
// (no literal `ghcr.io` left to match) still leaves the pooled total at 50,
// comfortably above 40 — while the guard keeps reporting success. Each
// population gets its own non-vacuity floor instead, so losing any one of
// them fails loudly rather than hiding in the noise of the others.
//
// BLOCK SCALARS. docker/metadata-action documents a multi-line form,
// `images: |` followed by indented ghcr.io lines. Both current workflows use
// the single-line form (`images: ghcr.io/...`), but a future one could use the
// block form — so image-reference values are scanned line-by-line and, when a
// key's inline value is a bare block-scalar indicator (`|`, `|-`, `>`, etc.),
// the following more-indented lines are pulled in as that key's value too.
// canonicalImageOrg is "eighred" because that is where this repository actually
// lives, and it is the only namespace GITHUB_TOKEN can publish into.
//
// It was "eighred" until 2026-07-27, which broke every push to main: all 25
// images failed at the push step because GITHUB_TOKEN is scoped to the repo's
// owner and cannot write another org's packages. The investigation found that
// **eighred does not exist on GitHub at all** — so the value named a namespace
// nothing could ever publish to, while ghcr.io/eighred/* already held working
// images. Owner decision (REL-P0a): adopt eighred as canonical.
//
// This is deliberately a LITERAL and not ${{ github.repository_owner }}. A
// computed owner is what let the publish side and the pull side disagree
// silently in the first place; a literal means that if the repository ever does
// move to a eighred org, this guard fails loudly and forces the manifests, the
// admission policy and the workflows to be updated as one reviewed change.
const canonicalImageOrg = "eighred"

// thirdPartyImageOrgs are ghcr namespaces we legitimately consume but do not
// own. Each is a set entry (not a map value) because the reason is documentary
// for a future reader of this source, not something the test logic branches
// on — it lives in the comment beside the name, not in a discarded variable.
//
//   - spiffe:   SPIRE server/agent, the CSI driver and spiffe-helper — upstream
//     sigstore/spiffe images.
//   - gitleaks: the secret-scanning image security.yml runs; not built here.
//   - cloudnative-pg: the PostgreSQL operator and operand images pinned by the
//     Tokyo testnet release lock and data-plane architecture guard.
var thirdPartyImageOrgs = map[string]struct{}{
	"spiffe":         {},
	"gitleaks":       {},
	"cloudnative-pg": {},
}

// imageRefKeys are the YAML keys whose values carry an image reference.
// `IMAGE` is release.yml's job-level env var; `images`/`tags` are
// docker/metadata-action and build-push-action inputs; `image` is Kubernetes.
// `glob` — the sigstore ClusterImagePolicy's `images[].glob` image-match
// pattern — deliberately is NOT in this list: it is checked and floored
// separately below (see globLineRe / globChecked / globFloor), because it is
// the one occurrence in the whole repo and would otherwise hide, uncounted
// for coverage purposes, inside a pool the other keys fill on their own.
var imageRefKeys = []string{"image", "images", "tags", "IMAGE"}

// workflowImageRefFloor, infraImageRefFloor, globFloor and subjectRegExpFloor
// are non-vacuity backstops for the four independent checks
// TestImageOrgIsCanonical runs (see the test's doc comment for why they are
// not pooled into one count). If any check's real match count ever drops
// below its floor, the key set, file layout, or value shape changed and that
// check is silently asserting almost nothing.
//
// workflowImageRefFloor is 3, not higher, because build.yml's `images`,
// preview.yml's `tags` and release.yml's `IMAGE` are the only three publish
// targets that exist today — the point of this floor is that losing any of
// them (e.g. a DRY refactor to `images: ${{ env.REGISTRY }}/...`, which
// contains no literal `ghcr.io` for the regex to match) must fail loudly, not
// that more than 3 are expected. It is deliberately its own floor rather than
// folded into infraImageRefFloor: infra alone clears 40 on its own, so a
// pooled counter would absorb the workflow side dropping to zero without
// moving enough to trip a floor of 40.
//
// globFloor and subjectRegExpFloor are 1, not higher, because each field has
// exactly one legitimate occurrence in this repo today — the point of a floor
// of 1 is that zero must fail loudly, not that many are expected.
const workflowImageRefFloor = 3
const infraImageRefFloor = 40
const globFloor = 1
const subjectRegExpFloor = 1

// blockScalarRe matches a bare YAML block-scalar indicator as a key's entire
// inline value: `|`, `|-`, `|+`, `|4`, `>`, `>-`, etc., optionally followed by
// a trailing comment. When a key's inline value is only this, the actual
// content lives on the following more-indented lines.
var blockScalarRe = regexp.MustCompile(`^[|>][+\-]?\d*\s*(#.*)?$`)

func TestImageOrgIsCanonical(t *testing.T) {
	root := moduleRoot(t)
	repoRoot := filepath.Dir(root)

	// keyLineRe matches one line: an optional YAML list-item dash, then one of
	// imageRefKeys, then its inline value (which may be empty or a block-scalar
	// indicator — see blockScalarRe).
	keyLineRe := regexp.MustCompile(`^(\s*)(?:-\s*)?(` + strings.Join(imageRefKeys, "|") + `)\s*:\s*(.*)$`)
	ghcrRe := regexp.MustCompile(`ghcr\.io/([A-Za-z0-9._\-]+|\$\{\{[^}]*\}\})/`)

	// globLineRe matches the ClusterImagePolicy's `images[].glob` image-match
	// pattern in isolation from imageRefKeys — see globFloor for why it has its
	// own counter instead of sharing imageRefsChecked.
	globLineRe := regexp.MustCompile(`^(\s*)(?:-\s*)?(glob)\s*:\s*(.*)$`)

	// subjectLineRe matches the ClusterImagePolicy's keyless signing identity.
	// Its value is a regex-as-a-string carrying a
	// "https://github.com/<org>/<repo>/..." subject, not a ghcr reference, so it
	// needs its own key pattern and its own org extractor.
	subjectLineRe := regexp.MustCompile(`^\s*subjectRegExp\s*:\s*(.*)$`)
	subjectOrgRe := regexp.MustCompile(`https://github\.com/([A-Za-z0-9._\-]+)/`)

	// expandBlockValue returns a key's full value text: the inline value as-is,
	// or — when the inline value is only a block-scalar indicator (`|`, `>`,
	// etc.) — the following more-indented lines joined, since the real content
	// lives there instead. Shared by the imageRefKeys and glob line handlers so
	// both benefit from the same BLOCK SCALARS handling described in this
	// test's doc comment.
	expandBlockValue := func(lines []string, i, indent int, value string) string {
		if !blockScalarRe.MatchString(value) {
			return value
		}
		var block []string
		for j := i + 1; j < len(lines); j++ {
			next := lines[j]
			if strings.TrimSpace(next) == "" {
				block = append(block, next)
				continue
			}
			nextIndent := len(next) - len(strings.TrimLeft(next, " "))
			if nextIndent <= indent {
				break
			}
			block = append(block, next)
		}
		return strings.Join(block, "\n")
	}

	var files []string
	files = append(files, workflowFiles(t, repoRoot)...)
	err := filepath.WalkDir(filepath.Join(root, "infra"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking infra/: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no workflow or infra files found — has the layout changed? " +
			"(this test would otherwise pass vacuously)")
	}

	// checkOrg is shared by all three checks. isSigningIdentity distinguishes
	// subjectRegExp from the image-reference fields: that field carries a
	// GitHub Actions OIDC signing identity ("who is trusted to sign"), not a
	// container image reference, and a message that called it an image would
	// misdirect whoever has to act on it.
	checkOrg := func(problems *[]string, relSlash, fieldDesc, org string, isSigningIdentity bool) {
		if org == canonicalImageOrg {
			return
		}
		if _, ok := thirdPartyImageOrgs[org]; ok {
			return
		}
		if strings.HasPrefix(org, "${{") {
			if isSigningIdentity {
				*problems = append(*problems, fmt.Sprintf(
					"%s: %s: names a COMPUTED signing-identity org %s — the OIDC subject this "+
						"authorizes would resolve to the GitHub repository owner at sign time, not to "+
						"%q, so a genuine signature from this repo would still fail verification against %q",
					relSlash, fieldDesc, org, canonicalImageOrg, canonicalImageOrg))
				return
			}
			*problems = append(*problems, fmt.Sprintf(
				"%s: %s: names a COMPUTED image org %s — it resolves to the GitHub "+
					"repository owner, not to %q, so images published here cannot be pulled "+
					"by manifests that name %q", relSlash, fieldDesc, org, canonicalImageOrg, canonicalImageOrg))
			return
		}
		if isSigningIdentity {
			*problems = append(*problems, fmt.Sprintf(
				"%s: %s: signing-identity org %q is neither the canonical %q nor a listed third "+
					"party — this field is the GitHub Actions OIDC subject the ClusterImagePolicy trusts "+
					"to sign, not a container image reference, so a mismatch here means a genuine "+
					"signature from the real release workflow would fail verification",
				relSlash, fieldDesc, org, canonicalImageOrg))
			return
		}
		*problems = append(*problems, fmt.Sprintf(
			"%s: %s: image org %q is neither the canonical %q nor a listed third party",
			relSlash, fieldDesc, org, canonicalImageOrg))
	}

	var problems []string
	workflowImageRefsChecked := 0
	infraImageRefsChecked := 0
	globChecked := 0
	subjectRegExpsChecked := 0

	for _, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		rel, _ := filepath.Rel(repoRoot, path)
		relSlash := filepath.ToSlash(rel)
		// The workflow and infra populations get independent counters and
		// floors (see workflowImageRefFloor's comment) — this is what tells
		// the two apart as the scan walks both sets of files in one pass.
		isWorkflowFile := strings.Contains(relSlash, ".github/workflows/")

		// Normalize CRLF for the same reason TestWorkflowActionsArePinnedToSHA
		// does: a line-anchored scan must behave the same on a Windows checkout
		// (core.autocrlf=true) as on CI.
		lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")

		for i := 0; i < len(lines); i++ {
			line := lines[i]

			if m := subjectLineRe.FindStringSubmatch(line); m != nil {
				for _, g := range subjectOrgRe.FindAllStringSubmatch(m[1], -1) {
					subjectRegExpsChecked++
					checkOrg(&problems, relSlash, "subjectRegExp", g[1], true)
				}
				continue
			}

			if m := globLineRe.FindStringSubmatch(line); m != nil {
				indent, value := len(m[1]), strings.TrimSpace(m[3])
				valueText := expandBlockValue(lines, i, indent, value)
				for _, g := range ghcrRe.FindAllStringSubmatch(valueText, -1) {
					globChecked++
					checkOrg(&problems, relSlash, "glob", g[1], false)
				}
				continue
			}

			m := keyLineRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			indent, key, value := len(m[1]), m[2], strings.TrimSpace(m[3])
			valueText := expandBlockValue(lines, i, indent, value)

			for _, g := range ghcrRe.FindAllStringSubmatch(valueText, -1) {
				if isWorkflowFile {
					workflowImageRefsChecked++
				} else {
					infraImageRefsChecked++
				}
				checkOrg(&problems, relSlash, key, g[1], false)
			}
		}
	}

	if workflowImageRefsChecked < workflowImageRefFloor {
		t.Fatalf("only %d ghcr.io image references matched across workflow files (build.yml's "+
			"images:, preview.yml's tags:, release.yml's IMAGE:) — expected at least %d; a "+
			"publish target was removed, or rewritten to a form (e.g. `images: ${{ env.REGISTRY "+
			"}}/...`) with no literal ghcr.io for this regex to match — either way this check is "+
			"now asserting almost nothing about publish targets, even though infra/ coverage "+
			"alone would still look healthy", workflowImageRefsChecked, workflowImageRefFloor)
	}
	if infraImageRefsChecked < infraImageRefFloor {
		t.Fatalf("only %d ghcr.io image references matched under kanz/infra/ — "+
			"expected at least %d; the key set or layout changed and this test is now "+
			"asserting almost nothing about infra image references", infraImageRefsChecked, infraImageRefFloor)
	}
	if globChecked < globFloor {
		t.Fatalf("only %d ClusterImagePolicy glob field(s) matched under infra/ — expected at "+
			"least %d; the policy was deleted, `glob` was renamed, or the value shape changed, "+
			"and this test is now asserting nothing about the image-match pattern",
			globChecked, globFloor)
	}
	if subjectRegExpsChecked < subjectRegExpFloor {
		t.Fatalf("only %d subjectRegExp signing-identity field(s) matched under infra/ — "+
			"expected at least %d; the ClusterImagePolicy moved, was renamed, or the pattern "+
			"broke, and this test is now asserting nothing about the signing identity",
			subjectRegExpsChecked, subjectRegExpFloor)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("image org disagreement (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
}

// A RELEASE THAT SHIPS A SUBSET IS WORSE THAN NO RELEASE.
//
// release.yml built 2 services while build.yml built 25. Nothing said so: the
// release workflow was green, the tag looked cut, and 23 services simply had no
// signed image, no SBOM and no vuln attestation. The gap is invisible until a
// cluster pulls one of them and the admission policy — which requires a
// signature from release.yml — refuses to run it.
//
// This test holds the two matrices identical. It compares (service, dockerfile)
// PAIRS, not just names, because the Dockerfile path is where the old
// release.yml was actually wrong: it hardcoded kanz/services/<service>/Dockerfile,
// which is false for inference (kanz-py/) and for kanz-migrate, kanz-halt and
// kanz-provisioner (kanz/cmd/). A name-only check would have called that correct.
func TestReleaseMatrixCoversEveryBuiltService(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))

	type matrixEntry struct {
		Service    string `yaml:"service"`
		Dockerfile string `yaml:"dockerfile"`
	}
	type workflow struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Include []matrixEntry `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}

	load := func(file, job string) map[string]string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		var wf workflow
		if err := yaml.Unmarshal(body, &wf); err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		entries := wf.Jobs[job].Strategy.Matrix.Include
		if len(entries) == 0 {
			t.Fatalf("%s: job %q has an empty matrix.include — the workflow shape changed and "+
				"this test would otherwise pass by comparing nothing", file, job)
		}
		out := map[string]string{}
		for _, e := range entries {
			if e.Service == "" || e.Dockerfile == "" {
				t.Fatalf("%s: matrix entry with service=%q dockerfile=%q — both are required",
					file, e.Service, e.Dockerfile)
			}
			out[e.Service] = e.Dockerfile
		}
		return out
	}

	built := load("build.yml", "image")
	released := load("release.yml", "release")

	var problems []string
	for svc, dockerfile := range built {
		relDockerfile, ok := released[svc]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"%s is built by build.yml but NOT released by release.yml — it would ship with no "+
					"signature, no SBOM and no vuln attestation, and admission would refuse it", svc))
			continue
		}
		if relDockerfile != dockerfile {
			problems = append(problems, fmt.Sprintf(
				"%s: build.yml uses %q, release.yml uses %q — the release would build a different "+
					"artifact than CI verified", svc, dockerfile, relDockerfile))
		}
	}
	for svc := range released {
		if _, ok := built[svc]; !ok {
			problems = append(problems, fmt.Sprintf(
				"%s is released by release.yml but not built by build.yml — it is released without "+
					"ever having been built on a pull request", svc))
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("release and build matrices disagree (%d):\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// mutableTagExempt records an image reference in infra/ that is deliberately
// NOT digest-pinned, with a written reason. Every entry must be retired; the
// dead-exemption arm below fails the build when one outlives its reason.
var mutableTagExempt = map[string]string{
	"infra/gitops/preview-applicationset.yaml": "per-PR preview environments build a fresh :pr-N image per pull " +
		"request; there is no release digest to pin to and the environment is ephemeral",

	// RETIRED 2026-07-27 by the mechanism that was supposed to retire it.
	// infra/dr/nats/rebuild-job.yaml carried a temporary entry reading "no
	// published digest exists until the next release. Retire on the first
	// pin-digests run". v0.2.0's pin-digests run rewrote it to
	// @sha256:70d5351a…, and BOTH anti-rot arms then failed kanz-ci on this
	// branch — "every image here is digest-pinned but the file is still in
	// mutableTagExempt" and "in mutableTagExempt but has no mutable tag".
	// The exemption could not be forgotten because the guard would not let the
	// pin PR merge with it present. That is the whole design, observed working
	// rather than asserted, so the evidence is recorded here and not only in a
	// commit message.
}

// productionImageRef matches an image reference to our own registry, by the
// SHAPE of the reference rather than by the YAML key that carries it.
//
// Keying on `image:` was the first attempt and it was WRONG THREE WAYS, all
// found by running it:
//   - kustomize carries a plural `images:` LIST of bare quoted strings
//     (infra/gitops/preview-applicationset.yaml), so both preview refs
//     were invisible — and the exemption declared for that file was therefore
//     permanently dead, which failed the build on a clean tree;
//   - infra/deploy/operator-deploy.yaml passes the provisioner image as an
//     env var `value:`, not an `image:`. That is the image the operator injects
//     into every provisioning Job spec it builds, so a mutable tag there ships
//     an unpinned provisioner to every TUI-provisioned node — precisely the
//     supply chain this guard exists to protect, and it was outside its view;
//   - it silently defined "production manifest" as "whatever uses the key I
//     thought of", which is how a guard ends up asserting less than it claims.
//
// A reference must carry a tag or a digest, which is what distinguishes it from
// the sigstore GLOB at infra/security/admission/cluster-image-policy.yaml
// (`ghcr.io/eighred/**`) — a policy pattern, not an image, and not pinnable.
//
// TAG-PLUS-DIGEST. The digest branch used to require `@sha256:` to immediately
// follow the name, so a fully-pinned `name:tag@sha256:digest` fell through to
// the tag branch, matched only as far as `:tag`, and was reported as a mutable
// tag — a false failure on a reference that was already fully pinned, printing
// the truncated `name:tag` while doing it. The tag branch now accepts an
// optional trailing `@sha256:...` of its own, so `:tag`, `@sha256:...`, and
// `:tag@sha256:...` all match, while a bare name with neither — e.g. the
// glob's `ghcr.io/eighred/**` — still does not: at least one of tag or digest
// is required by the alternation's construction, not by a separate check.
var productionImageRef = regexp.MustCompile(
	`ghcr\.io/eighred/[a-z0-9][a-z0-9._-]*(?:@sha256:[a-f0-9]{64}|:[A-Za-z0-9._{}-]+(?:@sha256:[a-f0-9]{64})?)`)

// coverageGapExempt names files that legitimately contain the literal
// "ghcr.io/eighred/" without carrying any pinnable productionImageRef match —
// checked by the per-file coverage arm in TestProductionManifestsPinImagesByDigest.
// This is deliberately a SEPARATE list from mutableTagExempt, not an entry in
// it: mutableTagExempt excuses a real, matched, mutable reference from
// failing the build; this one excuses a file from the "the prefix is here but
// nothing parsed" alarm because the file genuinely has nothing pinnable in it
// — a policy glob, not an image.
var coverageGapExempt = map[string]string{
	"infra/overlays/testnet-tokyo/kustomization.yaml": "Kustomize image transforms carry each " +
		"canonical ghcr.io/eighred source name separately from its mandatory ECR digest; " +
		"TestTokyoTestnetOverlayLocksEveryCapitalPathImageToECR parses and cross-checks that structure (#1098)",
	"infra/security/admission/cluster-image-policy.yaml": "carries the sigstore glob `ghcr.io/eighred/**`, " +
		"which has neither tag nor digest and is not a pinnable image reference",
}

// yamlComment strips trailing `#` comments so the guard scans CONFIGURATION,
// not prose. Without this, matching on reference shape would trip on a comment
// mentioning an example tag. `d34bb7d` fixed this same class of defect in the
// NATS posture guards, where needles matched the very comments that documented
// them.
//
// YAML only opens a comment at the start of a line or after whitespace — a
// `#` glued to a preceding character (`a#b`) is literal scalar content, not a
// comment marker. The old `#.*$` stripped from the FIRST `#` on the line
// regardless of what preceded it, which silently deleted a reference living
// after a mid-token `#` instead of scanning it — the opposite of the failure
// direction this guard exists to prevent, so getting this wrong is worse than
// under-stripping. `(^|\s)#.*$` requires whitespace or line-start immediately
// before the `#` before treating anything as a comment, which also means a
// URL fragment sharing a line with a reference survives intact.
var yamlComment = regexp.MustCompile(`(?m)(^|\s)#.*$`)

// TestProductionManifestsPinImagesByDigest is OPS-M4a's other half.
//
// release.yml already pins everything AFTER the build to the immutable digest —
// trivy, cosign and the SBOM all attest one specific image — and pin-digests
// rewrites the manifests to match. But nothing stopped a manifest drifting back
// to a tag, and one already had: the DR Job ran ghcr.io/eighred/nats-rebuild:latest.
//
// A mutable tag breaks the supply chain in two distinct ways, and both are live:
// :latest defaults imagePullPolicy to Always, so replicas rescheduled at
// different moments can run DIFFERENT CODE under one Deployment; and there is no
// previous digest to roll back TO, which is the primitive the TUI's rollback is
// supposed to wrap.
func TestProductionManifestsPinImagesByDigest(t *testing.T) {
	root := moduleRoot(t)
	infra := filepath.Join(root, "infra")

	var problems []string
	seenFiles := map[string]bool{}
	refCount := 0
	// walkedFiles and gapFiles feed the coverageGapExempt anti-rot loop below:
	// every infra/ YAML file visited by the walk, and the subset of those
	// whose "ghcr.io/eighred/" prefix count and productionImageRef match count
	// disagree (see the reference-level coverage floor below).
	walkedFiles := map[string]bool{}
	gapFiles := map[string]bool{}

	err := filepath.WalkDir(infra, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || (!strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml")) {
			return nil
		}
		body, rErr := os.ReadFile(path)
		if rErr != nil {
			return rErr
		}
		rel, _ := filepath.Rel(root, path)
		relSlash := filepath.ToSlash(rel)
		walkedFiles[relSlash] = true

		scanned := yamlComment.ReplaceAllString(string(body), "")
		matches := productionImageRef.FindAllString(scanned, -1)
		refCount += len(matches)
		prefixCount := strings.Count(scanned, "ghcr.io/eighred/")
		if prefixCount != len(matches) {
			gapFiles[relSlash] = true
		}

		// Accumulate per file before judging the exemption, then report stale
		// only once the file's scan is complete. The old code reported "every
		// image here is digest-pinned but the file is still in
		// mutableTagExempt" (stale exemption) the instant it saw ONE pinned
		// reference in an exempt file, without checking whether that same
		// file also still had a mutable one left — and once per pinned
		// reference, not once per file. A file mixing a pinned sidecar with a
		// genuinely-unpinned preview tag would deadlock: the stale arm says
		// remove the exemption, the mutable-tag arm (on the same file) says
		// add it back, and editing the test is the only way out. Report
		// stale only when the file has at least one pinned reference AND no
		// mutable reference left — the dead-exemption arm below already
		// checks its own condition this same accumulate-then-judge way.
		fileHasPinned := false
		fileHasMutable := false
		for _, ref := range matches {
			if strings.Contains(ref, "@sha256:") {
				fileHasPinned = true
				continue
			}
			fileHasMutable = true
			seenFiles[relSlash] = true
			if _, exempt := mutableTagExempt[relSlash]; exempt {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"%s: %s is a MUTABLE tag. Production manifests must pin @sha256: — "+
					"release.yml's pin-digests job writes them. Add a named exemption with a "+
					"written reason only if the image genuinely has no release digest.", relSlash, ref))
		}
		if _, exempt := mutableTagExempt[relSlash]; exempt && fileHasPinned && !fileHasMutable {
			problems = append(problems, relSlash+
				": every image here is digest-pinned but the file is still in mutableTagExempt — stale exemption, remove it")
		}

		// Per-file coverage floor: REFERENCE-level, not file-level. refCount==0
		// below only trips on TOTAL breakage across every file, and would not
		// have caught version one of this guard, which saw 39 of 42 references
		// and passed. A naive file-level floor (fire only when a file yields
		// ZERO matches) is not enough either: sixteen files under infra/ carry
		// TWO "ghcr.io/eighred/" references apiece (every *-deploy.yaml with a
		// kanz-migrate initContainer, plus preview-applicationset.yaml), and in
		// a multi-reference file one reference taking an unmatched form still
		// leaves the other matching, so a zero-match test stays silent — a
		// literal `:latest` written as a nested repository path
		// (`ghcr.io/eighred/kanz-migrate:latest`) or a `${TAG}`-
		// interpolated tag (`ghcr.io/eighred/kanz-migrate:${TAG}`) passes the
		// guard right next to a reference that still matches. The fix is to
		// count: every "ghcr.io/eighred/" occurrence in the comment-stripped
		// text must correspond to a parsed productionImageRef match, one for
		// one. A count mismatch means the pattern has narrowed and gone blind
		// to some reference form in THIS file (a nested repository path, a
		// ${TAG}-interpolated tag, a malformed digest, kustomize's split
		// newName/newTag — any of these carries the prefix without matching
		// the shape). coverageGapExempt names the one file that legitimately
		// has no pinnable reference at all: the sigstore glob.
		//
		// Compare against `scanned`, NOT `body`. `matches` was produced from
		// `scanned` (post-comment-strip), so the count it is compared to must
		// come from the same text — comparing against raw `body` would count
		// prefix occurrences living inside real YAML comments (which
		// yamlComment is supposed to remove) as if they were unmatched
		// references, reporting a spurious gap on every file with an ordinary
		// comment mentioning the prefix.
		if prefixCount != len(matches) {
			if _, known := coverageGapExempt[relSlash]; !known {
				problems = append(problems, fmt.Sprintf(
					"%s: %d \"ghcr.io/eighred/\" occurrence(s) in the comment-stripped text but "+
						"productionImageRef matched only %d of them — the pattern has narrowed and is "+
						"blind to some reference form here", relSlash, prefixCount, len(matches)))
			}
		}
		// Second, independent safety net: RAW BODY, NOT scanned. This checks the
		// literal string against `body` (pre-comment-strip) for the case where
		// yamlComment itself over-strips a real reference — if the comment
		// stripper ever ate part of a genuine image line, both `scanned`'s
		// prefix count and `matches` would drop together and the check above
		// would see them agree (both zero) while the reference is genuinely
		// lost. This arm exists to catch exactly that: total blindness (zero
		// matches) alongside evidence in the untouched raw text that a
		// reference was here. The consequence is that a file whose ONLY
		// "ghcr.io/eighred/" occurrence sits inside a YAML comment, with no
		// real image reference anywhere in the file, will also trip this arm
		// and demand a coverageGapExempt entry even though nothing is
		// unpinned. No file in this repo does that today; if one ever does,
		// the correct response is to add it to coverageGapExempt with a
		// reason saying so — not to change this check to scan `scanned`
		// instead.
		if len(matches) == 0 && strings.Contains(string(body), "ghcr.io/eighred/") {
			if _, known := coverageGapExempt[relSlash]; !known {
				problems = append(problems, relSlash+
					": contains \"ghcr.io/eighred/\" but productionImageRef matched zero references in it — "+
					"the pattern has narrowed and is blind to some reference form here")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk infra: %v", err)
	}

	// Non-vacuity, coarse floor: if the regex stops matching ANYTHING, this
	// test would pass having checked nothing. The per-file coverage check
	// above is the fine-grained floor that catches losing just one form,
	// which is how this guard's first version actually failed (39 of 42 seen,
	// and refCount==0 alone would never have noticed).
	if refCount == 0 {
		t.Fatal("no ghcr.io/eighred/ image references found under infra/ — the parse is broken, " +
			"and this test would otherwise pass vacuously")
	}

	// Anti-rot, direction two: an exemption for a file with no mutable tag left.
	for file := range mutableTagExempt {
		if !seenFiles[file] {
			problems = append(problems, file+
				": in mutableTagExempt but has no mutable tag — dead exemption, remove it")
		}
	}

	// Anti-rot for coverageGapExempt, same idiom as the mutableTagExempt loop
	// above: an exemption nobody re-checks is how this repository ends up with
	// exemptions that outlived their reason and silently disabled the check
	// they were carved out of. A coverageGapExempt entry is dead weight, with
	// nothing else flagging it, in either of two directions — the file it
	// names was never walked (deleted, renamed, or moved out of infra/), or
	// the file's prefix count now equals its match count (no gap left — the
	// coverage floor above found nothing to complain about, so there is
	// nothing left to excuse). Keyed to the same reference-level notion as the
	// floor itself (gapFiles, populated from prefixCount != len(matches)), not
	// to "the file was matched at all" — a file can have real matches AND
	// still have a gap (one of two references unmatched), and the old
	// file-level notion would have called that file's exemption dead while a
	// genuine gap was still open under it.
	for file := range coverageGapExempt {
		if !walkedFiles[file] {
			problems = append(problems, file+
				": in coverageGapExempt but was not found under infra/ — dead exemption, remove it")
			continue
		}
		if !gapFiles[file] {
			problems = append(problems, file+
				": in coverageGapExempt but has no coverage gap (prefix count equals match count) — dead exemption, remove it")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("mutable image tags in production manifests:\n\n  %s", strings.Join(problems, "\n  "))
	}
}

// THE CI TOOLCHAIN IS PART OF THE SUPPLY CHAIN, AND NOTHING PINNED IT.
//
// TestProductionManifestsPinImagesByDigest covers what the estate DEPLOYS.
// TestWorkflowActionsArePinnedToSHA covers what the estate USES as actions.
// Between them sat a third category with no guard at all: container images that
// workflows `docker run` directly, and the scripts those workflows invoke. #141
// pinned golangci-lint and wrote the reasoning down —
//
//	"latest is not a version; it is a promise that the gate can change
//	 without a commit."
//
// — then fixed one workflow. Four references were left (#155), and the argument
// applies hardest to the two that were missed:
//
//   - security.yml's gitleaks IS THE SECRET GATE. It is the one job whose silent
//     success is indistinguishable from a real pass — a scanner that stops
//     matching still exits 0. An upstream change to default rules or allowlist
//     semantics turns it quietly permissive with no commit to review.
//   - gitleaks-planted-secret.sh is the SEC-02e control that proves that gate
//     works. It ran `:latest` too, so the gate and its proof could drift
//     independently — a self-test that validates a different build than the one
//     in service is a green tick making a weaker claim than it appears to.
//
// k6 is the demonstration that this is not theoretical: it shipped v2.0.0 on
// 2026-05-11, so the p99/error-budget gate crossed an engine MAJOR under a
// `:latest` tag with no commit anywhere in this repository.
//
// Scope note: this guard reads workflow YAML and the shell scripts those
// workflows invoke. It does not scan Go source, so it does not match itself —
// but the `:latest` spellings in the prose above would match, which is why
// comments are stripped before scanning (yamlComment, shared with the digest
// guard). Match the OPERATION, not a mention of it: a guard that fires on a
// sentence describing the rule gets deleted rather than fixed.
func TestCIContainerImagesAreNotLatest(t *testing.T) {
	root := moduleRoot(t)
	repoRoot := filepath.Dir(root)

	scanned := ciExecutedFiles(t, root, repoRoot)

	// An image reference, not a bare mention: at least one path character must
	// precede the colon, so prose like "said :latest" cannot match even if a
	// comment survives stripping.
	latestRef := regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9._/-]*:latest\b`)

	// Default-deny. A genuinely unpinnable image goes here WITH the issue that
	// retires it; the dead-entry check below stops an exemption outliving its
	// repair. Empty today, and that is the point — every reference is pinned.
	exempt := map[string]string{}
	used := map[string]bool{}

	var problems []string
	for _, path := range scanned {
		raw, rErr := os.ReadFile(path)
		if rErr != nil {
			t.Fatalf("read %s: %v", path, rErr)
		}
		// Normalize CRLF for the same reason TestWorkflowActionsArePinnedToSHA
		// does: a Windows checkout hands these over with CRLF and a line-anchored
		// regex would behave differently here than on CI.
		body := strings.ReplaceAll(string(raw), "\r\n", "\n")
		body = yamlComment.ReplaceAllString(body, "")

		rel, _ := filepath.Rel(repoRoot, path)
		relSlash := filepath.ToSlash(rel)

		hits := latestRef.FindAllString(body, -1)
		if len(hits) == 0 {
			continue
		}
		if reason, ok := exempt[relSlash]; ok {
			used[relSlash] = true
			t.Logf("exempt: %s (%s)", relSlash, reason)
			continue
		}
		sort.Strings(hits)
		problems = append(problems, fmt.Sprintf(
			"%s: %s — `latest` is not a version, it is a promise the gate can change with no commit; "+
				"pin an explicit tag (or digest) so moving it is reviewable",
			relSlash, strings.Join(dedupeStrings(hits), ", ")))
	}

	for file, reason := range exempt {
		if !used[file] {
			problems = append(problems, fmt.Sprintf(
				"stale exemption: %s (%s) no longer references a :latest image — delete the entry, "+
					"an exemption that outlives its repair re-opens the hole silently", file, reason))
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("unpinned container images in CI:\n\n  %s", strings.Join(problems, "\n  "))
	}
}

// dedupeStrings keeps the failure message readable when one file names the same
// unpinned image more than once (latency.yml had two k6 steps).
func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ciExecutedFiles is everything CI actually runs: the workflows, plus the shell
// scripts they invoke. The second half is not optional — the fourth unpinned
// `:latest` in #155 was in a .sh file, and a workflows-only scan reported the
// repository clean while the control proving the secret gate still floated.
func ciExecutedFiles(t *testing.T, root, repoRoot string) []string {
	t.Helper()
	files := workflowFiles(t, repoRoot)
	shellDir := filepath.Join(root, "infra", "security", "test")
	entries, err := os.ReadDir(shellDir)
	if err != nil {
		t.Fatalf("read %s: %v — has the security test layout moved? "+
			"(this guard would otherwise silently stop covering shell scripts)", shellDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sh") {
			files = append(files, filepath.Join(shellDir, e.Name()))
		}
	}
	if len(files) == 0 {
		t.Fatal("no workflow or CI shell files found — has the layout changed? " +
			"(this test would otherwise pass vacuously)")
	}
	return files
}

// A `go install` IS A CONTAINER IMAGE BY ANOTHER SPELLING.
//
// TestCIContainerImagesAreNotLatest closed `:latest` on images CI runs. It could
// not see this one, because the same defect wears different syntax here:
//
//	go install golang.org/x/vuln/cmd/govulncheck@latest
//
// That was govulncheck — the vulnerability gate — floating, while every other
// `go install` in all seven workflows named an explicit version
// (protoc-gen-go@v1.36.11, protoc-gen-go-grpc@v1.5.1). Fourteen pinned, one
// floating, and the floating one decided whether known CVEs fail the build.
//
// The argument is #141's, unchanged: "latest is not a version; it is a promise
// that the gate can change without a commit." A tool that can fail a build must
// move by reviewable commit, whichever registry it comes from.
//
// `@main` and `@master` are rejected for the same reason and are worse — a
// branch tip is not even a release. A commit SHA or a semver tag both pass: what
// is required is immutability, not a particular spelling of it.
func TestCIToolInstallsArePinned(t *testing.T) {
	root := moduleRoot(t)
	repoRoot := filepath.Dir(root)
	scanned := ciExecutedFiles(t, root, repoRoot)

	// Capture the module path so the failure names WHICH tool floats, and the
	// version so a pinned one can be reported as such.
	installRe := regexp.MustCompile(`go\s+install\s+(\S+?)@(\S+)`)
	floating := map[string]bool{"latest": true, "main": true, "master": true, "HEAD": true}

	// Default-deny with a named-exemption escape hatch, same shape as the rest of
	// this file. Empty today: every install in the repository is pinned.
	exempt := map[string]string{}
	used := map[string]bool{}

	var problems []string
	pinned := 0
	for _, path := range scanned {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := strings.ReplaceAll(string(raw), "\r\n", "\n")
		body = yamlComment.ReplaceAllString(body, "")

		rel, _ := filepath.Rel(repoRoot, path)
		relSlash := filepath.ToSlash(rel)

		for _, m := range installRe.FindAllStringSubmatch(body, -1) {
			pkg, ver := m[1], strings.Trim(m[2], `"'`)
			if !floating[ver] {
				pinned++
				continue
			}
			key := relSlash + " " + pkg
			if reason, ok := exempt[key]; ok {
				used[key] = true
				t.Logf("exempt: %s (%s)", key, reason)
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"%s: `go install %s@%s` — %q is not a version, it is a promise the gate can "+
					"change with no commit; name an explicit version (or a commit SHA) so moving "+
					"it is reviewable",
				relSlash, pkg, ver, ver))
		}
	}

	// NON-VACUITY. If the regex stops matching — a syntax change, a layout move —
	// this test would report every workflow clean while nothing was checked. The
	// repository has many pinned installs; zero matches means the scanner broke.
	if pinned == 0 && len(problems) == 0 {
		t.Fatal("matched no `go install <pkg>@<version>` lines at all across the CI files — " +
			"the scanner is broken, not the estate (this test would otherwise pass vacuously)")
	}

	for key, reason := range exempt {
		if !used[key] {
			problems = append(problems, fmt.Sprintf(
				"stale exemption: %s (%s) is no longer a floating install — delete the entry, "+
					"an exemption that outlives its repair re-opens the hole silently", key, reason))
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("unpinned tool installs in CI:\n\n  %s", strings.Join(problems, "\n  "))
	}
	t.Logf("%d pinned `go install` reference(s) checked", pinned)
}

// A PRIVATE BASE IMAGE MEANS AUTHENTICATION IS A BUILD DEPENDENCY, NOT A
// PUBLISH DEPENDENCY.
//
// Before #161 every Dockerfile FROM was anonymous (Docker Hub, gcr.io), so a
// ghcr login was needed only to PUSH the built image. build.yml encoded exactly
// that: `if: github.event_name == 'push'`. Correct at the time.
//
// The cutover inverted it. Bases now come from ghcr.io/eighred/base/*, which is
// private by policy, so a workflow that builds an image cannot even START
// without a credential. Left as it was, build.yml would have gone green on main
// (pushes authenticate) and red on every pull_request, with the cause a 403 in a
// docker layer twelve steps down — the failure shape that costs an afternoon.
//
// latency.yml is the one that shows why this needs a guard rather than care: it
// has no build-push-action and does not look like an image-building workflow at
// all, just `docker compose up --build`. It had no ghcr login, and nothing would
// have reported that until the load stack failed as "gateway did not become
// ready", two layers from the truth.
//
// So the invariant is: if any Dockerfile pulls from the mirror, then every
// workflow that builds an image must log in to ghcr FIRST, unconditionally. The
// guard reads the FROM lines to decide whether it applies — it turns itself on
// from the state of the tree, rather than being a rule someone has to remember
// to keep in step with the Dockerfiles.
func TestWorkflowsBuildingMirrorImagesAuthenticateFirst(t *testing.T) {
	root := moduleRoot(t)
	repoRoot := filepath.Dir(root)

	// Does the invariant apply at all? If no FROM uses the mirror, bases are
	// anonymous and a build needs no credential — say so and stop, rather than
	// asserting a rule the tree has not opted into.
	froms := dockerfileFroms(t, repoRoot)
	if len(froms) == 0 {
		t.Fatal("found zero Dockerfiles — the scanner is broken, not the estate")
	}
	usesMirror := false
	for _, f := range froms {
		if strings.HasPrefix(f.image, mirrorPrefix) {
			usesMirror = true
			break
		}
	}
	if !usesMirror {
		t.Skipf("no Dockerfile pulls from %s — bases are anonymous, so a build needs no "+
			"registry credential and this guard does not apply", mirrorPrefix)
	}

	// A workflow builds an image if it runs docker build, a build-push-action, or
	// a compose build. The last is the one that hid: it names no image and no
	// action, so a check written against `build-push-action` alone would have
	// reported latency.yml clean.
	buildsImage := regexp.MustCompile(`docker/build-push-action|docker\s+build|compose[^\n]*\s--build|docker\s+compose\s+build`)
	loginAction := regexp.MustCompile(`docker/login-action`)
	// A login guarded by `if:` is not a login — it is a login on some events. The
	// whole defect was a conditional one, so a conditional login must not satisfy
	// this. Matches an `if:` on the same step block as the login (the two lines
	// before it, which is where docker/login-action carries its condition).
	conditionalLogin := regexp.MustCompile(`(?m)^\s*if:.*\n(?:\s*(?:-\s*)?name:.*\n)?\s*(?:-\s*)?uses:\s*docker/login-action`)

	var problems []string
	checked := 0
	for _, path := range workflowFiles(t, repoRoot) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := strings.ReplaceAll(string(raw), "\r\n", "\n")
		scan := yamlComment.ReplaceAllString(body, "")

		rel, _ := filepath.Rel(repoRoot, path)
		relSlash := filepath.ToSlash(rel)

		if !buildsImage.MatchString(scan) {
			continue
		}
		checked++

		switch {
		case !loginAction.MatchString(scan):
			problems = append(problems, fmt.Sprintf(
				"%s builds an image but never logs in to ghcr.io — every Dockerfile FROM now "+
					"resolves to %s, which is private, so the base pull will 403 before any "+
					"build step runs (#161)", relSlash, mirrorPrefix))
		case conditionalLogin.MatchString(scan):
			problems = append(problems, fmt.Sprintf(
				"%s guards its docker/login-action with an `if:` — authentication is now a BUILD "+
					"dependency, not a publish one, so a conditional login means the events it "+
					"excludes cannot build at all. This is exactly the `if: github.event_name == "+
					"'push'` that made main green and every pull_request red (#161)", relSlash))
		}
	}

	// NON-VACUITY. The estate builds 26 images; if this matched no workflow the
	// regex is wrong, and reporting "all clear" would be worse than saying so.
	if checked == 0 {
		t.Fatal("matched no image-building workflow at all — the scanner is broken, not the " +
			"estate (this test would otherwise pass vacuously while every build was unauthenticated)")
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("image builds without a registry credential:\n\n  %s", strings.Join(problems, "\n  "))
	}
	t.Logf("%d image-building workflow(s) checked, all authenticate unconditionally", checked)
}
