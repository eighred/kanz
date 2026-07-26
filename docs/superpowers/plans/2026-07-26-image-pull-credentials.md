# OPS-M2f-b — Image Pull Credentials Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every pod that runs a private `ghcr.io/kanz-eng/*` image a credential to pull it, so a node joined through the TUI is immediately usable instead of failing `ErrImagePull`.

**Architecture:** A `kubernetes.io/dockerconfigjson` Secret named `ghcr-pull` in each of the three namespaces that run private images, attached via `imagePullSecrets` on the 26 ServiceAccounts that own those pods — never on pod specs. The credential lives in the cluster's secret plane, so rotation touches no node and nodes joined by any path are covered. An arch guard fails the build if any of those ServiceAccounts loses its attachment.

**Tech Stack:** Kubernetes manifests (`kanz/infra/`), Go 1.x arch tests (`kanz/test/arch/`, `gopkg.in/yaml.v3`), Go unit tests with `k8s.io/client-go/kubernetes/fake`.

**Spec:** `docs/superpowers/specs/2026-07-26-image-pull-credentials-design.md`

## Global Constraints

- The Secret name is exactly `ghcr-pull` in every namespace. No per-service variants.
- Attach at the **ServiceAccount**, never at a pod spec. The two operator-built Job specs inherit via `ServiceAccountName: "kanz-node-provisioner"` and must need **no Go change**.
- Three namespaces only: `kanz-services` (22 SAs), `kanz-operator` (3 SAs: `operator`, `kanz-halt`, `kanz-node-provisioner`), `kanz-messaging` (1 SA: `nats-rebuild`).
- Do **not** commit any credential value. Manifests reference the Secret by name; the Secret itself is created out-of-band.
- Do **not** build a TUI rotation RPC. Explicitly out of scope (see spec).
- Do **not** wire the sigstore policy-controller credential. Explicitly out of scope (see spec).
- All Go work happens inside the `kanz/` module. Per the project's build memory, use `GOFLAGS=-mod=mod`.
- The live cluster is **not reachable** from this box. No step may claim the runtime proof.

---

### Task 1: The guard, and the 26 attachments it demands

**Files:**
- Modify: `kanz/test/arch/supplychain_test.go` (append a new test + its doc type)
- Modify: 22 files under `kanz/infra/deploy/` (the `kanz-services` ServiceAccounts)
- Modify: `kanz/infra/deploy/operator-deploy.yaml` (SAs `operator`, `kanz-node-provisioner`)
- Modify: `kanz/infra/operator/halt-job.yaml` (SA `kanz-halt`)
- Modify: `kanz/infra/dr/nats/rebuild-job.yaml` (SA `nats-rebuild`)

**Interfaces:**
- Consumes: `moduleRoot(t *testing.T) string` — already defined in `kanz/test/arch/risk_boundary_test.go:121`, package-shared.
- Produces: nothing other tasks import. Task 2 and Task 3 are independent of this task's symbols.

- [ ] **Step 1: Write the failing guard**

Append to `kanz/test/arch/supplychain_test.go`:

```go
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
```

Add `"errors"`, `"fmt"`, `"io"` and `"gopkg.in/yaml.v3"` to the file's import block if not already present (`io/fs`, `os`, `path/filepath`, `sort`, `strings`, `testing` are already imported at the top of `supplychain_test.go`).

- [ ] **Step 2: Run it and confirm it fails for the right reason**

```bash
cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run TestPrivateImagesHavePullSecrets -v
```

Expected: **FAIL**, listing ~25 problems, each of the form
`infra/deploy/oms-deploy.yaml: Deployment/oms runs a private image under ServiceAccount "oms" (namespace "kanz-services"), which does not carry imagePullSecrets…`

If it fails with `found 0 pod-bearing manifests` instead, the walk or the decode is wrong — fix that before proceeding, because the guard would otherwise pass vacuously once the manifests are edited.

- [ ] **Step 3: Attach the pull secret to all 26 ServiceAccounts**

For every ServiceAccount named in the failure list, add a top-level `imagePullSecrets` block. The ServiceAccount document already looks like this:

```yaml
kind: ServiceAccount
metadata:
  name: oms
  namespace: kanz-services
  labels:
    app.kubernetes.io/part-of: kanz
```

Make it look like this — `imagePullSecrets` is a sibling of `metadata`, not a child of it:

```yaml
kind: ServiceAccount
metadata:
  name: oms
  namespace: kanz-services
  labels:
    app.kubernetes.io/part-of: kanz
imagePullSecrets:
  - name: ghcr-pull
```

The 26 ServiceAccounts, by file:

- `kanz/infra/deploy/` — one each in `accounting-deploy.yaml`, `alternatives-deploy.yaml`, `api-gateway-deploy.yaml`, `archiver-deploy.yaml`, `audit-deploy.yaml`, `compliance-deploy.yaml`, `copilot-deploy.yaml`, `datamaster-deploy.yaml`, `inference-deploy.yaml`, `lake-sink-deploy.yaml`, `market-data-deploy.yaml`, `market-ingest-deploy.yaml`, `oms-deploy.yaml`, `oms-acme.yaml`, `regulatory-deploy.yaml`, `risk-engine-rollout.yaml`, `schema-registry-deploy.yaml`, `tv-sync-deploy.yaml`, `venue-binance-deploy.yaml`, `venue-okx-deploy.yaml`, `wealth-deploy.yaml`, `webhook-ingest-deploy.yaml`
- `kanz/infra/deploy/operator-deploy.yaml` — SAs `operator` **and** `kanz-node-provisioner` (two documents in one file; both need it — the provisioning and probe Jobs run under `kanz-node-provisioner`)
- `kanz/infra/operator/halt-job.yaml` — SA `kanz-halt`
- `kanz/infra/dr/nats/rebuild-job.yaml` — SA `nats-rebuild`

Do not touch `kanz-node-provisioner`'s `automountServiceAccountToken: false`. It stays.

- [ ] **Step 4: Run the guard and confirm it passes**

```bash
cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run TestPrivateImagesHavePullSecrets -v
```

Expected: **PASS**.

- [ ] **Step 5: Prove the guard is not vacuous**

Remove the `imagePullSecrets` block you just added to `kanz/infra/deploy/oms-deploy.yaml`, then:

```bash
cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ -run TestPrivateImagesHavePullSecrets 2>&1 | head -20
```

Expected: **FAIL** naming `infra/deploy/oms-deploy.yaml` and ServiceAccount `"oms"` specifically. Restore the block and re-run to confirm PASS. Paste both outputs into the commit body — a guard nobody has seen red is a guard nobody has proven works.

- [ ] **Step 6: Run the whole arch suite for regressions**

```bash
cd kanz && GOFLAGS=-mod=mod go test ./test/arch/
```

Expected: **PASS**. In particular `TestProvisionerServiceAccountHasNoToken` and `TestProvisioningJobsArePinnedToControlPlane` must still pass — the first because `imagePullSecrets` and `automountServiceAccountToken` are unrelated fields, the second because nothing in this task touches placement.

- [ ] **Step 7: Commit**

```bash
git add kanz/test/arch/supplychain_test.go kanz/infra/
git commit -m "feat(infra): give every private-image pod a credential to pull with

Attaches a ghcr-pull dockerconfigjson Secret at the 26 ServiceAccounts that
own pods running ghcr.io/kanz-eng/*, across kanz-services, kanz-operator and
kanz-messaging. At the ServiceAccount and not the pod spec, so the two
Go-built provisioning Jobs inherit it through ServiceAccountName with no code
change, and the credential attaches at 26 points rather than 39.

TestPrivateImagesHavePullSecrets is the enforcement, with a workload floor so
it cannot pass by matching nothing. Shown red by removing one attachment and
green again after restoring it."
```

---

### Task 2: Retire the justification this change just killed

**Files:**
- Modify: `kanz/services/operator/internal/provision/provision.go:65-71` (comment only)
- Modify: `kanz/services/operator/internal/provision/provision_test.go` (append one test)

**Interfaces:**
- Consumes: `New(cs kubernetes.Interface, c Config) *Provisioner`, `(*Provisioner).AddNode(ctx, Request) (string, error)`, and the package-local `cfg()` helper — all already used by `TestAddNodeSetsImagePullPolicyWhenConfigured` at `provision_test.go:132`.
- Produces: nothing other tasks import.

**Why this task exists.** `provision.go` gives two reasons for pinning the provisioning and probe Jobs to the control plane. Task 1 deletes the second one. A comment that defends a security trade-off on a premise that is no longer true is dated evidence wearing the costume of current truth, and this repository has already paid for that once.

- [ ] **Step 1: Write the failing test**

Append to `kanz/services/operator/internal/provision/provision_test.go`:

```go
// TestAddNodeJobRunsUnderProvisionerServiceAccount pins the inheritance that
// OPS-M2f-b relies on. The ghcr-pull credential is attached to the
// kanz-node-provisioner ServiceAccount in infra/deploy/operator-deploy.yaml,
// NOT to this Job's pod spec. So the Job can pull its private image only for
// as long as it keeps naming that ServiceAccount. Renaming it here — or
// dropping it and falling back to the namespace default SA — silently
// reintroduces the ErrImagePull this milestone exists to remove, on a code
// path that has no manifest a reviewer would think to check.
func TestAddNodeJobRunsUnderProvisionerServiceAccount(t *testing.T) {
	cs := fake.NewSimpleClientset()
	id, err := New(cs, cfg()).AddNode(context.Background(), Request{
		Hostname: "london", IP: "10.0.0.5", SSHPort: 22, SSHUser: "root", SSHKey: []byte("PEM")})
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	job, err := cs.BatchV1().Jobs("kanz-operator").Get(context.Background(), id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("job not created: %v", err)
	}
	if got := job.Spec.Template.Spec.ServiceAccountName; got != "kanz-node-provisioner" {
		t.Errorf("provisioning Job ServiceAccountName = %q, want %q — the ghcr-pull "+
			"credential is attached to that ServiceAccount, so any other value means "+
			"this Job cannot pull its own image", got, "kanz-node-provisioner")
	}
}
```

- [ ] **Step 2: Run it**

```bash
cd kanz && GOFLAGS=-mod=mod go test ./services/operator/internal/provision/ -run TestAddNodeJobRunsUnderProvisionerServiceAccount -v
```

Expected: **PASS** immediately. This test characterises behaviour that already exists (`provision.go:247`); it is a regression pin, not a red-green cycle. Confirm it is not vacuous by temporarily changing `provision.go:247` to `ServiceAccountName: "default"`, re-running to see it FAIL with the named message, then reverting.

- [ ] **Step 3: Correct the comment**

In `kanz/services/operator/internal/provision/provision.go`, replace the second-reason paragraph (currently beginning `// The second reason is that an unpinned Job does not reliably run at all.` and ending `// entirely by where the pod ran.`) with:

```go
// THE SECOND REASON IS RETIRED, AND IS RECORDED HERE BECAUSE ITS ABSENCE IS THE
// POINT. Until OPS-M2f-b this Job also had to be pinned because the estate held no
// registry credentials at all: ghcr is private, nothing in infra/ carried
// imagePullSecrets, and a Job landing on a node that had not pre-loaded the
// kanz-provisioner image died in ErrImagePull. Observed live — a probe Job landed on
// a freshly joined worker, 403'd, and reported a healthy host unreachable after the
// full 60s wait, a false verdict about someone's node caused entirely by where the
// pod ran. That reason is gone: the kanz-node-provisioner ServiceAccount now carries
// the ghcr-pull credential, and this Job inherits it (pinned by
// TestAddNodeJobRunsUnderProvisionerServiceAccount). So credential confinement above
// is now the ONLY thing holding this pin — which is exactly what OPS-M2f-a has to
// replace, and it should not have to rediscover that the other half already expired.
```

- [ ] **Step 4: Run the package tests and the placement guards**

```bash
cd kanz && GOFLAGS=-mod=mod go test ./services/operator/... ./test/arch/
```

Expected: **PASS**. `TestProvisioningJobsArePinnedToControlPlane` parses this source file — confirm the comment edit did not disturb what it greps for.

- [ ] **Step 5: Commit**

```bash
git add kanz/services/operator/internal/provision/provision.go kanz/services/operator/internal/provision/provision_test.go
git commit -m "fix(operator): retire the pin justification OPS-M2f-b just made false

provision.go gave two reasons for pinning the provisioning and probe Jobs to
the control plane. The second — that the estate held no registry credentials,
so an unpinned Job died in ErrImagePull — stopped being true the moment the
kanz-node-provisioner ServiceAccount got the ghcr-pull credential. Credential
confinement is now the only load-bearing reason, which is what OPS-M2f-a has
to replace.

Pins the inheritance the credential now depends on: the Job carries no
imagePullSecrets of its own, so renaming its ServiceAccount would silently
reintroduce the failure with no manifest a reviewer would think to check."
```

---

### Task 3: Bootstrap, memory, and the board

**Files:**
- Modify: `DEMO_DEPLOYMENT.md` (new subsection after §3)
- Modify: `KANZ_BRAIN.md` (one bullet under "Security & multi-tenancy")
- Modify: `KANZ_TASKS.md` (OPS-M2f-b row; add the deferred rotation row)

**Interfaces:**
- Consumes: nothing.
- Produces: nothing.

- [ ] **Step 1: Document the bootstrap step**

Add to `DEMO_DEPLOYMENT.md`, immediately after §3 ("Build and load the images"), a new subsection:

````markdown
### 3b. The registry pull credential (bootstrap — once per cluster)

The platform images are private (`ghcr.io/kanz-eng/*`, 403 to anonymous pulls) and
every workload's ServiceAccount references a `ghcr-pull` Secret. That Secret cannot
be created from inside the cluster, because the operator that would create it runs a
private image itself. So this is a genuine bootstrap step, not a workaround, and it
is the only one:

```
for ns in kanz-services kanz-operator kanz-messaging; do
  kubectl create secret docker-registry ghcr-pull \
    --namespace "$ns" \
    --docker-server=ghcr.io \
    --docker-username="$GHCR_BOT_USER" \
    --docker-password="$GHCR_BOT_PAT" \
    --dry-run=client -o yaml | kubectl apply -f -
done
```

`GHCR_BOT_PAT` is a machine-account fine-grained PAT scoped to `read:packages` on
`kanz-eng` only — never a personal token, because the estate's ability to pull images
must not depend on one person's account surviving.

Rotation is the same command with a new token, and touches no node. If pods start
reporting `ErrImagePull` on `ghcr.io/kanz-eng/*` across the estate, this Secret
expiring is the first thing to check.

**On the dev rig this Secret is inert and that is expected.** `tools/rig_dev_patch.py`
rewrites `imagePullPolicy` to `IfNotPresent` because the rig runs `kind load`ed local
builds with no registry access, so nothing ever pulls. The patch mutates pod specs in
place and does not touch `imagePullSecrets`, so the field survives the rewrite unused.
````

- [ ] **Step 2: Record the durable decision**

Add one bullet to `KANZ_BRAIN.md` under "## Security & multi-tenancy", after the Zero-trust bullet:

```markdown
- **The registry pull credential is a named exception to the Vault pattern, and it cannot be otherwise (OPS-M2f-b, 2026-07-26).** Every other secret arrives via Vault + CSI. This one cannot: the Vault CSI driver materializes a Kubernetes Secret only when a pod mounting the volume is *running*, and no pod runs until its image has been pulled. **The pull credential is the one secret that must exist before the mechanism that delivers every other secret.** So `ghcr-pull` is a plain `dockerconfigjson` Secret, created out-of-band once per namespace and attached at the ServiceAccount (never the pod spec, so Go-built Job specs inherit it). Attaching at the ServiceAccount is also what keeps rotation off the nodes: the rejected alternative, k3s `registries.yaml` written at join, puts a fleet-wide credential on every node's disk and makes revocation an SSH sweep — the exact daily-path-by-SSH outcome the operational control plane exists to remove. Anyone moving this into Vault will rediscover the deadlock from the inside.
```

- [ ] **Step 3: Update the board**

In `KANZ_TASKS.md`, rewrite the OPS-M2f-b row's status. Keep the row (it is not finished — the live proof is outstanding). Replace the `_Verified when:_` line with:

```markdown
_Verified when:_ a node provisioned through the TUI runs a platform workload with **no manual image step**, proven by scheduling one onto it immediately after join; and an arch guard asserts the chosen mechanism is present. **Mechanism chosen and shipped 2026-07-26** — `ghcr-pull` dockerconfigjson Secret attached at all 26 ServiceAccounts across `kanz-services`/`kanz-operator`/`kanz-messaging`, guarded by `TestPrivateImagesHavePullSecrets` (shown red, then green). Design: `docs/superpowers/specs/2026-07-26-image-pull-credentials-design.md`. **The runtime half is NOT done and is not claimed:** no cluster was reachable from the box that did this work, so nobody has yet watched a freshly joined node pull an image. That is the whole remaining scope of this row.
```

Then add a new row directly after OPS-M2f-c:

```markdown
**OPS-M2f-d · Registry-credential rotation from the TUI.** _(Deferred out of OPS-M2f-b, 2026-07-26, deliberately: the board's verified-when never asked for it and it would have pushed that row past three days.)_ Today rotating `ghcr-pull` is three `kubectl create secret docker-registry --dry-run | kubectl apply` calls — no SSH, which is the property that made the ServiceAccount mechanism the right one, but still `kubectl`, and this epic's gate is a full cycle with **zero** `kubectl`. Shape it exactly like `SetVenueKeys`: write-only by construction, no value-returning method on the interface, the credential existing as bytes only in the request and the Secret — never a model field, never rendered, never logged.
_Verified when:_ a token entered in the TUI replaces `ghcr-pull` in all three namespaces, a pod that was failing `ErrImagePull` on the old credential reaches Running without a `kubectl` invocation, and a credential-leak check over the TUI's process and logs comes back clean — the same executed check OPS-M2e ran against `SetVenueKeys`, not an assertion that it should be clean.
```

- [ ] **Step 4: Validate the board**

```bash
sh tools/validate-board.sh KANZ_TASKS.md; echo "exit=$?"
```

Expected: `board OK: 62 rows, 5 columns each` and `exit=0`. If the exit code is non-zero, a literal `|` leaked into prose — find it and remove it. Check the exit code, not the text.

- [ ] **Step 5: Commit**

```bash
git add DEMO_DEPLOYMENT.md KANZ_BRAIN.md KANZ_TASKS.md
git commit -m "docs: bootstrap step, the Vault exception, and what OPS-M2f-b did not finish

DEMO_DEPLOYMENT gets the one genuine bootstrap step — the ghcr-pull Secret
cannot come from inside a cluster whose operator runs a private image — plus
why the Secret is inert on the dev rig.

KANZ_BRAIN records why this secret cannot live in Vault: the CSI driver
materializes its Secret from a running pod, and no pod runs before its image
is pulled. Recorded so the next engineer does not rediscover the deadlock
from the inside.

The board keeps OPS-M2f-b open, because no cluster was reachable and nobody
has watched a freshly joined node pull an image. Mechanism shipped is not the
same claim as workflow proven."
```

---

## Definition of done for this plan

- `cd kanz && GOFLAGS=-mod=mod go test ./test/arch/ ./services/operator/...` passes.
- `sh tools/validate-board.sh KANZ_TASKS.md` exits 0.
- `TestPrivateImagesHavePullSecrets` has been observed **red** and **green**, with both outputs in a commit body.
- `KANZ_TASKS.md` still lists OPS-M2f-b as open, naming the live proof as the remaining scope.
- No credential value appears anywhere in the repository.
