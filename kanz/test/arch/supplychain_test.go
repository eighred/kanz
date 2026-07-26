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
// Every platform image is ghcr.io/kanz-eng/*, and that repository is private
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

// privateImagePrefix is the registry path that requires the credential.
const privateImagePrefix = "ghcr.io/kanz-eng/"

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
// The owner's decision (2026-07-26) is that kanz-eng is canonical. This test is
// what keeps the two halves from drifting apart again: publish targets, pull
// references, and the signing identity are the same string or the build fails.
//
// WHY IT READS POSITIONS, NOT TEXT. A comment at infra/deploy/operator-deploy.yaml
// quotes the error string "403 Forbidden from ghcr.io/token". A guard that grepped
// free text would flag that comment as an image reference under an org called
// "token". So this walks YAML keys that actually carry image references.
//
// THREE INDEPENDENT CHECKS, THREE INDEPENDENT FLOORS. Manifest/workflow image
// references (the `imageRefKeys` below), the ClusterImagePolicy's `glob`, and
// the signing identity (`subjectRegExp`) are three unrelated YAML shapes — an
// `image: ghcr.io/org/svc` field, a `glob: "ghcr.io/org/**"` list entry, and a
// `subjectRegExp: "^https://github.com/org/repo/..."` string share no key name
// and no value format. Counting them into one pooled total would let a
// coverage regression in either of the smaller checks hide behind padding
// from the largest one: manifest `image:` references alone clear 40
// comfortably (82 occurrences across 45 files at time of writing), while
// `glob` and `subjectRegExp` each have exactly ONE occurrence in this repo —
// if `cluster-image-policy.yaml` were deleted, or `glob`/`subjectRegExp` were
// renamed by a future sigstore API, the pooled count would barely move and
// the guard would still pass. Each check gets its own non-vacuity floor
// instead, so losing either one fails loudly rather than hiding in the noise
// of the other two.
//
// BLOCK SCALARS. docker/metadata-action documents a multi-line form,
// `images: |` followed by indented ghcr.io lines. Both current workflows use
// the single-line form (`images: ghcr.io/...`), but a future one could use the
// block form — so image-reference values are scanned line-by-line and, when a
// key's inline value is a bare block-scalar indicator (`|`, `|-`, `>`, etc.),
// the following more-indented lines are pulled in as that key's value too.
const canonicalImageOrg = "kanz-eng"

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

// imageRefFloor, globFloor and subjectRegExpFloor are non-vacuity backstops
// for the three independent checks TestImageOrgIsCanonical runs (see the
// test's doc comment for why they are not pooled into one count). If any
// check's real match count ever drops below its floor, the key set, file
// layout, or value shape changed and that check is silently asserting almost
// nothing. globFloor and subjectRegExpFloor are 1, not higher, because each
// field has exactly one legitimate occurrence in this repo today — the point
// of a floor of 1 is that zero must fail loudly, not that many are expected.
const imageRefFloor = 40
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
	imageRefsChecked := 0
	globChecked := 0
	subjectRegExpsChecked := 0

	for _, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		rel, _ := filepath.Rel(repoRoot, path)
		relSlash := filepath.ToSlash(rel)

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
				imageRefsChecked++
				checkOrg(&problems, relSlash, key, g[1], false)
			}
		}
	}

	if imageRefsChecked < imageRefFloor {
		t.Fatalf("only %d ghcr.io image references matched across workflows and infra/ — "+
			"expected at least %d; the key set or layout changed and this test is now "+
			"asserting almost nothing about image references", imageRefsChecked, imageRefFloor)
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
