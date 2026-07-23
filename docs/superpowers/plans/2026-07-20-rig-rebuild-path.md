# Rig Rebuild-and-Redeploy Path Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the dev kind rig reproducible from source — one command builds every image, loads it into the cluster, and applies the repo's own manifests — so the rig can serve as evidence about this repository's code again.

**Architecture:** Two shell tools under `tools/`, both fail-closed, both deriving their inputs from artifacts that already exist rather than from new lists. `rig-images.sh` parses the service→dockerfile matrix out of `.github/workflows/build.yml` (the authoritative list, already guarded by `kanz/test/arch/deployability_test.go`) and builds + `kind load`s each image. `rig-apply.sh` applies the in-repo SPIRE manifests, then applies `kanz/infra/deploy/` with the Vault-CSI volumes swapped for a declared dev Secret — the single, guarded deviation from production posture.

**Tech Stack:** Bash (Git Bash on Windows), Docker, kind, kubectl, Go (arch tests), buf.

## Global Constraints

- **Build context is the repo ROOT**, never `kanz/`. Every Dockerfile `COPY`s `kanz-schemas/gen/go/`, which the `kanz/go.mod` `replace` points at. Build with `docker build -f kanz/services/<svc>/Dockerfile .`
- **`kanz-schemas/gen/go` is generated-not-committed (EVT-15a).** It must be regenerated before any image build, exactly as `.github/workflows/build.yml:158-166` does it.
- **Image tag must be `ghcr.io/kanz-eng/<service>:latest`** — that is what every manifest in `kanz/infra/deploy/` references.
- **No second service list.** The matrix in `.github/workflows/build.yml` is the only enumeration of service→dockerfile. Anything needing that list parses it. A hardcoded copy is a plan failure.
- **Fail closed.** Every script exits non-zero on an empty or unexpectedly small parse result. Precedent and rationale: `tools/validate-board.sh`, and board row "#8" — a check whose result nothing acts on is not a check.
- **Dev deviations must be declared and guarded**, following `infra/deploy/postgres-dev.yaml` + `test/arch/postgres_dev_convention_test.go` and `infra/nats/bootstrap-job-dev-plaintext.yaml` + `test/arch/nats_bootstrap_posture_test.go`.
- **Guard discipline (board row: "guards that could not fire").** Never record a guard as mutation-proven unless EVERY arm was individually tripped and observed to fail. Strip YAML comments before scanning a manifest, so needles match configuration and not prose. Anchor needles that could collide with a resource name (`- name: spiffe-helper`, not `spiffe-helper`).

---

### Task 1: `tools/rig-images.sh` — build and load every image from the CI matrix

**Files:**
- Create: `tools/rig-images.sh`
- Test: `tools/rig-images.sh --list` (self-checking; no Go test — this is a shell tool, matching `tools/validate-board.sh`)

**Interfaces:**
- Produces: `tools/rig-images.sh [--list|--build|--load] [--cluster NAME]`. `--list` prints `service dockerfile` pairs one per line and exits 0. Later tasks call `--build --load`.

- [ ] **Step 1: Write the script**

```bash
#!/usr/bin/env bash
# Rebuild the dev rig's images from source and load them into kind.
#
# The rig's images were built by hand on 2026-07-12 and recorded nowhere, so the
# cluster drifted eight days from the repository and nothing could bring it back.
# That is the third instance of the same defect this week (the NATS bootstrap and
# the Postgres role were the first two): a one-shot setup step with no re-run path.
#
# THE SERVICE LIST IS NOT DUPLICATED HERE. It is parsed out of the CI build
# matrix, which test/arch/deployability_test.go already forces to stay complete.
# A local list would be a second enumeration that drifts silently; parsing makes
# the drift unrepresentable rather than merely detectable.
set -euo pipefail

WORKFLOW="${WORKFLOW:-.github/workflows/build.yml}"
REGISTRY="${REGISTRY:-ghcr.io/kanz-eng}"
CLUSTER="${CLUSTER:-kanz-dryrun}"
MIN_SERVICES=20   # fail-closed floor; the matrix had 23 entries on 2026-07-20

usage() { echo "usage: $0 [--list|--build|--load] [--cluster NAME]" >&2; exit 2; }

matrix() {
  [ -f "$WORKFLOW" ] || { echo "FATAL: $WORKFLOW not found (run from the repo root)" >&2; exit 2; }
  awk '
    /^[[:space:]]*-[[:space:]]+service:[[:space:]]*/  { svc=$NF; next }
    /^[[:space:]]*dockerfile:[[:space:]]*/ && svc!="" { print svc, $NF; svc="" }
  ' "$WORKFLOW"
}

require_matrix() {
  local n; n=$(matrix | wc -l)
  if [ "$n" -lt "$MIN_SERVICES" ]; then
    echo "FATAL: parsed only $n services from $WORKFLOW (expected >= $MIN_SERVICES)." >&2
    echo "The matrix format changed and this parser no longer understands it. Teach it the" >&2
    echo "new shape — do NOT lower MIN_SERVICES. A rebuild that silently skips services" >&2
    echo "produces a rig that is stale in exactly the way this tool exists to prevent." >&2
    exit 1
  fi
}

generate_sdk() {
  echo "==> regenerating kanz-schemas Go SDK (generated-not-committed, EVT-15a)"
  ( cd kanz-schemas && buf generate )
}

build() {
  require_matrix; generate_sdk
  while read -r svc dockerfile; do
    echo "==> build $REGISTRY/$svc:latest  (-f $dockerfile)"
    docker build -f "$dockerfile" -t "$REGISTRY/$svc:latest" .
  done < <(matrix)
}

load() {
  require_matrix
  while read -r svc _; do
    echo "==> kind load $REGISTRY/$svc:latest -> $CLUSTER"
    kind load docker-image "$REGISTRY/$svc:latest" --name "$CLUSTER"
  done < <(matrix)
}

[ $# -gt 0 ] || usage
did=""
while [ $# -gt 0 ]; do
  case "$1" in
    --list)    require_matrix; matrix; did=1 ;;
    --build)   build; did=1 ;;
    --load)    load;  did=1 ;;
    --cluster) shift; CLUSTER="${1:-}"; [ -n "$CLUSTER" ] || usage ;;
    *)         usage ;;
  esac
  shift
done
[ -n "$did" ] || usage
```

- [ ] **Step 2: Prove the parser fails closed before trusting it**

Run each and confirm the exact outcome:

```bash
bash tools/rig-images.sh                          # expect: usage, exit 2
WORKFLOW=/nonexistent bash tools/rig-images.sh --list   # expect: FATAL not found, exit 2
bash tools/rig-images.sh --list | wc -l           # expect: 23
```

Now trip the floor deliberately — this is the arm that actually protects us:

```bash
sed 's/^          - service:/          - xservice:/' .github/workflows/build.yml > /tmp/broken.yml
WORKFLOW=/tmp/broken.yml bash tools/rig-images.sh --list   # expect: FATAL parsed only 0, exit 1
```

Expected: the third command prints the "parsed only 0" FATAL and exits 1. If it exits 0, the floor is dead and must be fixed before going further.

- [ ] **Step 3: Verify the parse matches the manifests**

```bash
bash tools/rig-images.sh --list | awk '{print $1}' | sort > /tmp/built.txt
grep -rhoE "image: ghcr\.io/kanz-eng/([^:]+)" kanz/infra/deploy/ | sed 's|.*/||' | sort -u > /tmp/referenced.txt
diff /tmp/referenced.txt /tmp/built.txt
```

Expected: the only line in `built.txt` not in `referenced.txt` is `kanz-halt` (SEC-M3c — it runs as a Job in `ns/kanz-operator`, not from `infra/deploy/`). Any other difference means the matrix and the manifests disagree and `deployability_test.go` should have caught it — stop and investigate rather than proceeding.

- [ ] **Step 4: Commit**

```bash
git add tools/rig-images.sh
git commit -m "feat(tools): declare the rig's image build path, parsed from the CI matrix

The rig's images were hand-built on 2026-07-12 and recorded nowhere, so the
cluster ran code eight days older than the repo and could not be rebuilt from
source. Third instance of the one-shot-step-with-no-re-run-path defect after
the NATS bootstrap and the Postgres role.

The service list is parsed from .github/workflows/build.yml rather than copied,
so it cannot drift; the parser fails closed below 20 services.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Apply the in-repo SPIRE manifests to the rig

**Files:**
- Modify: none (the manifests already exist)
- Create: `tools/rig-apply.sh` (SPIRE stage only in this task; the deploy stage lands in Task 3)

**Interfaces:**
- Consumes: `kanz/infra/security/spire/{namespaces,rbac,spire-server,spire-agent,registration}.yaml`
- Produces: `tools/rig-apply.sh --spire [--cluster NAME]`, exit 0 once the agent DaemonSet is Ready.

**Why this is not a dev deviation:** SPIRE is declared in this repository and was simply never applied to the rig. Applying it makes the rig's SPIFFE mTLS real rather than stripped, which removes the largest reachable fidelity gap. Vault CSI is the one that stays unreachable (ONBOARD-M6) and is handled in Task 3.

- [ ] **Step 1: Write the SPIRE stage**

```bash
#!/usr/bin/env bash
# Stand the dev rig back up from the repository's own manifests.
set -euo pipefail

CLUSTER="${CLUSTER:-kanz-dryrun}"
SPIRE_DIR="kanz/infra/security/spire"

apply_spire() {
  [ -d "$SPIRE_DIR" ] || { echo "FATAL: $SPIRE_DIR not found (run from the repo root)" >&2; exit 2; }
  echo "==> applying SPIRE from $SPIRE_DIR"
  kubectl apply -f "$SPIRE_DIR/namespaces.yaml"
  kubectl apply -f "$SPIRE_DIR/rbac.yaml"
  kubectl apply -f "$SPIRE_DIR/spire-server.yaml"
  kubectl apply -f "$SPIRE_DIR/spire-agent.yaml"
  kubectl rollout status statefulset/spire-server -n spire-system --timeout=180s
  kubectl rollout status daemonset/spire-agent   -n spire-system --timeout=180s
  kubectl apply -f "$SPIRE_DIR/registration.yaml"
}

case "${1:-}" in
  --spire) apply_spire ;;
  *) echo "usage: $0 --spire [--cluster NAME]" >&2; exit 2 ;;
esac
```

- [ ] **Step 2: Run it against the rig**

```bash
bash tools/rig-apply.sh --spire
```

Expected: both rollouts report ready. If `registration.yaml` assumes a server API socket path that differs in this cluster, read the actual error — do not paper over it by skipping registration, because an agent without entries issues no SVIDs and every workload will then fail exactly as it did before.

- [ ] **Step 3: Prove SVIDs are actually issued**

```bash
kubectl get pods -n spire-system
kubectl exec -n spire-system statefulset/spire-server -- \
  /opt/spire/bin/spire-server entry show | head -40
```

Expected: at least one registration entry printed. An empty entry list means SPIRE is running and useless — treat that as failure, not success.

- [ ] **Step 4: Commit**

```bash
git add tools/rig-apply.sh
git commit -m "feat(tools): apply the in-repo SPIRE manifests to the dev rig

SPIRE has been declared in infra/security/spire/ all along and was never applied
to the rig, which is why the rig's manifests were hand-stripped of their SPIFFE
volumes. Applying it makes the rig's mTLS real instead of absent.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 3: The one declared dev deviation — Vault CSI swapped for a dev Secret

**Files:**
- Create: `kanz/infra/deploy/rig-dev-secrets.yaml`
- Modify: `tools/rig-apply.sh` (add the `--deploy` stage)
- Test: `kanz/test/arch/rig_dev_posture_test.go`

**Interfaces:**
- Consumes: `tools/rig-images.sh --list` (Task 1), the SPIRE stage (Task 2)
- Produces: `tools/rig-apply.sh --deploy`, which applies `kanz/infra/deploy/` with `secrets-store.csi.k8s.io` volumes replaced.

**The deviation, stated plainly:** 16 of 21 workloads mount DSNs and credentials from Vault via `secrets-store.csi.k8s.io`. Vault is absent and unreachable from this box (ONBOARD-M6), so the rig substitutes committed dev Secrets. The SPIFFE CSI volumes are NOT substituted — after Task 2 they are real.

**The substitution convention already exists in this repo — follow it, do not invent one.** In a `SecretProviderClass`, `objectName` is the FILENAME written into the mount and `secretKey` is the key inside Vault. So a Kubernetes Secret standing in for a CSI mount must use the `objectName` values as its KEYS. `postgres-dev.yaml` already does exactly this, and it names its Secret **`oms-db` — identical to the SecretProviderClass it shadows.** That name-for-name shadowing is what makes the patch in Step 4 a mechanical rename rather than a per-service mapping table.

The five SecretProviderClasses the rig's workloads reference, and the keys each must carry:

| SecretProviderClass | required Secret keys | status |
|---|---|---|
| `oms-db` | `database-url`, `migrate-database-url` | **already declared** in `postgres-dev.yaml` — do not duplicate |
| `tv-sync-db` | `database-url`, `migrate-database-url` | to create |
| `api-gateway-secrets` | `signing-secret` | to create |
| `webhook-ingest-redis` | `redis-url` | to create |
| `webhook-ingest-config` | `config.json` | to create |

`compliance` mounts no Vault CSI volume and needs nothing.

- [ ] **Step 1: Write the dev Secret manifest**

Create `kanz/infra/deploy/rig-dev-secrets.yaml` with the four Secrets NOT already covered by `postgres-dev.yaml`. Match that file's header style and its `kanz.eighred.com/posture: dev-only` label exactly — that is the existing posture label, and a second spelling of it would be a second convention.

Read the real DSN first and use it verbatim; do not retype it from memory:

```bash
grep -n "postgres://" kanz/infra/deploy/postgres-dev.yaml
```

At time of writing it is `postgres://kanzapp:kanz@postgres.kanz-services.svc:5432/kanzapp?sslmode=disable` (note the password is `kanz`, not `kanzapp`). If the manifest disagrees, the manifest wins.

```yaml
# DEV-ONLY. Do not apply to any cluster that trades real money.
#
# Production mounts every DSN and credential from Vault through
# secrets-store.csi.k8s.io (infra/security/secrets/secretproviderclass.yaml).
# The dev rig has no Vault and cannot reach one (ONBOARD-M6), so tools/rig-apply.sh
# rewrites each Vault CSI volume into a reference to a Secret OF THE SAME NAME as
# the SecretProviderClass it replaces, and this file declares those Secrets.
#
# Each Secret's KEYS are the SecretProviderClass's objectName values, because those
# are the filenames the workload reads. Getting this wrong produces a mount that is
# present and empty, which every service reports as a config error rather than a
# missing secret.
#
# oms-db is deliberately NOT here — postgres-dev.yaml already declares it.
apiVersion: v1
kind: Secret
metadata:
  name: tv-sync-db
  namespace: kanz-services
  labels:
    app.kubernetes.io/part-of: kanz
    kanz.eighred.com/posture: dev-only
type: Opaque
stringData:
  database-url: postgres://kanzapp:kanz@postgres.kanz-services.svc:5432/kanzapp?sslmode=disable
  migrate-database-url: postgres://kanzapp:kanz@postgres.kanz-services.svc:5432/kanzapp?sslmode=disable
---
apiVersion: v1
kind: Secret
metadata:
  name: api-gateway-secrets
  namespace: kanz-services
  labels:
    app.kubernetes.io/part-of: kanz
    kanz.eighred.com/posture: dev-only
type: Opaque
stringData:
  # HS256 signing secret for the dev gateway token. A known value in a committed
  # dev file is acceptable ONLY because this rig trades against the simulator.
  signing-secret: dev-only-not-a-real-signing-secret
---
apiVersion: v1
kind: Secret
metadata:
  name: webhook-ingest-redis
  namespace: kanz-services
  labels:
    app.kubernetes.io/part-of: kanz
    kanz.eighred.com/posture: dev-only
type: Opaque
stringData:
  redis-url: redis://redis.kanz-services.svc:6379
---
apiVersion: v1
kind: Secret
metadata:
  name: webhook-ingest-config
  namespace: kanz-services
  labels:
    app.kubernetes.io/part-of: kanz
    kanz.eighred.com/posture: dev-only
type: Opaque
stringData:
  config.json: |
    {"hmac_secret": "dev-only-not-a-real-hmac-secret"}
```

Before committing the `config.json` shape, confirm what `webhook-ingest` actually parses out of it — a guessed schema here fails at boot:

```bash
grep -rn "config.json\|/run/secrets" kanz/services/webhook-ingest/internal/config/ | head
```

Use the real field names. A guessed header name is exactly what board row #10's corollary records as the defect that only firing the thing catches.

- [ ] **Step 2: Write the failing posture guard**

```go
package arch

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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

	// --- the DSN must be the one convention, not a third copy of it ------------
	dsn := regexp.MustCompile(`postgres://[^\s"']+`).FindString(pgCfg)
	if dsn == "" {
		t.Fatal("postgres-dev.yaml no longer carries a postgres:// DSN. That manifest is the " +
			"source of truth for the rig's Postgres convention (itself pinned to CI); if it " +
			"changed shape, teach this guard the new one rather than deleting it.")
	}
	if !strings.Contains(devCfg, dsn) {
		t.Errorf("rig-dev-secrets.yaml does not carry the DSN %q from postgres-dev.yaml.\n"+
			"A rig whose Secret addresses a different database than the one its Postgres "+
			"manifest creates fails at runtime with an authentication error that looks like a "+
			"password problem and is not — which is exactly the failure that cost a session "+
			"on 2026-07-20.", dsn)
	}

	// --- posture ---------------------------------------------------------------
	// Scans the RAW file: the header is a comment, and stripping comments first
	// would make this arm permanently true. That inversion is deliberate.
	if !strings.Contains(dev, "DEV-ONLY") {
		t.Error("rig-dev-secrets.yaml must state DEV-ONLY in its header. It commits real-looking " +
			"credentials in plaintext; the only thing making that acceptable is that it is " +
			"unmistakably a dev artifact.")
	}
	if !strings.Contains(devCfg, "kanz.eighred.com/posture: dev-only") {
		t.Error("rig-dev-secrets.yaml must carry the label kanz.eighred.com/posture: dev-only on " +
			"every Secret, matching postgres-dev.yaml. A second spelling of the posture label is a " +
			"second convention, and the point of this file is that the rig has exactly one.")
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
```

If `stripYAMLComments` does not already exist in the package, reuse the one added by `d34bb7d` in `nats_bootstrap_posture_test.go`. Check first:

```bash
grep -rn "func stripYAMLComments" kanz/test/arch/
```

If it exists, do not write a second copy — call it. If it does not, move it into a shared file in the package rather than duplicating.

- [ ] **Step 3: Run the guard and watch every arm fail**

Partial mutation testing is what certified the last two dead guards. Trip each arm individually:

```bash
cd kanz && go test ./test/arch/ -run TestRigDevSecretsShadowEverySecretProviderClassTheRigMounts -v
```

Expected first run: PASS. Now break it five ways, one at a time, re-running after each and restoring before the next:

1. Delete the whole `tv-sync-db` Secret from `rig-dev-secrets.yaml` → expect the shadowing failure naming `tv-sync-deploy.yaml` and `tv-sync-db`.
2. Change the DSN's database name in `rig-dev-secrets.yaml` → expect the DSN mismatch failure.
3. Delete `DEV-ONLY` from the header → expect the DEV-ONLY failure.
4. Change one Secret's label to `kanz.dev/posture: dev-only` → expect the posture-label failure.
5. Add `  binance_key: "x"` under any `stringData` → expect the forbidden-credential failure.

Then confirm the deliberate inversion in arm 3: move `DEV-ONLY` so it appears **only** inside a `#` comment body. The arm must still PASS, because it scans the raw file on purpose. If it fails, it is reading the stripped copy and is checking the wrong string.

Do not record this guard as mutation-proven unless all five failures and the inversion check were each individually observed. Partial mutation testing is what certified the last two dead guards in this repo.

- [ ] **Step 4: Add the deploy stage to `tools/rig-apply.sh`**

First confirm the interpreter this repo can rely on:

```bash
python3 --version    # kanz-py already requires a Python toolchain
```

If `python3` is unavailable, stop and raise it rather than substituting `sed` — a
regex that edits YAML volume blocks by hand will silently mangle a manifest, and a
mangled manifest applied to the rig is the failure this whole plan exists to end.

```bash
DEPLOY_DIR="kanz/infra/deploy"

apply_deploy() {
  kubectl apply -f "$DEPLOY_DIR/rig-dev-secrets.yaml"
  kubectl apply -f "$DEPLOY_DIR/postgres-dev.yaml"
  # Redis is declared in-repo and was simply never applied to the rig — the same
  # never-applied pattern as SPIRE. webhook-ingest's nonce replay store needs it.
  kubectl apply -f kanz/infra/messaging/redis.yaml
  local applied=0
  for f in "$DEPLOY_DIR"/*-deploy.yaml "$DEPLOY_DIR"/*-rollout.yaml; do
    [ -e "$f" ] || continue
    # Swap the Vault CSI volumes for the dev Secret; leave the SPIFFE CSI volumes
    # alone, because after --spire they are real.
    python3 tools/rig_dev_patch.py "$f" | kubectl apply -f -
    applied=$((applied+1))
  done
  [ "$applied" -gt 0 ] || { echo "FATAL: applied 0 workloads from $DEPLOY_DIR" >&2; exit 1; }
  echo "==> applied $applied workloads"
}
```

And update the argument handling from Task 2 so both stages are reachable:

```bash
case "${1:-}" in
  --spire)  apply_spire ;;
  --deploy) apply_deploy ;;
  *) echo "usage: $0 --spire|--deploy [--cluster NAME]" >&2; exit 2 ;;
esac
```

`tools/rig_dev_patch.py`:

```python
#!/usr/bin/env python3
"""Swap Vault CSI volumes for the rig's dev Secret, on stdout.

Production mounts DSNs and credentials from Vault via secrets-store.csi.k8s.io.
The dev rig has no Vault (ONBOARD-M6), so each such volume becomes a reference to
the committed rig-dev-secrets Secret.

csi.spiffe.io volumes are deliberately LEFT ALONE: after `rig-apply.sh --spire`
the rig runs real SPIRE, so its SVIDs are genuine and stripping them would
reintroduce the very deviation this replaces.
"""
import sys
import yaml

VAULT_DRIVER = "secrets-store.csi.k8s.io"


def patch_pod_spec(spec, path):
    for vol in spec.get("volumes") or []:
        csi = vol.get("csi")
        if not (isinstance(csi, dict) and csi.get("driver") == VAULT_DRIVER):
            continue
        spc = (csi.get("volumeAttributes") or {}).get("secretProviderClass")
        if not spc:
            # Fail loudly. A Vault volume with no class named is a manifest we do
            # not understand, and guessing a Secret name here would produce a mount
            # that is present and empty — which surfaces as a confusing config error
            # at boot rather than as the missing secret it actually is.
            sys.exit(f"FATAL: {path}: volume {vol.get('name')!r} uses {VAULT_DRIVER} "
                     f"with no secretProviderClass; cannot choose a dev Secret for it")
        # The dev Secret shadows the SecretProviderClass name-for-name, which is
        # the convention postgres-dev.yaml already established with `oms-db`.
        vol.pop("csi")
        vol["secret"] = {"secretName": spc}


def main():
    if len(sys.argv) != 2:
        sys.exit("usage: rig_dev_patch.py <manifest.yaml>")

    with open(sys.argv[1]) as fh:
        docs = [d for d in yaml.safe_load_all(fh) if d]

    if not docs:
        sys.exit(f"FATAL: {sys.argv[1]} parsed to no documents")

    for doc in docs:
        # Deployment/StatefulSet/DaemonSet/Job and the Argo Rollout all carry the
        # pod spec at the same path.
        tmpl = (doc.get("spec") or {}).get("template") or {}
        if tmpl.get("spec"):
            patch_pod_spec(tmpl["spec"])

    yaml.safe_dump_all(docs, sys.stdout, default_flow_style=False)


if __name__ == "__main__":
    main()
```

Verify it edits what it should and nothing else, on a manifest that carries both drivers:

```bash
python3 tools/rig_dev_patch.py kanz/infra/deploy/oms-deploy.yaml | grep -c "secrets-store.csi.k8s.io"  # expect 0
python3 tools/rig_dev_patch.py kanz/infra/deploy/oms-deploy.yaml | grep -c "csi.spiffe.io"             # expect 1
python3 tools/rig_dev_patch.py kanz/infra/deploy/oms-deploy.yaml | grep -c "secretName: oms-db"        # expect 1
```

- [ ] **Step 5: Commit**

```bash
git add kanz/infra/deploy/rig-dev-secrets.yaml kanz/test/arch/rig_dev_posture_test.go tools/rig-apply.sh tools/rig_dev_patch.py
git commit -m "feat(rig): declare the dev secrets deviation and guard its posture

Replaces the hand-stripping of CSI volumes with one declared substitution:
Vault CSI becomes a committed dev Secret, SPIFFE CSI stays real. Guarded by
test/arch/rig_dev_posture_test.go, whose four arms were each individually
tripped and observed to fail.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 4: Bring the rig up and prove it runs this repository's code

**Files:** none (verification only)

This task produces no code. Its deliverable is evidence, and the board rows it settles are only settled if the evidence is real.

- [ ] **Step 1: Confirm Docker and the cluster**

```bash
docker info --format '{{.ServerVersion}}'
kind get clusters
kubectl config current-context
```

If the cluster is gone, recreate it before continuing. Do not proceed on a cluster whose name differs from `--cluster` without passing the real one.

- [ ] **Step 2: Full rebuild**

```bash
bash tools/rig-images.sh --build --load --cluster "$(kind get clusters | head -1)"
bash tools/rig-apply.sh --spire
bash tools/rig-apply.sh --deploy
kubectl -n kanz-services rollout restart deploy
kubectl -n kanz-services get pods -w
```

- [ ] **Step 3: Prove the two missing migrations now apply**

Board line 64: the rig's schema was two migrations behind — `positions` and `position_fills` did not exist.

```bash
kubectl -n kanz-services exec deploy/postgres -- \
  psql -U kanzapp -d kanzapp -c '\dt' | grep -E 'positions|position_fills'
```

Expected: both tables present. If they are not, the migrate initContainer is still applying an old image — check `kubectl -n kanz-services describe pod <oms-pod>` for the image ID actually pulled, not the tag.

- [ ] **Step 4: Prove the rig runs current code, not 2026-07-12 code**

The sharpest available check, from board line 64 — the renamed env var:

```bash
kubectl -n kanz-services get deploy tv-sync -o yaml | grep -A2 TV_SYNC_PRICE
```

Expected: the renamed variable from `76e538c`, and NOT `TV_SYNC_PRICE_SUBJECT=market.>` (the singular name carrying the wildcard the mark-poisoning fix removed). Seeing the old name means the apply did not take.

Also confirm `tv-sync` now has `TV_SYNC_DATABASE_URL`, without which `services/tv-sync/internal/config/config.go:101` refuses to start.

- [ ] **Step 5: Run the full trading loop**

Re-run the end-to-end proof recorded in the `kanz-e2e-cluster-proof` memory (webhook-ingest on the cluster spine, EXECUTION stream + EventFrame, limit-orders-only sim venue, HS256 gateway token). That is what converts "pods are Running" into "the loop works".

- [ ] **Step 6: Update the board honestly**

Rewrite board line 64 with what was actually verified and what was not. Specifically: state that Vault CSI remains substituted, so any row whose evidence depends on real Vault-mounted credentials is still unverified. Do not mark rows verified that this bring-up did not exercise.

```bash
bash tools/validate-board.sh KANZ_TASKS.md
git add KANZ_TASKS.md && git commit -m "docs(board): close the stale-rig row; the rig rebuilds from source

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Verification

The plan is complete when all of the following hold:

1. `bash tools/rig-images.sh --list` prints 23 service/dockerfile pairs; the floor arm exits 1 on a broken matrix.
2. `cd kanz && go test ./test/arch/` is green, and `TestRigDevSecretsShadowEverySecretProviderClassTheRigMounts` had all five arms tripped plus the raw-file inversion confirmed.
3. `cd kanz && make lint build test` is green.
4. All pods in `kanz-services` are Running, `positions` and `position_fills` exist, and `tv-sync` carries the renamed price env var plus `TV_SYNC_DATABASE_URL`.
5. The trading loop completes end to end against the rebuilt rig.
6. `bash tools/validate-board.sh KANZ_TASKS.md` exits 0.

**Known limit, to be stated on the board rather than glossed:** the rig substitutes Vault. It proves the code, the schema, the wiring and the loop. It does not prove anything about production credential mounting, and ONBOARD-M6 stays open.
