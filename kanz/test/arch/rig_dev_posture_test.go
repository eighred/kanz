package arch

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// THE RIG HAS EXACTLY ONE DECLARED DEVIATION FROM PRODUCTION.
//
// The rig's manifests were hand-stripped of their CSI volumes on 2026-07-12 and
// nothing recorded that, so the rig drifted eight days from the repo and could
// not be used as evidence about any change. rig-dev-secrets.yaml replaces the
// hand-stripping with one declared, guarded substitution.
//
// This guard pins two things: that the dev Secret carries the SAME Postgres
// convention as the rest of the repo, and that it cannot quietly become a
// production manifest.
func TestRigDevSecretsShadowEverySecretProviderClassTheRigMounts(t *testing.T) {
	root := moduleRoot(t)
	dev := readFile(t, filepath.Join(root, "infra", "deploy", "rig-dev-secrets.yaml"))
	pg := readFile(t, filepath.Join(root, "infra", "deploy", "postgres-dev.yaml"))

	// Comments are prose, not configuration. Both of this repo's previous posture
	// guards were dead because their needles matched a comment (see the board's
	// "guards that could not fire" row); strip them before scanning.
	devCfg := stripYAMLComments(dev)
	pgCfg := stripYAMLComments(pg)

	// --- every mounted SecretProviderClass must have a same-named dev Secret ---
	//
	// This is the arm that matters. rig-apply.sh rewrites each Vault CSI volume into
	// a reference to a Secret named after its SecretProviderClass; if that Secret is
	// not declared anywhere, the workload mounts nothing and fails at boot. The rig
	// hit exactly this on 2026-07-20 — tv-sync-db did not exist in the cluster — and
	// nothing in the repo would have told us.
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*name:\s*(\S+)`).FindAllStringSubmatch(devCfg+pgCfg, -1) {
		declared[m[1]] = true
	}

	for _, svc := range rigWorkloads {
		manifest := stripYAMLComments(readFile(t, filepath.Join(root, "infra", "deploy", svc)))
		for _, m := range regexp.MustCompile(`secretProviderClass:\s*(\S+)`).FindAllStringSubmatch(manifest, -1) {
			spc := m[1]
			if !declared[spc] {
				t.Errorf("%s mounts SecretProviderClass %q but no dev Secret of that name is declared "+
					"in rig-dev-secrets.yaml or postgres-dev.yaml.\n"+
					"tools/rig-apply.sh rewrites the Vault CSI volume into secretName: %s, so the pod "+
					"will mount an empty volume and report a config error that looks like a code bug and "+
					"is not. Declare the Secret with the SecretProviderClass's objectName values as its "+
					"KEYS — those are the filenames the service reads.", svc, spc, spc)
			}
		}
	}

	// redis.yaml lives under infra/messaging, not infra/deploy, so it is not a
	// rigWorkloads entry — but tools/rig-apply.sh now routes it through the same
	// tools/rig_dev_patch.py as the workloads above, so its Vault CSI volume
	// (redis-auth) is bound by the exact same rule: no matching dev Secret means an
	// empty mount and Redis exits 1 rather than start unauthenticated.
	redisManifest := stripYAMLComments(readFile(t, filepath.Join(root, "infra", "messaging", "redis.yaml")))
	for _, m := range regexp.MustCompile(`secretProviderClass:\s*(\S+)`).FindAllStringSubmatch(redisManifest, -1) {
		spc := m[1]
		if !declared[spc] {
			t.Errorf("infra/messaging/redis.yaml mounts SecretProviderClass %q but no dev Secret of that name is "+
				"declared in rig-dev-secrets.yaml or postgres-dev.yaml.\n"+
				"tools/rig-apply.sh routes this manifest through tools/rig_dev_patch.py, which rewrites the Vault "+
				"CSI volume into secretName: %s — an undeclared Secret mounts empty and Redis's own start script "+
				"refuses to run rather than come up unauthenticated.", spc, spc)
		}
	}

	// --- the redis-url HOST and the redis-auth NAMESPACE must match redis.yaml's own Service ---
	//
	// The original Critical-1 bug was exactly a wrong host: redis-url pointed at
	// redis.kanz-services.svc while Redis's Service is declared in kanz-messaging, so
	// the DNS name could never resolve. The password-match arm below would not have
	// caught that — it only compares the two passwords, never the host — so a future
	// edit that reverted the host (or moved redis-auth back to kanz-services) would
	// pass every existing arm and silently reintroduce the exact bug.
	//
	// Derived, not hardcoded: parsed structurally out of infra/messaging/redis.yaml's
	// own Service document (gopkg.in/yaml.v3, the same parser and pattern
	// TestEveryDeclaredVolumeIsMounted uses), so a manifest edit that moves Redis to a
	// different namespace changes what this guard expects rather than leaving it
	// checking a copied-and-pasted literal.
	var redisSvcName, redisSvcNamespace string
	dec := yaml.NewDecoder(strings.NewReader(redisManifest))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("infra/messaging/redis.yaml: parse as YAML: %v", err)
		}
		if doc.Kind == "Service" {
			redisSvcName = doc.Metadata.Name
			redisSvcNamespace = doc.Metadata.Namespace
			break
		}
	}
	if redisSvcName == "" || redisSvcNamespace == "" {
		t.Fatal("infra/messaging/redis.yaml: found no Service to derive the redis-url host from. " +
			"This guard needs a Service document (kind: Service) to know the DNS name Redis actually answers on.")
	}
	wantHost := redisSvcName + "." + redisSvcNamespace + ".svc"

	urlHost := regexp.MustCompile(`redis-url:\s*redis://:[^@\s]+@([^\s:]+):`).FindStringSubmatch(devCfg)
	if urlHost == nil {
		t.Fatal("rig-dev-secrets.yaml's webhook-ingest-redis Secret no longer carries a redis://:<password>@<host>: DSN " +
			"this guard can parse a host out of.")
	}
	if urlHost[1] != wantHost {
		t.Errorf("rig-dev-secrets.yaml: webhook-ingest-redis's redis-url points at host %q, but "+
			"infra/messaging/redis.yaml's Service is %q in namespace %q, which only resolves as %q. "+
			"A wrong host here is exactly the Critical-1 bug this rig shipped with: the DSN's host cannot "+
			"resolve, and webhook-ingest never reaches its nonce store.", urlHost[1], redisSvcName, redisSvcNamespace, wantHost)
	}

	authNamespace := regexp.MustCompile(`(?s)name:\s*redis-auth\b.*?namespace:\s*(\S+)`).FindStringSubmatch(devCfg)
	if authNamespace == nil {
		t.Fatal("rig-dev-secrets.yaml declares no redis-auth Secret with a namespace this guard can parse.")
	}
	if authNamespace[1] != redisSvcNamespace {
		t.Errorf("rig-dev-secrets.yaml: the redis-auth Secret declares namespace %q, but "+
			"infra/security/secrets/secretproviderclass.yaml's redis-auth SecretProviderClass (and the Redis pod "+
			"that mounts it) live in namespace %q, matching infra/messaging/redis.yaml's own Service. A same-named "+
			"Secret in the wrong namespace is invisible to the pod that needs it — the mount resolves to nothing "+
			"and Redis refuses to start unauthenticated.", authNamespace[1], redisSvcNamespace)
	}

	// --- the redis-auth password and the password embedded in redis-url MUST MATCH ---
	//
	// Redis authenticates with exactly one password (redis.yaml's --requirepass); a
	// webhook-ingest DSN carrying a different one connects to nothing and the
	// cross-pod nonce store never comes up. Two literal copies of a secret in one
	// file is exactly the kind of drift a text diff misses, so parse both out of the
	// SAME devCfg blob and compare them structurally rather than trusting they were
	// typed identically.
	urlPassword := regexp.MustCompile(`redis-url:\s*redis://:([^@\s]+)@`).FindStringSubmatch(devCfg)
	authPassword := regexp.MustCompile(`(?s)name:\s*redis-auth\b.*?password:\s*(\S+)`).FindStringSubmatch(devCfg)
	if urlPassword == nil {
		t.Fatal("rig-dev-secrets.yaml's webhook-ingest-redis Secret no longer carries a redis://:<password>@ DSN. " +
			"Redis refuses to start unauthenticated (infra/messaging/redis.yaml), so an unauthenticated DSN cannot work.")
	}
	if authPassword == nil {
		t.Fatal("rig-dev-secrets.yaml declares no redis-auth Secret with a password key. infra/messaging/redis.yaml " +
			"mounts secretProviderClass: redis-auth and refuses to start without one.")
	}
	if urlPassword[1] != authPassword[1] {
		t.Errorf("rig-dev-secrets.yaml: the password embedded in webhook-ingest-redis's redis-url (%q) does not "+
			"match redis-auth's password (%q). Redis authenticates with exactly one password; a webhook-ingest DSN "+
			"carrying a different one connects to nothing and the cross-pod nonce store never comes up.",
			urlPassword[1], authPassword[1])
	}

	// --- every database a dev Secret addresses must be one Postgres actually creates ---
	//
	// A DSN naming a database postgres-dev.yaml never CREATEs fails at runtime with an
	// authentication error that looks like a wrong password and is really a missing
	// database — the failure that cost a session on 2026-07-20. This checks BOTH files'
	// DSNs (oms-db in postgres-dev, tv-sync-db + redis in rig-dev-secrets) against the
	// set of databases the initdb script creates. It is why tv-sync gets its OWN
	// database (tvsync): kanz-migrate keys schema_migrations by version, so sharing the
	// OMS's database makes their two 0001 migrations collide, and the guard now proves
	// the separate database it depends on is actually provisioned.
	created := map[string]bool{}
	for _, m := range regexp.MustCompile(`CREATE DATABASE\s+(\w+)`).FindAllStringSubmatch(pgCfg, -1) {
		created[m[1]] = true
	}
	if len(created) == 0 {
		t.Fatal("postgres-dev.yaml's initdb no longer CREATEs any database. That manifest is the source " +
			"of truth for which databases the rig's Secrets may address (itself pinned to CI); if it " +
			"changed shape, teach this guard the new one rather than deleting it.")
	}
	pgDSNs := regexp.MustCompile(`postgres://[^/\s"']+/([A-Za-z0-9_]+)`).FindAllStringSubmatch(devCfg+pgCfg, -1)
	if pgDSNs == nil {
		t.Fatal("no postgres:// DSN with a database name found in rig-dev-secrets.yaml or postgres-dev.yaml. " +
			"The rig's workloads reach Postgres through these DSNs; if their shape changed, teach this guard.")
	}
	for _, m := range pgDSNs {
		if db := m[1]; !created[db] {
			t.Errorf("a dev Secret's DSN addresses database %q, but postgres-dev.yaml's initdb never "+
				"CREATE DATABASE %s. The pod fails at runtime with an authentication error that looks like a "+
				"wrong password and is really a missing database — the failure that cost a session on "+
				"2026-07-20. Point the DSN at a created database, or add the database to the initdb script.", db, db)
		}
	}

	// --- posture ---------------------------------------------------------------
	// Scans the RAW file: the header is a comment, and stripping comments first
	// would make this arm permanently true. That inversion is deliberate.
	if !strings.Contains(dev, "DEV-ONLY") {
		t.Error("rig-dev-secrets.yaml must state DEV-ONLY in its header. It commits real-looking " +
			"credentials in plaintext; the only thing making that acceptable is that it is " +
			"unmistakably a dev artifact.")
	}
	// EVERY Secret must carry the label, not merely one of them. A plain
	// strings.Contains here would pass as long as ANY Secret in the file kept the
	// correct label, so relabeling a single Secret (leaving the rest alone) would
	// slip straight through — this repo has already shipped that exact class of
	// dead guard twice (see the board's "guards that could not fire" row, and
	// commits d34bb7d / 0d48f3c which had to fix it). Count instances instead of
	// merely detecting presence.
	wantSecrets := strings.Count(devCfg, "kind: Secret")
	gotLabels := strings.Count(devCfg, "kanz.eighred.com/posture: dev-only")
	if gotLabels < wantSecrets {
		t.Errorf("rig-dev-secrets.yaml declares %d Secret(s) but only %d carry the label "+
			"kanz.eighred.com/posture: dev-only, matching postgres-dev.yaml. A second spelling of "+
			"the posture label on even one Secret is a second convention, and the point of this "+
			"file is that the rig has exactly one.", wantSecrets, gotLabels)
	}

	// --- it must never grow into the production secret surface ------------------
	for _, forbidden := range []string{"binance", "okx", "api_key", "apikey", "private_key"} {
		if strings.Contains(strings.ToLower(devCfg), forbidden) {
			t.Errorf("rig-dev-secrets.yaml contains %q. Exchange credentials do not belong in a "+
				"committed file under any posture. If the rig needs a venue adapter, point it at "+
				"the simulator — never at a real key.", forbidden)
		}
	}
}

// rigWorkloads are the manifests the dev rig applies. It is a deliberate, named
// list rather than a glob over infra/deploy: the rig runs a subset, and a glob
// would fail this guard on venue-binance/venue-okx, whose credentials must NEVER
// have a committed dev stand-in. Adding a workload to the rig means adding it here
// and declaring its Secret — which is the review conversation we want to force.
var rigWorkloads = []string{
	"oms-deploy.yaml",
	"tv-sync-deploy.yaml",
	"api-gateway-deploy.yaml",
	"webhook-ingest-deploy.yaml",
	"compliance-deploy.yaml",
}

// tools/rig-apply.sh --deploy must apply EXACTLY rigWorkloads, no more and no
// fewer. The two once disagreed: the guard above modeled the rig as this curated
// subset while apply_deploy globbed every *-deploy.yaml, so the rig came up as the
// whole 23-service estate — venue-binance/venue-okx crash-looping on absent
// exchange keys, an Argo Rollout that never ran, heavy ML services off the loop.
// A curated list in one file and a glob in the other is precisely the kind of
// silent drift this repo exists to make unrepresentable, so this arm pins them to
// each other: change one and the build fails until the other matches.
func TestRigApplyDeploysExactlyRigWorkloads(t *testing.T) {
	repoRoot := filepath.Dir(moduleRoot(t))
	script := readFile(t, filepath.Join(repoRoot, "tools", "rig-apply.sh"))

	block := regexp.MustCompile(`(?s)RIG_WORKLOADS=\((.*?)\)`).FindStringSubmatch(script)
	if block == nil {
		t.Fatal("tools/rig-apply.sh no longer contains a RIG_WORKLOADS=( ... ) array. apply_deploy " +
			"must enumerate the rig's workloads explicitly (not glob infra/deploy), and this guard " +
			"reads that array to prove it matches rigWorkloads. If the array was renamed, teach this " +
			"guard the new name rather than deleting the check.")
	}

	got := map[string]bool{}
	for _, m := range regexp.MustCompile(`\S+-(?:deploy|rollout)\.yaml`).FindAllString(block[1], -1) {
		got[m] = true
	}
	want := map[string]bool{}
	for _, w := range rigWorkloads {
		want[w] = true
	}

	for w := range want {
		if !got[w] {
			t.Errorf("rigWorkloads lists %q but tools/rig-apply.sh does not apply it. The rig would be "+
				"missing a workload its loop needs; add it to RIG_WORKLOADS in apply_deploy.", w)
		}
	}
	for g := range got {
		if !want[g] {
			t.Errorf("tools/rig-apply.sh applies %q but rigWorkloads does not list it. Either the rig is "+
				"deploying something the posture guard never vetted for a dev Secret, or the two drifted; "+
				"reconcile them. (A workload that mounts Vault CSI also needs a declared dev Secret here.)", g)
		}
	}
}

// PRODUCTION MANIFESTS ARE DIGEST-PINNED; THE RIG BUILDS AND LOADS :latest.
//
// release.yml's pin-digests job rewrites every production manifest's image to
// ghcr.io/eighred/<service>@sha256:<digest> — a digest computed from the CI
// build. tools/rig-images.sh builds images from source and `kind load`s them
// as ghcr.io/eighred/<service>:latest; a locally built image can never
// reproduce a CI-computed digest, so a rig applying the manifest unmodified
// asks the kubelet for a reference no local image can satisfy and every pod
// ImagePullBackOffs against a private registry the node has no credential for.
//
// tools/rig_dev_patch.py is supposed to rewrite each of our own images from
// the pinned digest form to the :latest tag the rig actually loaded, so that
// the imagePullPolicy: IfNotPresent it also sets (see
// TestRigDevSecretsShadowEverySecretProviderClassTheRigMounts's neighbor,
// patch_pod_spec) makes the kind-loaded image authoritative.
//
// This test runs the ACTUAL patcher against the ACTUAL rig manifests and
// inspects its stdout — not the script's source — because a guard that reads
// the source and asserts it contains a rewrite would be exactly the "guard
// that was read rather than fired" pattern this repository has already
// catalogued (see the board's "guards that could not fire" row, cited
// elsewhere in this package).
func TestRigDevPatchRewritesDigestPinnedImagesToLocalTags(t *testing.T) {
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		// FAIL, do not skip. tools/rig-apply.sh already hard-requires
		// python3 for this exact script and refuses to proceed without it
		// ("a sed-based substitute would risk silently mangling a manifest");
		// a skip here would make this suite silently assert less than it
		// claims, which is the same class of defect the "157 pkgs / 0 fail"
		// incident already cost this repo credibility over. Install Python 3
		// (kanz-py already requires a Python toolchain) and PyYAML
		// (`pip install pyyaml`) and re-run.
		t.Fatalf("python3 not found on PATH: %v — tools/rig_dev_patch.py needs a real Python "+
			"interpreter with PyYAML installed (pip install pyyaml); this test refuses to skip "+
			"rather than silently assert nothing about the patcher's output", err)
	}

	root := moduleRoot(t)
	repoRoot := filepath.Dir(root)
	patcher := filepath.Join(repoRoot, "tools", "rig_dev_patch.py")

	// Matches any recognized own-registry shape: bare-digest, tag-only, or
	// tag+digest. It intentionally does NOT tell the three apart — the
	// assertion below does that, by checking for "@sha256:" directly, because
	// they have different expected outcomes (see Finding 2): a digest, with
	// or without an accompanying tag, must become :latest; a tag with no
	// digest must pass through byte-identical. Folding all three into one
	// "want :latest" here was the review finding — it silently demanded
	// :latest even for the digest-free, already-tagged shape, contradicting
	// the patcher's own "leave already-tagged alone" contract. Dormant today
	// because no rig manifest carries a tag-only or tag+digest own image (see
	// TestRigDevPatchHandlesSyntheticImageShapes for synthetic coverage of
	// both), but real production images pass through here too, so the
	// classification must stay correct even while unexercised.
	ownImageRe := regexp.MustCompile(
		`^ghcr\.io/eighred/([a-z0-9][a-z0-9._-]*)(?:@sha256:[a-f0-9]{64}|:[A-Za-z0-9._{}-]+(?:@sha256:[a-f0-9]{64})?)$`)

	manifestsProcessed := 0
	imagesInspected := 0
	ownImagesInspected := 0
	thirdPartyImagesInspected := 0

	for _, svc := range rigWorkloads {
		manifestPath := filepath.Join(root, "infra", "deploy", svc)
		origRaw := readFile(t, manifestPath)

		cmd := exec.Command(pythonPath, patcher, manifestPath)
		cmd.Dir = repoRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("python3 tools/rig_dev_patch.py %s: %v\noutput:\n%s", svc, err, string(out))
		}
		patchedRaw := string(out)

		// Coarse, whole-output check: no digest reference of ANY kind may survive
		// the patch, wherever in the document it lives — not just inside a
		// container's image: field. This is the arm that would have caught the
		// unfixed patcher outright: it never touches the image string at all, so
		// every @sha256: in the source manifest survives verbatim into stdout.
		if strings.Contains(patchedRaw, "@sha256:") {
			t.Errorf("%s: patched output still contains a @sha256: digest reference — the rig cannot "+
				"satisfy a digest with a locally built image, so this pod will ImagePullBackOff. "+
				"patched output:\n%s", svc, patchedRaw)
		}

		origImages, err := decodeContainerImages(origRaw)
		if err != nil {
			t.Fatalf("%s: parse original manifest as YAML: %v", svc, err)
		}
		patchedImages, err := decodeContainerImages(patchedRaw)
		if err != nil {
			t.Fatalf("%s: parse patched output as YAML: %v", svc, err)
		}
		if len(origImages) != len(patchedImages) {
			t.Fatalf("%s: original manifest has %d container image(s) but patched output has %d — "+
				"the patch must never add or remove a container", svc, len(origImages), len(patchedImages))
		}
		if len(origImages) == 0 {
			t.Fatalf("%s: found zero container images in the manifest — this test cannot assert "+
				"anything about it; has the manifest's shape changed?", svc)
		}

		manifestsProcessed++
		for i, orig := range origImages {
			patched := patchedImages[i]
			imagesInspected++

			if m := ownImageRe.FindStringSubmatch(orig); m != nil {
				ownImagesInspected++
				name := m[1]
				if strings.Contains(orig, "@sha256:") {
					// Digest-bearing — with or without an accompanying tag. Per
					// OCI reference resolution the digest is authoritative over
					// any tag also present, so a tag+digest reference is exactly
					// as unpullable on the rig as a bare digest and must be
					// rewritten identically (Finding 1's regression case).
					want := "ghcr.io/eighred/" + name + ":latest"
					if patched != want {
						t.Errorf("%s: our own digest-bearing image %q was patched to %q, want %q — "+
							"the rig loaded ghcr.io/eighred/%s:latest via tools/rig-images.sh, and "+
							"imagePullPolicy: IfNotPresent only helps if the requested reference "+
							"actually matches what was kind-loaded", svc, orig, patched, want, name)
					}
				} else {
					// Tag-only, no digest — already what the rig wants, so the
					// patcher's documented contract is to leave it byte-identical
					// (Finding 2's contract).
					if patched != orig {
						t.Errorf("%s: our own already-tagged image %q (no digest) was rewritten to %q "+
							"— the patcher's contract is to pass an already-tagged, digest-free own "+
							"image through unchanged", svc, orig, patched)
					}
				}
				continue
			}

			if strings.HasPrefix(orig, "ghcr.io/eighred/") {
				// Carries our registry prefix but not a shape ownImageRe understands
				// (neither a recognized digest nor a recognized tag form) — this is
				// exactly the "reference shape we do not understand" the patcher
				// itself must fail loudly on rather than silently leave a digest in
				// place. If the patcher passed it through unchanged, catch that here
				// too: the shape is unrecognized either way, so the manifest (or this
				// test's regex) needs to be taught the new shape.
				t.Errorf("%s: image %q carries our registry prefix but neither this test's ownImageRe "+
					"nor (by contract) tools/rig_dev_patch.py should have let it through unrecognized — "+
					"patched value was %q", svc, orig, patched)
				continue
			}

			// Third-party image (gcr.io/distroless/..., ghcr.io/spiffe/..., etc.) —
			// the rig pulls these normally; the patcher must leave them byte-identical.
			thirdPartyImagesInspected++
			if patched != orig {
				t.Errorf("%s: third-party image %q was rewritten to %q — the patcher must only "+
					"touch references to our own registry (ghcr.io/eighred/*); rewriting a "+
					"third-party reference is worse than the bug this patch exists to fix, since "+
					"the rig has no local build of it to fall back on", svc, orig, patched)
			}
		}
	}

	// Non-vacuity. A test that iterated an empty rigWorkloads set, or found zero
	// images across every manifest it did walk, would pass having checked
	// nothing — exactly the failure mode this repository's board already
	// catalogues under "guards that could not fire."
	if manifestsProcessed == 0 {
		t.Fatal("processed zero manifests — rigWorkloads is empty or every iteration failed before " +
			"incrementing the counter; this test would otherwise pass vacuously")
	}
	if imagesInspected == 0 {
		t.Fatal("inspected zero container images across every rig manifest — this test would " +
			"otherwise pass vacuously")
	}
	if ownImagesInspected == 0 {
		t.Fatal("found zero of our own (ghcr.io/eighred/*) images across every rig manifest — the " +
			"digest-to-:latest rewrite this test exists to verify was never exercised")
	}
	if thirdPartyImagesInspected == 0 {
		// Not a failure — an honest statement. None of the five rig workload
		// manifests (oms, tv-sync, api-gateway, webhook-ingest, compliance) carry
		// a third-party image: field; their only non-ghcr.io/eighred reference is
		// the csi.spiffe.io CSI *driver* on the spiffe volume, which is not an
		// image and patch_pod_spec deliberately leaves it alone (see this file's
		// TestRigDevSecretsShadowEverySecretProviderClassTheRigMounts doc comment
		// on csi.spiffe.io volumes). The over-rewrite arm above is still real code
		// that would fire the moment a rig manifest gains one.
		t.Log("no third-party container image found in any rig workload manifest — the " +
			"over-rewrite arm above never had a case to exercise this run; it remains live for " +
			"the first rig manifest that adds one")
	}
}

// TestRigDevPatchHandlesSyntheticImageShapes exercises reference shapes that
// no rig workload manifest currently carries, so
// TestRigDevPatchRewritesDigestPinnedImagesToLocalTags above never exercises
// them:
//
//   - a tag+digest own image. Per OCI reference resolution, when a reference
//     carries both a tag and a digest, the digest is authoritative for the
//     pull — the tag is cosmetic. A locally `kind load`ed :latest image can
//     no more satisfy this than it can a bare digest, so it must be rewritten
//     identically to the bare-digest case (see _rig_image's docstring in
//     tools/rig_dev_patch.py).
//   - a tag-only own image (no digest), which must pass through
//     byte-identical — the patcher's "already tagged" contract.
//   - a third-party image, which must also pass through byte-identical. None
//     of the five real rig manifests carry one today (see that test's t.Log),
//     so this is the only place the over-rewrite guard actually fires.
//   - an own-registry reference in a shape the patcher does not recognize
//     (here, a nested repository path), which must FATAL rather than silently
//     pass a possibly-unpullable reference through — naming the offending
//     reference in stderr.
//
// This writes a synthetic manifest to t.TempDir() and runs the ACTUAL patcher
// against it and inspects its stdout/stderr, for the same reason the
// real-manifest test above does: a guard that reads the source rather than
// running it is not a guard that fired.
func TestRigDevPatchHandlesSyntheticImageShapes(t *testing.T) {
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 not found on PATH: %v — see the identical requirement in "+
			"TestRigDevPatchRewritesDigestPinnedImagesToLocalTags", err)
	}

	repoRoot := filepath.Dir(moduleRoot(t))
	patcher := filepath.Join(repoRoot, "tools", "rig_dev_patch.py")

	digestA := strings.Repeat("a1", 32)
	digestB := strings.Repeat("b2", 32)

	t.Run("OwnRegistryShapes", func(t *testing.T) {
		manifest := "apiVersion: apps/v1\n" +
			"kind: Deployment\n" +
			"metadata:\n" +
			"  name: synthetic\n" +
			"  namespace: kanz-services\n" +
			"spec:\n" +
			"  template:\n" +
			"    spec:\n" +
			"      containers:\n" +
			"        - name: bare-digest\n" +
			"          image: ghcr.io/eighred/svc-a@sha256:" + digestA + "\n" +
			"        - name: tag-plus-digest\n" +
			"          image: ghcr.io/eighred/svc-b:v1.2.3@sha256:" + digestB + "\n" +
			"        - name: tag-only\n" +
			"          image: ghcr.io/eighred/svc-c:v1.2.3\n" +
			"        - name: third-party\n" +
			"          image: gcr.io/distroless/static:nonroot\n"

		path := filepath.Join(t.TempDir(), "synthetic-deploy.yaml")
		if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
			t.Fatalf("write synthetic manifest: %v", err)
		}

		cmd := exec.Command(pythonPath, patcher, path)
		cmd.Dir = repoRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("python3 tools/rig_dev_patch.py %s: %v\noutput:\n%s", path, err, string(out))
		}

		images, err := decodeContainerImages(string(out))
		if err != nil {
			t.Fatalf("parse patched output as YAML: %v\noutput:\n%s", err, string(out))
		}
		if len(images) != 4 {
			t.Fatalf("expected 4 container images in patched output, got %d:\n%s", len(images), string(out))
		}
		bareDigest, tagPlusDigest, tagOnly, thirdParty := images[0], images[1], images[2], images[3]

		if want := "ghcr.io/eighred/svc-a:latest"; bareDigest != want {
			t.Errorf("bare-digest image: got %q, want %q", bareDigest, want)
		}

		// Finding 1's regression case: a tag+digest reference must be rewritten
		// exactly like the bare-digest case. The digest, not the tag, is what
		// the registry actually resolves, so a tag does not make this
		// reference any more pullable on the rig than the bare digest is;
		// leaving it in place silently reproduces the exact ImagePullBackOff
		// this patch exists to eliminate.
		if want := "ghcr.io/eighred/svc-b:latest"; tagPlusDigest != want {
			t.Errorf("tag+digest image: got %q, want %q — a tag does not make a "+
				"digest-bearing reference pullable; the digest is authoritative "+
				"over any accompanying tag per OCI reference resolution",
				tagPlusDigest, want)
		}

		// Finding 2's contract: a tag with NO digest is already what the rig
		// wants and must pass through byte-identical.
		if want := "ghcr.io/eighred/svc-c:v1.2.3"; tagOnly != want {
			t.Errorf("tag-only image: got %q, want %q (byte-identical passthrough)", tagOnly, want)
		}

		if want := "gcr.io/distroless/static:nonroot"; thirdParty != want {
			t.Errorf("third-party image: got %q, want %q (byte-identical passthrough)", thirdParty, want)
		}
	})

	t.Run("FailLoudOnUnrecognizedOwnRegistryShape", func(t *testing.T) {
		// A nested repository path: carries OWN_REGISTRY_PREFIX but the
		// remainder ("team/svc@sha256:...") is neither the bare-digest nor the
		// tag-only shape _rig_image recognizes (both require a single
		// slash-free service segment). This must FATAL, not silently pass a
		// digest-bearing reference through.
		badRef := "ghcr.io/eighred/team/svc@sha256:" + digestA
		manifest := "apiVersion: apps/v1\n" +
			"kind: Deployment\n" +
			"metadata:\n" +
			"  name: synthetic-bad\n" +
			"  namespace: kanz-services\n" +
			"spec:\n" +
			"  template:\n" +
			"    spec:\n" +
			"      containers:\n" +
			"        - name: nested-path\n" +
			"          image: " + badRef + "\n"

		path := filepath.Join(t.TempDir(), "synthetic-bad-deploy.yaml")
		if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
			t.Fatalf("write synthetic manifest: %v", err)
		}

		cmd := exec.Command(pythonPath, patcher, path)
		cmd.Dir = repoRoot
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected python3 tools/rig_dev_patch.py to exit non-zero on an "+
				"unrecognized own-registry reference shape (nested repository path), "+
				"but it exited 0. output:\n%s", string(out))
		}
		if !strings.Contains(string(out), "FATAL") {
			t.Errorf("expected FATAL in output, got:\n%s", string(out))
		}
		if !strings.Contains(string(out), badRef) {
			t.Errorf("expected the offending reference %q to be named in the FATAL "+
				"output, got:\n%s", badRef, string(out))
		}
	})
}

// decodeContainerImages walks every YAML document in raw and returns every
// container/initContainer image string it finds, in document order and then
// containers-before-initContainers within a document. It reuses pullSecretDoc
// (declared in supplychain_test.go, same package) rather than a second
// hand-rolled struct shaped identically to it — the two are the same
// Deployment/StatefulSet/DaemonSet/Job pod-template shape, just read for a
// different purpose.
func decodeContainerImages(raw string) ([]string, error) {
	var images []string
	dec := yaml.NewDecoder(strings.NewReader(raw))
	for {
		var doc pullSecretDoc
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		ps := doc.Spec.Template.Spec
		for _, c := range ps.Containers {
			images = append(images, c.Image)
		}
		for _, c := range ps.InitContainers {
			images = append(images, c.Image)
		}
	}
	return images, nil
}
