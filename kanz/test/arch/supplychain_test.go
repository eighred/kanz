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
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
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
// `images[].glob` and its keyless `subjectRegExp`) name ghcr.io/kanz-eng. The
// two steps that actually PUSH images computed their target from
// ${{ github.repository_owner }}, which for this repository resolves to
// "eighred". So a green release.yml would have published images the estate
// could never pull, under an identity the admission policy would never
// accept — and nothing would have said so until someone tried.
//
// The owner's decision (REL-P0a, 2026-07-27) is that eighred is canonical — see
// canonicalImageOrg for why kanz-eng was never available. This test is
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
// comfortably (53 ghcr matches — 39 kanz-eng + 14 spiffe — across 45 files at
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
// It was "kanz-eng" until 2026-07-27, which broke every push to main: all 25
// images failed at the push step because GITHUB_TOKEN is scoped to the repo's
// owner and cannot write another org's packages. The investigation found that
// **kanz-eng does not exist on GitHub at all** — so the value named a namespace
// nothing could ever publish to, while ghcr.io/eighred/* already held working
// images. Owner decision (REL-P0a): adopt eighred as canonical.
//
// This is deliberately a LITERAL and not ${{ github.repository_owner }}. A
// computed owner is what let the publish side and the pull side disagree
// silently in the first place; a literal means that if the repository ever does
// move to a kanz-eng org, this guard fails loudly and forces the manifests, the
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
var thirdPartyImageOrgs = map[string]struct{}{
	"spiffe":   {},
	"gitleaks": {},
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

	// TEMPORARY — retire on the first release that publishes this image.
	// nats-rebuild joined the build/release matrices in the same branch as this
	// guard, so no digest exists for it yet. release.yml's pin-digests job
	// rewrites it on the first tagged run; delete this line then, and the dead-
	// exemption arm below will fail the build until it is deleted.
	"infra/dr/nats/rebuild-job.yaml": "image added to the CI matrices in this branch; no published digest exists " +
		"until the next release. Retire on the first pin-digests run",
}

// productionImageRef matches an image reference to our own registry, by the
// SHAPE of the reference rather than by the YAML key that carries it.
//
// Keying on `image:` was the first attempt and it was WRONG THREE WAYS, all
// found by running it:
//   - kustomize carries a plural `images:` LIST of bare quoted strings
//     (infra/gitops/preview-applicationset.yaml:53-55), so both preview refs
//     were invisible — and the exemption declared for that file was therefore
//     permanently dead, which failed the build on a clean tree;
//   - infra/deploy/operator-deploy.yaml:179 passes the provisioner image as an
//     env var `value:`, not an `image:`. That is the image the operator injects
//     into every provisioning Job spec it builds, so a mutable tag there ships
//     an unpinned provisioner to every TUI-provisioned node — precisely the
//     supply chain this guard exists to protect, and it was outside its view;
//   - it silently defined "production manifest" as "whatever uses the key I
//     thought of", which is how a guard ends up asserting less than it claims.
//
// A reference must carry a tag or a digest, which is what distinguishes it from
// the sigstore GLOB at infra/security/admission/cluster-image-policy.yaml:27
// (`ghcr.io/eighred/**`) — a policy pattern, not an image, and not pinnable.
var productionImageRef = regexp.MustCompile(
	`ghcr\.io/eighred/[a-z0-9][a-z0-9._-]*(?:@sha256:[a-f0-9]{64}|:[A-Za-z0-9._{}-]+)`)

// yamlComment strips trailing `#` comments so the guard scans CONFIGURATION,
// not prose. Without this, matching on reference shape would trip on a comment
// mentioning an example tag. `d34bb7d` fixed this same class of defect in the
// NATS posture guards, where needles matched the very comments that documented
// them.
var yamlComment = regexp.MustCompile(`(?m)#.*$`)

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

		scanned := yamlComment.ReplaceAllString(string(body), "")
		for _, ref := range productionImageRef.FindAllString(scanned, -1) {
			refCount++
			if strings.Contains(ref, "@sha256:") {
				if _, exempt := mutableTagExempt[relSlash]; exempt {
					problems = append(problems, relSlash+
						": every image here is digest-pinned but the file is still in mutableTagExempt — stale exemption, remove it")
				}
				continue
			}
			seenFiles[relSlash] = true
			if _, exempt := mutableTagExempt[relSlash]; exempt {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"%s: %s is a MUTABLE tag. Production manifests must pin @sha256: — "+
					"release.yml's pin-digests job writes them. Add a named exemption with a "+
					"written reason only if the image genuinely has no release digest.", relSlash, ref))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk infra: %v", err)
	}

	// Non-vacuity: if the regex stops matching, this test would pass having
	// checked nothing.
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

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("mutable image tags in production manifests:\n\n  %s", strings.Join(problems, "\n  "))
	}
}
