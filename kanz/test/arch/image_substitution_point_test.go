package arch

import (
	"path/filepath"
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

	// 2. NO SECOND MECHANISM. Walk every Argo CD Application/ApplicationSet and
	//    look for the image-override surfaces: kustomize.images, kustomize's
	//    newTag/newName, and helm parameters naming an image. Presence is not
	//    automatically wrong — it is wrong unless somebody wrote down why.
	found := map[string]string{} // repo-relative file -> what was found
	for _, rel := range []string{
		filepath.Join("infra", "gitops", "applicationset.yaml"),
		filepath.Join("infra", "gitops", "preview-applicationset.yaml"),
		filepath.Join("infra", "gitops", "bootstrap.yaml"),
	} {
		body := readFile(t, filepath.Join(root, rel))
		key := filepath.ToSlash(rel)
		if mech := imageOverrideMechanism(t, body); mech != "" {
			found[key] = mech
		}
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
