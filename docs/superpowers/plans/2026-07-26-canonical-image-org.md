# Canonical Image Org Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make CI publish images to the same registry namespace the cluster pulls them from, and make the split unrepresentable rather than merely fixed.

**Architecture:** One canonical org constant in the arch tests. A guard walks both `.github/workflows/` and `kanz/infra/`, reads image-reference positions only, and fails if any Kanz image is published or pulled under a different org — including a computed one. Two workflow lines change from `${{ github.repository_owner }}` to the literal org.

**Tech Stack:** GitHub Actions YAML, Go arch tests (`kanz/test/arch/`, reusing `workflowFiles` and `moduleRoot`).

## The problem, as verified

| what | where | resolves to |
|---|---|---|
| `IMAGE: ghcr.io/${{ github.repository_owner }}/${{ matrix.service }}` | `.github/workflows/release.yml:31` | `ghcr.io/eighred/*` |
| `images: ghcr.io/${{ github.repository_owner }}/${{ matrix.service }}` | `.github/workflows/build.yml:192` | `ghcr.io/eighred/*` |
| 39 manifest `image:` refs, `preview.yml:56`, the `ClusterImagePolicy` glob and its `subjectRegExp` | `kanz/infra/`, `.github/workflows/preview.yml` | `ghcr.io/kanz-eng/*` |
| `gh repo view` | origin | `eighred/kanz`, owner `eighred` |

CI publishes to one namespace and the cluster pulls from another. They never meet. 747 files hardcode `kanz-eng`, including the Go module path (`module github.com/kanz-eng/kanz`); exactly two places compute the owner, and they are the two image-push steps.

**Owner decision (2026-07-26): `kanz-eng` is canonical. Fix the two workflows.**

Reference census across `kanz/infra/`, `.github/workflows/` and `kanz/test/`: `kanz-eng` 50, `spiffe` 17, `gitleaks` 2, computed `${{ … }}` 2, plus one `ghcr.io/token` occurring inside a quoted error message in a comment at `kanz/infra/deploy/operator-deploy.yaml:189`.

## Global Constraints

- The canonical org is exactly `kanz-eng`.
- Third-party ghcr orgs `spiffe` and `gitleaks` are legitimate and must not be flagged. Carry them as a named allowlist with a reason each.
- The guard must read **image-reference positions only** (`image:` in manifests; `images:`, `IMAGE:`, `tags:` in workflows) — never free text. A comment at `kanz/infra/deploy/operator-deploy.yaml:189` quotes the string `403 Forbidden from ghcr.io/token`, and a text-scanning guard would trip on it.
- Do **not** wire a `write:packages` credential for `kanz-eng`. It cannot be tested from here and is boarded as an OPS-M1 prerequisite instead.
- Do not change the Go module path.
- All Go work happens inside `kanz/`. Use `GOFLAGS=-mod=mod`.
- No cluster and no CI is reachable. Nothing may claim a published image, a green workflow run, or a successful push.

---

### Task 1: The guard, and the two lines it demands

**Files:**
- Modify: `kanz/test/arch/supplychain_test.go` (append a test + its constants)
- Modify: `.github/workflows/build.yml:192`
- Modify: `.github/workflows/release.yml:31`

**Interfaces:**
- Consumes: `moduleRoot(t *testing.T) string` (`kanz/test/arch/risk_boundary_test.go:121`) and `workflowFiles(t *testing.T, repoRoot string) []string` (`kanz/test/arch/supplychain_test.go:143`). Both are package-shared and already used by `TestWorkflowActionsArePinnedToSHA`.
- Produces: nothing other tasks import.

- [ ] **Step 1: Write the failing guard**

Append to `kanz/test/arch/supplychain_test.go`:

```go
// CI PUBLISHED TO ONE NAMESPACE AND THE CLUSTER PULLED FROM ANOTHER.
//
// Every manifest, preview.yml, and the sigstore ClusterImagePolicy (both its glob
// and its keyless subjectRegExp) name ghcr.io/kanz-eng. The two steps that actually
// PUSH images computed their target from ${{ github.repository_owner }}, which for
// this repository resolves to "eighred". So a green release.yml would have published
// images the estate could never pull, under an identity the admission policy would
// never accept — and nothing would have said so until someone tried.
//
// The owner's decision (2026-07-26) is that kanz-eng is canonical. This test is what
// keeps the two halves from drifting apart again: publish targets and pull references
// are the same string or the build fails.
//
// WHY IT READS POSITIONS, NOT TEXT. A comment at infra/deploy/operator-deploy.yaml
// quotes the error string "403 Forbidden from ghcr.io/token". A guard that grepped
// free text would flag that comment as an image reference under an org called
// "token". So this walks YAML keys that actually carry image references.
const canonicalImageOrg = "kanz-eng"

// thirdPartyImageOrgs are ghcr namespaces we legitimately consume but do not own.
// Each entry needs a reason: an unexplained exemption is how a guard rots into
// a list of things somebody once wanted to skip.
var thirdPartyImageOrgs = map[string]string{
	"spiffe":   "SPIRE server/agent, the CSI driver and spiffe-helper — upstream sigstore/spiffe images",
	"gitleaks": "the secret-scanning image security.yml runs; not built here",
}

// imageRefKeys are the YAML keys whose values carry an image reference.
// `IMAGE` is release.yml's job-level env var; `images`/`tags` are
// docker/metadata-action and build-push-action inputs; `image` is Kubernetes.
var imageRefKeys = []string{"image", "images", "tags", "IMAGE"}

func TestImageOrgIsCanonical(t *testing.T) {
	root := moduleRoot(t)
	repoRoot := filepath.Dir(root)

	// key: value may be "ghcr.io/org/name:tag" or a multi-line block; match each
	// ghcr.io reference inside the value.
	keyRe := regexp.MustCompile(`(?m)^\s*(?:-\s*)?(` + strings.Join(imageRefKeys, "|") + `)\s*:\s*(.*)$`)
	ghcrRe := regexp.MustCompile(`ghcr\.io/([A-Za-z0-9._\-]+|\$\{\{[^}]*\}\})/`)

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

	var problems []string
	checked := 0
	for _, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		rel, _ := filepath.Rel(repoRoot, path)
		relSlash := filepath.ToSlash(rel)
		for _, m := range keyRe.FindAllStringSubmatch(string(body), -1) {
			for _, g := range ghcrRe.FindAllStringSubmatch(m[2], -1) {
				org := g[1]
				checked++
				if org == canonicalImageOrg {
					continue
				}
				if reason, ok := thirdPartyImageOrgs[org]; ok {
					_ = reason
					continue
				}
				if strings.HasPrefix(org, "${{") {
					problems = append(problems, fmt.Sprintf(
						"%s: %s: names a COMPUTED image org %s — it resolves to the GitHub "+
							"repository owner, not to %q, so images published here cannot be pulled "+
							"by manifests that name %q", relSlash, m[1], org, canonicalImageOrg, canonicalImageOrg))
					continue
				}
				problems = append(problems, fmt.Sprintf(
					"%s: %s: image org %q is neither the canonical %q nor a listed third party",
					relSlash, m[1], org, canonicalImageOrg))
			}
		}
	}

	if checked < 40 {
		t.Fatalf("only %d ghcr.io image references matched across workflows and infra/ — "+
			"expected at least 40; the key set or layout changed and this test is now "+
			"asserting almost nothing", checked)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("image org disagreement (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
}
```

`fmt`, `io/fs`, `os`, `path/filepath`, `regexp`, `sort`, `strings`, `testing` are already imported by this file after the previous branch's additions. Verify rather than assume.

- [ ] **Step 2: Run it and confirm it fails for the right reason**

```bash
cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run TestImageOrgIsCanonical -v
```

Expected: **FAIL** with exactly two problems, one naming `.github/workflows/build.yml` and one naming `.github/workflows/release.yml`, each reporting a COMPUTED image org.

If it instead fails with `only N ghcr.io image references matched`, the key regex is wrong — fix that first, because the guard would otherwise pass by looking at almost nothing once the two lines are changed.

- [ ] **Step 3: Fix the two workflow lines**

`.github/workflows/release.yml:31` — change:

```yaml
      IMAGE: ghcr.io/${{ github.repository_owner }}/${{ matrix.service }}
```

to:

```yaml
      # kanz-eng is canonical and is NOT github.repository_owner: this repository
      # lives at eighred/kanz, so the computed owner published images no manifest
      # references and the ClusterImagePolicy's subjectRegExp would never accept.
      # Guarded by TestImageOrgIsCanonical.
      IMAGE: ghcr.io/kanz-eng/${{ matrix.service }}
```

`.github/workflows/build.yml:192` — change:

```yaml
          images: ghcr.io/${{ github.repository_owner }}/${{ matrix.service }}
```

to:

```yaml
          # Canonical org, not the computed owner — see release.yml's IMAGE comment.
          # Guarded by TestImageOrgIsCanonical.
          images: ghcr.io/kanz-eng/${{ matrix.service }}
```

Change nothing else in either workflow. In particular do **not** touch the `docker/login-action` steps: pushing to `kanz-eng` needs a `write:packages` credential that `secrets.GITHUB_TOKEN` cannot provide, and that is deliberately boarded as an OPS-M1 prerequisite rather than written blind here.

- [ ] **Step 4: Run the guard and confirm it passes**

```bash
cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run TestImageOrgIsCanonical -v
```

Expected: **PASS**.

- [ ] **Step 5: Prove the guard is not vacuous**

Change `.github/workflows/release.yml`'s `IMAGE` back to `ghcr.io/${{ github.repository_owner }}/${{ matrix.service }}`, then:

```bash
cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run TestImageOrgIsCanonical 2>&1 | head -10
```

Expected: **FAIL** naming `.github/workflows/release.yml` and the computed org. Restore and re-run to confirm PASS. Put both outputs in the commit body.

Then do the same with a manifest: change one `image: ghcr.io/kanz-eng/oms:latest` in `kanz/infra/deploy/oms-deploy.yaml` to `ghcr.io/eighred/oms:latest`, confirm the guard names that file, and restore. This proves the guard polices the pull side as well as the push side — the whole point of a single constant.

- [ ] **Step 6: Run the full arch suite**

```bash
cd kanz && GOFLAGS=-mod=mod go test ./test/arch/
```

Expected: **PASS**. `TestWorkflowActionsArePinnedToSHA` shares `workflowFiles` with the new test — confirm it is unaffected.

- [ ] **Step 7: Commit**

```bash
git add kanz/test/arch/supplychain_test.go .github/workflows/build.yml .github/workflows/release.yml
git commit -m "fix(ci): publish images to the namespace the cluster actually pulls from

build.yml and release.yml computed their push target from
github.repository_owner, which resolves to eighred for this repository. Every
manifest, preview.yml and the sigstore ClusterImagePolicy — glob and keyless
subjectRegExp both — name kanz-eng. So a green release.yml would have published
images the estate could never pull, signed under an identity admission would
never accept, and nothing would have said so until someone tried.

TestImageOrgIsCanonical reads image-reference positions (not free text, because
a comment quotes '403 Forbidden from ghcr.io/token') across workflows and infra,
and fails on any Kanz image published or pulled under a non-canonical or
computed org. Shown red on both the push side and the pull side.

Pushing to kanz-eng needs a write:packages credential GITHUB_TOKEN cannot
provide. That is boarded as an OPS-M1 prerequisite, not written blind here."
```

---

### Task 2: Board what this found and what it did not fix

**Files:**
- Modify: `KANZ_TASKS.md`

**Interfaces:**
- Consumes: nothing.
- Produces: nothing.

- [ ] **Step 1: Add the prerequisite to OPS-M1**

The OPS-M1 row currently says `release.yml` has never executed and lists two things to know before firing it. Add a third, in the row's existing voice: **pushing to `ghcr.io/kanz-eng/*` requires a `write:packages` credential for that namespace, and `secrets.GITHUB_TOKEN` cannot provide one** — it is scoped to `eighred/kanz` and can only publish packages owned by `eighred`. Both `build.yml` and `release.yml` now name `kanz-eng` explicitly (guarded by `TestImageOrgIsCanonical`), so the first dispatch fails on authentication unless that secret exists. State that this was found by reading, not by a run, and that whether the `eighred` account can hold such a credential for `kanz-eng` is unverified from this box.

- [ ] **Step 2: Record the release-matrix gap on OPS-M4a**

`release.yml`'s matrix is `[risk-engine, schema-registry]` — two services. `build.yml`'s matrix covers 25. Add to the OPS-M4a row that even a green OPS-M1 leaves 23 of 25 services with no signed release and therefore no digest to pin, so OPS-M4a's scope is gated on the release matrix reaching parity with the build matrix — and that this is a second, independent reason M4a cannot complete on OPS-M1 alone.

- [ ] **Step 3: Add the canonical-org row to DONE-adjacent context**

Do **not** create a new TODO row for the org fix — it is done. Instead add one sentence to the OPS-M4a row recording that the registry namespace disagreement was found and fixed on 2026-07-26 (CI computed `eighred`, everything else named `kanz-eng`), with `TestImageOrgIsCanonical` now preventing recurrence.

- [ ] **Step 4: Validate the board**

```bash
sh tools/validate-board.sh KANZ_TASKS.md; echo "exit=$?"
```

Expected: `board OK: 62 rows, 5 columns each` and `exit=0`. Check the exit code, not the text. If non-zero, a literal `|` leaked into prose — find it and remove it.

- [ ] **Step 5: Commit**

```bash
git add KANZ_TASKS.md
git commit -m "docs(board): the registry namespace disagreement, and the two things it blocks

OPS-M1 gains the prerequisite this found: pushing to ghcr.io/kanz-eng needs a
write:packages credential for that namespace, and GITHUB_TOKEN cannot provide
one. Found by reading, not by a run.

OPS-M4a gains the second reason it cannot complete on OPS-M1 alone: release.yml
builds 2 services where build.yml builds 25, so 23 have no signed release and
therefore no digest to pin."
```

---

## Definition of done for this plan

- `cd kanz && GOFLAGS=-mod=mod go test ./test/arch/` passes.
- `sh tools/validate-board.sh KANZ_TASKS.md` exits 0.
- `TestImageOrgIsCanonical` has been observed **red on both sides** — a workflow push target and a manifest pull reference — and green after each restore.
- No `write:packages` credential wiring was added.
- No claim anywhere that an image was published, a workflow ran, or a push succeeded.
