package arch

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// THERE IS EXACTLY ONE PLACE THAT DECIDES WHICH IMAGE THE ESTATE RUNS.
//
// OPS-M4a (#81) states the constraint as a choice: "Choose ONE substitution point
// — release.yml writing an image-digest map the GitOps layer consumes, OR Argo
// kustomize.images — never both." The choice was made — pin-digests rewrites the
// manifests in git — and TestProductionManifestsPinImagesByDigest then holds those
// manifests to digests. Neither of those notices a SECOND mechanism appearing
// beside them, and that is the failure this guard exists for.
//
// Two substitution points is worse than either one alone, because they do not
// conflict loudly. Manifests in git would say @sha256:abc while Argo's override
// silently deployed :latest, and every artifact a reader consults to answer "what
// is running?" — the manifest, the pin PR, the cosign attestation — would agree
// with each other and disagree with the cluster. The digest guard would stay green
// throughout: it reads git, and git would be correct. The lie would live entirely
// in the layer nobody diffs.
//
// It also destroys the rollback primitive. Rollback here means "re-point at the
// previous digest", which is only meaningful if the digest in git is what runs.
// An override upstream of it makes the TUI's rollback (#82) a no-op that reports
// success — the same shape as the DR failures elsewhere in this repo, where the
// control ran, said nothing, and changed nothing.
func TestExactlyOneImageSubstitutionPoint(t *testing.T) {
	root := moduleRoot(t)

	// 1. THE CHOSEN POINT MUST STILL EXIST. If pin-digests disappears, the digest
	//    guard keeps passing on whatever digests were last written and slowly
	//    becomes an assertion about history rather than about releases.
	release := readFile(t, filepath.Join(root, "..", ".github", "workflows", "release.yml"))
	if !strings.Contains(release, "kanz-pin-digests") {
		t.Error("release.yml no longer runs cmd/kanz-pin-digests\n\n" +
			"That binary IS the substitution point: it rewrites every manifest from the tag it was " +
			"authored with to the digest the release published. Without it the manifests keep whatever " +
			"digests they already had, TestProductionManifestsPinImagesByDigest keeps passing on those " +
			"stale values, and new releases are signed but never deployed — which is the exact state " +
			"#81 was opened to end.")
	}

	// 2. NO SECOND MECHANISM. Look for the image-override surfaces —
	//    kustomize.images, kustomize's newTag/newName, and helm parameters naming
	//    an image — in EVERY Argo CD Application/ApplicationSet in the repository.
	//    Presence is not automatically wrong; it is wrong unless somebody wrote
	//    down why.
	//
	// DISCOVERED BY KIND, NOT READ FROM A LIST. This loop used to open three
	// hard-coded paths — applicationset.yaml, preview-applicationset.yaml,
	// bootstrap.yaml — which happened to be every Argo document in the repo on
	// the day it was written, and made the guard's real property "none of these
	// three files declares a second substitution point" rather than the one its
	// name claims. A FOURTH FILE WAS INVISIBLE. Measured, not reasoned about: an
	// ApplicationSet carrying `kustomize: {newTag: latest}` — which overrides
	// EVERY pinned digest in the estate with a floating tag, the exact silent
	// disagreement between git and the cluster this file's header describes —
	// was added under infra/ and the WHOLE arch suite stayed green.
	//
	// It is the same lesson infraYAMLFiles already records one file over: "the
	// two guards that came before this one each read a single hard-coded file,
	// which is how a namespace with no policy at all stayed invisible". A guard
	// against a mechanism arriving cannot enumerate the places it may arrive.
	found := map[string]string{} // repo-relative file -> what was found
	argoDocs := 0
	for _, path := range repoYAMLFiles(t, root) {
		body := readFile(t, path)
		if !declaresArgoApplication(body) {
			continue
		}
		argoDocs++
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("relativize %s: %v", path, err)
		}
		if mech := imageOverrideMechanism(t, body); mech != "" {
			found[filepath.ToSlash(rel)] = mech
		}
	}

	// NON-VACUITY ON THE DISCOVERY ITSELF. The three files above are known to be
	// Argo documents, so finding fewer means the walk or the kind check is broken
	// — and a broken discovery reports "no second substitution point" for the
	// same reason an empty estate would, which is the failure mode this rewrite
	// exists to end.
	if argoDocs < 3 {
		t.Fatalf("found only %d Argo Application/ApplicationSet document(s) in the repository — "+
			"applicationset.yaml, preview-applicationset.yaml and bootstrap.yaml are all known to "+
			"be Argo documents, so the walk or the kind check is broken, not the estate. A guard "+
			"that discovers nothing cannot tell 'one substitution point' from 'none'", argoDocs)
	}

	for _, f := range sortedKeys(found) {
		if _, ok := substitutionPointExempt[f]; !ok {
			t.Errorf("%s declares an image-override mechanism (%s), which is a SECOND image "+
				"substitution point\n\n"+
				"pin-digests already rewrites manifests in git to the digest each release published. "+
				"An override in the GitOps layer sits downstream of that, so the two can disagree "+
				"about what the cluster runs — and the disagreement is silent: git, the pin PR and the "+
				"cosign attestation would all still agree with each other. Remove it, or add %q to "+
				"substitutionPointExempt with the reason it cannot be the digest path.", f, found[f], f)
		}
	}

	// 3. DEAD-ENTRY CHECK. An exemption for a file that no longer overrides
	//    anything reads as a live second mechanism to whoever audits this next,
	//    and sends them looking for a conflict that does not exist.
	for _, f := range sortedKeys(substitutionPointExempt) {
		if _, ok := found[f]; !ok {
			t.Errorf("substitutionPointExempt names %q but it declares no image-override mechanism\n\n"+
				"Either the override was removed or the file was renamed. Delete the entry — an "+
				"exemption that outlives its subject makes a solved problem look open.", f)
		}
	}

	// 4. NON-VACUITY. The preview override is real and expected; if the detector
	//    stops seeing it, the detector is broken and every future override would
	//    pass unnoticed.
	if len(found) == 0 {
		t.Error("found no image-override mechanism anywhere, including the per-PR preview " +
			"ApplicationSet that is known to use kustomize.images\n\n" +
			"The detector is broken, not the estate. A guard that finds nothing cannot tell " +
			"'one substitution point' from 'none'.")
	}
}

// substitutionPointExempt names GitOps files allowed to override images, with the
// reason the digest path cannot serve them.
var substitutionPointExempt = map[string]string{
	"infra/gitops/preview-applicationset.yaml": "per-PR preview environments deploy an image built FOR that " +
		"pull request (:pr-N, pushed by preview.yml), so no release digest exists to pin to — the commit " +
		"under review has not been released. It is not a second opinion about what production runs: it " +
		"targets its own namespace (kanz-preview-N) with a namePrefix, and the tag encodes the PR that " +
		"owns it. Same reasoning already recorded in mutableTagExempt for this file.",
}

// imageOverrideMechanism reports which image-override surface a GitOps manifest
// declares, or "" for none.
//
// Parsed structurally rather than grepped for `images:`. That string appears in
// prose and in unrelated keys, and a guard that fires on a comment is one people
// learn to route around — the same defect d34bb7d fixed in the NATS guards and
// that TestProductionManifestsPinImagesByDigest strips comments to avoid.
func imageOverrideMechanism(t *testing.T, body string) string {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(body))
	for {
		var doc struct {
			Spec struct {
				// Application: source lives directly under spec.
				Source appSource `yaml:"source"`
				// ApplicationSet: source lives under spec.template.spec.
				Template struct {
					Spec struct {
						Source appSource `yaml:"source"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			return ""
		}
		for _, s := range []appSource{doc.Spec.Source, doc.Spec.Template.Spec.Source} {
			if len(s.Kustomize.Images) > 0 {
				return "kustomize.images"
			}
			if s.Kustomize.NewTag != "" || s.Kustomize.NewName != "" {
				return "kustomize.newTag/newName"
			}
			for _, p := range s.Helm.Parameters {
				// Helm's conventional image keys. Narrow deliberately: matching any
				// parameter containing "image" would fire on things like
				// `imagePullPolicy`, which decides nothing about WHICH image runs.
				n := strings.ToLower(p.Name)
				if n == "image.tag" || n == "image.repository" || n == "image.digest" ||
					strings.HasSuffix(n, ".image.tag") || strings.HasSuffix(n, ".image.repository") {
					return "helm.parameters:" + p.Name
				}
			}
		}
	}
}

type appSource struct {
	Kustomize struct {
		Images  []string `yaml:"images"`
		NewTag  string   `yaml:"newTag"`
		NewName string   `yaml:"newName"`
	} `yaml:"kustomize"`
	Helm struct {
		Parameters []struct {
			Name string `yaml:"name"`
		} `yaml:"parameters"`
	} `yaml:"helm"`
}

// repoYAMLFiles returns every YAML file in the repository, both extensions.
//
// THE WHOLE REPO, NOT infra/. An Argo Application is a plain manifest and
// nothing confines one to infra/gitops — a bootstrap document under .github/,
// a DR runbook's manifest, or a new directory nobody has thought of yet are all
// places one can appear, and the guard above exists precisely for the file
// nobody anticipated. infraYAMLFiles is the narrower sibling of this and is
// deliberately not reused: it takes only ".yaml", so a rogue ".yml" would slip
// past, and it stops at infra/.
//
// Directories that cannot hold a hand-written manifest are skipped so the walk
// stays quick on a repo carrying node_modules and a web build.
func repoYAMLFiles(t *testing.T, moduleRootDir string) []string {
	t.Helper()
	repoRoot := filepath.Dir(moduleRootDir)
	skip := map[string]bool{
		".git": true, ".claude": true, "node_modules": true, "dist": true, "vendor": true,
		"build": true, ".next": true, "coverage": true,
	}
	var files []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// OTHER CHECKOUTS FIRST (#848). This walk starts at the REPO ROOT, so it
			// can see .claude/worktrees/ — another checkout of this same tree, whose
			// files are not the estate's. The shared set is what every repo-root guard
			// consults; the local names below are per-guard speed hints on top of it.
			if skipWalkDir(d) {
				return filepath.SkipDir
			}
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", repoRoot, err)
	}
	// The same floor infraYAMLFiles carries, for the same reason: a walk that
	// returns almost nothing makes every guard built on it pass by finding
	// nothing.
	if len(files) < 40 {
		t.Fatalf("found only %d YAML files in the repository — the walk is broken, and every "+
			"guard built on it would pass by finding nothing", len(files))
	}
	sort.Strings(files)
	return files
}

// declaresArgoApplication reports whether a manifest carries an Argo CD
// Application or ApplicationSet document.
//
// Decoded as a YAML stream and matched on apiVersion + kind, never grepped:
// "kind: Application" appears in prose in this repository's README files, and a
// guard that fires on documentation is one people learn to route around — the
// same reasoning imageOverrideMechanism gives for parsing rather than grepping.
// The apiVersion check is what keeps an unrelated CRD that happens to be called
// Application out.
func declaresArgoApplication(body string) bool {
	dec := yaml.NewDecoder(strings.NewReader(body))
	for {
		var doc struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
		}
		if err := dec.Decode(&doc); err != nil {
			return false
		}
		if !strings.HasPrefix(doc.APIVersion, "argoproj.io/") {
			continue
		}
		if doc.Kind == "Application" || doc.Kind == "ApplicationSet" {
			return true
		}
	}
}
