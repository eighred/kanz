# Dev-Rig Postgres Declaration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Declare the dev rig's Postgres — its role, its database, and the DSNs services consume — in the repository, so a pod restart rebuilds it instead of leaving the estate un-startable.

**Architecture:** One dev-only manifest (`infra/deploy/postgres-dev.yaml`) carrying a Service, a Deployment, an `initdb` ConfigMap, and a dev Secret. Postgres's official image runs every `/docker-entrypoint-initdb.d/*.sql` on a fresh data directory, so the role and database are recreated automatically on exactly the event that broke the rig — which the previous hand-run `psql` could not do. An arch test pins the manifest's role/database/password to the values `.github/workflows/kanz-ci.yml` declares, so the rig and CI cannot drift into two conventions.

**Tech Stack:** Kubernetes (kind), `postgres:16-alpine`, Go arch tests (`kanz/test/arch`).

## Global Constraints

- The role, database, and password come from `.github/workflows/kanz-ci.yml` **verbatim** and must not be invented: role `kanzapp`, `LOGIN PASSWORD 'kanz' NOSUPERUSER`, database `kanzapp` `OWNER kanzapp`, plus `GRANT ALL ON SCHEMA public TO kanzapp`.
- The DSN is `postgres://kanzapp:kanz@postgres.kanz-services.svc:5432/kanzapp?sslmode=disable`.
- `NOSUPERUSER` is load-bearing and must never be relaxed: every tenant table runs `FORCE  ROW LEVEL SECURITY` (note the two spaces — 13 occurrences across 11 migrations), which binds the table owner but **not** a superuser. A superuser DSN makes every tenant-isolation test pass vacuously. Verified live on 2026-07-20: as a NOSUPERUSER owner, an `ENABLE`-only table let `app.tenant_id='acme'` read a `globex` row, while the same table with `FORCE` returned 0 rows.
- Because every table FORCEs RLS, one role that owns its own database is safe. Do **not** add a second migrate/app role split; CI does not have one.
- This file is **dev-only**. Production Postgres is not in scope and is not declared here; `infra/deploy/oms-deploy.yaml` keeps its CSI `secretProviderClass: oms-db` volume and must not be edited.
- Storage stays ephemeral. The fix is that the rig **rebuilds** from source, not that it retains state. Do not add a PVC.
- The dev password is committed deliberately: it is already plaintext in `kanz-ci.yml`, and a dev credential that is hard to find is what produced the undeclared rig in the first place.
- Repo commits directly to `main`. No branch, no PR.
- Board rows use 5 columns; a literal `|` in prose breaks the table. Validate with `sh tools/validate-board.sh KANZ_TASKS.md` — **pass the file argument** and check the **exit code**, not the printed output.

---

### Task 1: The declared dev Postgres and its drift guard

**Files:**
- Create: `kanz/infra/deploy/postgres-dev.yaml`
- Create: `kanz/test/arch/postgres_dev_convention_test.go`
- Read for reference: `.github/workflows/kanz-ci.yml`, `kanz/infra/nats/bootstrap-job-dev-plaintext.yaml`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: a manifest at `infra/deploy/postgres-dev.yaml` containing Secret `oms-db` (keys `database-url`, `migrate-database-url`), ConfigMap `postgres-dev-initdb` (key `010-kanzapp.sql`), Service `postgres`, Deployment `postgres` — all in namespace `kanz-services`. Task 2 applies this file.

- [ ] **Step 1: Write the failing guard test**

Create `kanz/test/arch/postgres_dev_convention_test.go`. `readFile` and `moduleRoot` already exist in this package (`nats_bootstrap_posture_test.go`) — do not redeclare them.

```go
package arch

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The dev rig's Postgres was created by hand: somebody ran CREATE ROLE and
// CREATE DATABASE against a pod, wrote the DSN into a Secret, and nothing in the
// repository recorded any of it. The pod's storage is ephemeral, so when it was
// recreated the role and the database went with it and the OMS's migrate init
// container has been failing "password authentication failed for user kanzapp"
// ever since. The rig could not be rebuilt from source because the source never
// described it.
//
// infra/deploy/postgres-dev.yaml now declares it. This guard pins the one
// property that makes a second Postgres convention acceptable: it is not a
// second convention. CI already declares role/database/password in
// .github/workflows/kanz-ci.yml, the board documents WHY the role is
// NOSUPERUSER, and a rig that used different values would mean a test proving
// tenant isolation in CI proves nothing about the rig, and vice versa.
//
// This test reads CI as the source of truth and fails if the manifest disagrees,
// in EITHER direction — so changing CI's convention without changing the rig is
// also a failure, which is the drift that actually happens.
func TestDevPostgresMatchesTheCIConvention(t *testing.T) {
	root := moduleRoot(t)
	// The workflow lives above the Go module root.
	ci := readFile(t, filepath.Join(root, "..", ".github", "workflows", "kanz-ci.yml"))
	manifest := readFile(t, filepath.Join(root, "infra", "deploy", "postgres-dev.yaml"))

	role := mustFindOne(t, ci, `CREATE ROLE (\w+) LOGIN PASSWORD '([^']+)' NOSUPERUSER`,
		"CI no longer declares the app role with a literal CREATE ROLE ... NOSUPERUSER")
	db := mustFindOne(t, ci, `CREATE DATABASE (\w+) OWNER (\w+)`,
		"CI no longer declares the app database with a literal CREATE DATABASE ... OWNER")

	roleName, rolePassword := role[1], role[2]
	dbName, dbOwner := db[1], db[2]

	if dbOwner != roleName {
		t.Fatalf("CI declares database %q owned by %q but the app role is %q. This guard assumes "+
			"the single-role shape (the role owns its own database), which is safe ONLY because every "+
			"tenant table runs FORCE ROW LEVEL SECURITY and therefore binds its owner. If CI has split "+
			"into a migrate role and an app role, this test needs to learn about both before the dev "+
			"manifest copies one of them.", dbName, dbOwner, roleName)
	}

	// --- the manifest must declare the SAME role and database ---------------
	for _, want := range []struct{ needle, why string }{
		{"CREATE ROLE " + roleName + " LOGIN PASSWORD '" + rolePassword + "' NOSUPERUSER;",
			"the role, its password and its NOSUPERUSER posture must match CI exactly"},
		{"CREATE DATABASE " + dbName + " OWNER " + roleName + ";",
			"the database name and owner must match CI exactly"},
	} {
		if !strings.Contains(manifest, want.needle) {
			t.Errorf("infra/deploy/postgres-dev.yaml does not contain:\n  %s\n%s.\n\n"+
				"CI (.github/workflows/kanz-ci.yml) is the source of truth for this convention. If you "+
				"changed CI, change the manifest to match; if you changed the manifest, you have created "+
				"a second convention and a tenant-isolation test that passes in one place proves nothing "+
				"about the other.", want.needle, want.why)
		}
	}

	// --- NOSUPERUSER is the whole point ------------------------------------
	if strings.Contains(manifest, "SUPERUSER;") && !strings.Contains(manifest, "NOSUPERUSER;") {
		t.Error("infra/deploy/postgres-dev.yaml grants the app role SUPERUSER. A superuser BYPASSES " +
			"row-level security even where the table sets FORCE, so every tenant-isolation test would " +
			"pass without isolation existing. If migrations are failing on permissions, grant the " +
			"specific privilege — do not widen the role.")
	}

	// --- the DSN the services read must address that same database ---------
	wantDSN := "postgres://" + roleName + ":" + rolePassword + "@postgres.kanz-services.svc:5432/" + dbName + "?sslmode=disable"
	if !strings.Contains(manifest, wantDSN) {
		t.Errorf("infra/deploy/postgres-dev.yaml does not carry the DSN %q. The Secret's DSN and the "+
			"initdb script are the two halves of one fact: a manifest that creates database %q and hands "+
			"out a DSN for a different one fails at runtime with an authentication error that looks like "+
			"a password problem and is not.", wantDSN, dbName)
	}

	// --- it must not become a production manifest ---------------------------
	if !strings.Contains(manifest, "DEV-ONLY") {
		t.Error("infra/deploy/postgres-dev.yaml must state DEV-ONLY in its header. It commits a known " +
			"password and runs a single ephemeral replica with no backups; the reason that is acceptable " +
			"is that it is unmistakably a dev rig, and the header is what makes it unmistakable.")
	}
	if strings.Contains(manifest, "persistentVolumeClaim") {
		t.Error("infra/deploy/postgres-dev.yaml declares a PVC. The fix for the rig is that it REBUILDS " +
			"from the initdb script on a fresh data directory — that is what makes it reproducible from " +
			"source. A PVC preserves state instead, which means the next divergence survives too and is " +
			"invisible again. If this rig needs durable storage, that is a different decision than this one.")
	}
}

// mustFindOne applies pattern to src and requires exactly one match, so a
// workflow that grew a second CREATE ROLE fails loudly instead of silently
// pinning whichever one happened to come first.
func mustFindOne(t *testing.T, src, pattern, why string) []string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindAllStringSubmatch(src, -1)
	if len(m) == 0 {
		t.Fatalf("%s (pattern %q matched nothing). This guard reads CI as the source of truth for the "+
			"Postgres convention; if CI now provisions its database another way, teach this test the new "+
			"shape rather than deleting it — the rig and CI silently disagreeing is the failure it exists "+
			"to prevent.", why, pattern)
	}
	if len(m) > 1 {
		t.Fatalf("pattern %q matched %d times in the CI workflow. This guard pins ONE app role/database; "+
			"with several it cannot tell which one the dev rig should mirror. Narrow the pattern or split "+
			"the guard deliberately.", pattern, len(m))
	}
	return m[0]
}
```

- [ ] **Step 2: Run the test to verify it fails for the right reason**

Run from `C:\Users\root\Desktop\eighred-kanz\kanz`:

```
go test ./test/arch/ -run TestDevPostgresMatchesTheCIConvention -v
```

Expected: **FAIL**, with `read .../infra/deploy/postgres-dev.yaml: ... The system cannot find the file specified.` — the manifest does not exist yet. If it fails on the *CI* read instead, the `filepath.Join(root, "..", ".github", ...)` path is wrong for this repo layout; fix that before continuing, because a guard that cannot find CI would pass vacuously once the manifest exists.

- [ ] **Step 3: Write the manifest**

Create `kanz/infra/deploy/postgres-dev.yaml`:

```yaml
# DEV-ONLY Postgres for the kind rig. NOT a production database.
#
# WHY THIS FILE EXISTS. The rig's Postgres was never declared anywhere. Somebody
# ran CREATE ROLE / CREATE DATABASE against a pod by hand on 2026-07-12 and wrote
# the DSN into a Secret. The pod has no volumes, so when it was recreated the role
# and the database went with it — and the OMS's migrate init container has been
# failing since with `password authentication failed for user "kanzapp"`, which
# reads like a wrong password and is really a missing role. The rig could not be
# rebuilt from source, because the source never described it.
#
# This is the same failure the NATS bootstrap had: a one-shot setup step with no
# re-run path. The fix has the same shape. Postgres's entrypoint runs every
# /docker-entrypoint-initdb.d/*.sql on a FRESH data directory, so the role and
# database are recreated by exactly the event that destroyed them.
#
# STORAGE IS DELIBERATELY EPHEMERAL. The goal is a rig that REBUILDS from this
# file, not one that retains state. A PVC would preserve the data and, with it,
# the next undeclared hand-edit — invisible again until the next restart.
# Consequently: everything in this database is disposable. Do not put anything
# here you would miss.
#
# THE PASSWORD IS COMMITTED ON PURPOSE. It is already plaintext in
# .github/workflows/kanz-ci.yml, it guards nothing but a local kind cluster, and
# a dev credential that lives only in somebody's shell history is precisely what
# produced this outage. Production credentials come from Vault via the CSI mounts
# in oms-deploy.yaml and tv-sync-deploy.yaml, which this file does not touch.
#
# THE VALUES COME FROM CI, NOT FROM TASTE. Role, password and database are copied
# verbatim from kanz-ci.yml so that the rig and CI are one convention rather than
# two. test/arch/postgres_dev_convention_test.go fails if they drift apart in
# either direction.
#
#   kubectl apply -f infra/deploy/postgres-dev.yaml
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: postgres-dev-initdb
  namespace: kanz-services
  labels:
    app.kubernetes.io/part-of: kanz
    kanz.eighred.com/posture: dev-only
data:
  # Runs ONCE per fresh data directory, as the POSTGRES_USER superuser, before
  # the server accepts external connections.
  #
  # NOSUPERUSER IS LOAD-BEARING — it is not hardening decoration. Every tenant
  # table runs `FORCE  ROW LEVEL SECURITY` (13 occurrences across 11 migrations),
  # which makes the isolation policy bind even the table's OWNER. It does NOT
  # bind a superuser: Postgres exempts superusers from RLS unconditionally. So a
  # superuser DSN does not merely widen access, it makes every tenant-isolation
  # test pass while proving nothing.
  #
  # Because the tables FORCE RLS, one role owning its own database is safe, and
  # that is why there is no separate migrate role here: CI does not have one
  # either. A split would be a third convention.
  010-kanzapp.sql: |
    CREATE ROLE kanzapp LOGIN PASSWORD 'kanz' NOSUPERUSER;
    CREATE DATABASE kanzapp OWNER kanzapp;
    \connect kanzapp
    GRANT ALL ON SCHEMA public TO kanzapp;
---
apiVersion: v1
kind: Secret
metadata:
  name: oms-db
  namespace: kanz-services
  labels:
    app.kubernetes.io/part-of: kanz
    kanz.eighred.com/posture: dev-only
type: Opaque
stringData:
  # Two keys because the deployments mount two: the migrate init container reads
  # migrate-database-url and the service reads database-url. In PRODUCTION these
  # differ — migrate holds a DDL-capable credential the service deliberately does
  # not have, so a compromised service cannot alter its own schema. Here they are
  # the SAME string, because CI's single role owns its database and can therefore
  # do both.
  #
  # That collapse is a dev simplification and must not be read as the production
  # shape. Production keeps them distinct via the Vault-backed CSI mounts in
  # oms-deploy.yaml; this file does not change that and does not apply there.
  database-url: postgres://kanzapp:kanz@postgres.kanz-services.svc:5432/kanzapp?sslmode=disable
  migrate-database-url: postgres://kanzapp:kanz@postgres.kanz-services.svc:5432/kanzapp?sslmode=disable
---
apiVersion: v1
kind: Service
metadata:
  name: postgres
  namespace: kanz-services
  labels:
    app.kubernetes.io/part-of: kanz
spec:
  selector:
    app: postgres
  ports:
    - { name: postgres, port: 5432, targetPort: 5432 }
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
  namespace: kanz-services
  labels:
    app.kubernetes.io/part-of: kanz
    kanz.eighred.com/posture: dev-only
spec:
  # One. Two replicas over one emptyDir each are two unrelated databases behind a
  # single Service name, handing out different answers per connection.
  replicas: 1
  strategy:
    # Recreate, not RollingUpdate: a rolling update would briefly run a second,
    # empty database serving the same Service.
    type: Recreate
  selector:
    matchLabels:
      app: postgres
  template:
    metadata:
      labels:
        app: postgres
        app.kubernetes.io/part-of: kanz
    spec:
      containers:
        - name: postgres
          image: postgres:16-alpine
          ports:
            - { containerPort: 5432, name: postgres }
          env:
            # The bootstrap superuser, used only by the initdb script above and
            # by operators. Services connect as kanzapp.
            - { name: POSTGRES_USER, value: "postgres" }
            - { name: POSTGRES_PASSWORD, value: "postgres" }
            - { name: POSTGRES_DB, value: "kanz" }
            # initdb writes into the data directory, and mounting a volume at
            # /var/lib/postgresql/data itself leaves a lost+found that initdb
            # refuses to start on. The subdirectory is the documented shape.
            - { name: PGDATA, value: "/var/lib/postgresql/data/pgdata" }
          volumeMounts:
            - { name: initdb, mountPath: /docker-entrypoint-initdb.d, readOnly: true }
            - { name: data, mountPath: /var/lib/postgresql/data }
          readinessProbe:
            exec:
              command: ["pg_isready", "-U", "postgres", "-d", "kanz"]
            initialDelaySeconds: 5
            periodSeconds: 5
          livenessProbe:
            exec:
              command: ["pg_isready", "-U", "postgres", "-d", "kanz"]
            initialDelaySeconds: 20
            periodSeconds: 15
      volumes:
        - name: initdb
          configMap:
            name: postgres-dev-initdb
        # Ephemeral on purpose — see the header.
        - name: data
          emptyDir: {}
```

- [ ] **Step 4: Run the test to verify it passes**

```
go test ./test/arch/ -run TestDevPostgresMatchesTheCIConvention -v
```

Expected: **PASS**.

- [ ] **Step 5: Prove the guard is not vacuous**

A guard that passes against anything is worse than none. Make each arm fire once, reverting after each:

1. In `postgres-dev.yaml`, change `CREATE DATABASE kanzapp OWNER kanzapp;` to `CREATE DATABASE kanzdb OWNER kanzapp;`. Re-run → expect FAIL naming the missing `CREATE DATABASE kanzapp OWNER kanzapp;`. Revert.
2. Change the two DSN values' `/kanzapp?sslmode=disable` to `/kanzdb?sslmode=disable`. Re-run → expect FAIL naming the DSN. Revert.
3. Delete the word `DEV-ONLY` from the header's first line. Re-run → expect FAIL on the DEV-ONLY assertion. Revert.
4. Add `        - name: data\n          persistentVolumeClaim:\n            claimName: x` — simply insert the literal string `persistentVolumeClaim` in a comment line. Re-run → expect FAIL on the PVC assertion. Revert.

Confirm the file is byte-identical to Step 3 afterwards: `git diff --stat infra/deploy/postgres-dev.yaml` must show the file as new/untracked with no partial edits.

- [ ] **Step 6: Build and commit**

```
go build ./... && go vet ./test/arch/
git add infra/deploy/postgres-dev.yaml test/arch/postgres_dev_convention_test.go
git commit -m "fix(dev): declare the rig's Postgres role, database and DSN; guard against CI drift

The rig's Postgres was created by hand and never written down. Its pod has no
volumes, so a recreate destroyed the kanzapp role and database and left the OMS
migrate init container failing on an authentication error that was really a
missing role. An initdb ConfigMap rebuilds both on a fresh data directory, so
the event that broke it now fixes it. Values are copied verbatim from
kanz-ci.yml and pinned by an arch test, so the rig and CI stay one convention.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

### Task 2: Apply to the rig and prove the trading spine starts

**Files:**
- No repository files change in this task. It applies `kanz/infra/deploy/postgres-dev.yaml` to the live kind cluster and records the result.

**Interfaces:**
- Consumes: `infra/deploy/postgres-dev.yaml` from Task 1.
- Produces: a verified `oms` pod in `Running` state, or a named failure.

- [ ] **Step 1: Record the current broken state, so the fix is demonstrably the cause**

```
kubectl get pods -n kanz-services
```

Expected: `oms` in `Init:CrashLoopBackOff`. Record the exact restart count — it is the before-value.

- [ ] **Step 2: Apply the manifest**

```
kubectl apply -f infra/deploy/postgres-dev.yaml
```

Expected: `configmap/postgres-dev-initdb created`, `secret/oms-db configured`, `service/postgres configured`, `deployment.apps/postgres configured`.

- [ ] **Step 3: Force a fresh data directory**

The initdb script runs only on an EMPTY data directory. The running pod already has an initialised one, so applying the ConfigMap alone changes nothing — this is the step that is easy to skip and produces a "fix" that did not fix anything.

```
kubectl delete pod -n kanz-services -l app=postgres
kubectl rollout status deploy/postgres -n kanz-services --timeout=120s
```

Expected: `deployment "postgres" successfully rolled out`.

- [ ] **Step 4: Verify the role and database now exist**

```
kubectl exec -n kanz-services deploy/postgres -- psql -U postgres -d kanz -c "\du kanzapp" -c "\l kanzapp"
```

Expected: `kanzapp` listed with **no** `Superuser` attribute, and a database named `kanzapp` owned by `kanzapp`.

If the role is absent, read the pod's logs (`kubectl logs -n kanz-services deploy/postgres | head -50`) — initdb prints each script it runs. A ConfigMap mounted but not executed usually means the data directory was not empty, i.e. Step 3 did not take effect.

- [ ] **Step 5: Verify the OMS starts**

```
kubectl delete pod -n kanz-services -l app=oms
kubectl rollout status deploy/oms -n kanz-services --timeout=180s
kubectl get pods -n kanz-services
```

Expected: `oms` reaches `Running` with all containers ready. Its `migrate` init container must complete — check with `kubectl logs -n kanz-services deploy/oms -c migrate`, which should report applied migrations rather than an authentication failure.

- [ ] **Step 6: Record the outcome truthfully, and do not overclaim**

Write down what actually happened, including partial success. If `oms` starts but another service does not, that is a separate undeclared dependency and belongs on the board as its own row — not folded into this one. Do not report the rig as healthy on the strength of the OMS alone; run `kubectl get pods -n kanz-services` and report every pod's real state.

**THE HARD BOUND ON WHAT THIS TASK PROVES.** Established on 2026-07-20 by inspection: every deployment in `kanz-services` was created `2026-07-12` and pins `:latest`, so all five run images pulled eight days ago. The live `tv-sync` still sets `TV_SYNC_PRICE_SUBJECT=market.>` — the singular variable that `76e538c` renamed and the wildcard the mark-poisoning fix removed — which confirms the running binaries predate the last eight days of work.

Therefore `oms` reaching `Running` proves exactly one thing: **the Postgres role, database and DSN are now declared and reachable.** It does NOT prove any current OMS behaviour, because the image under test is not built from `HEAD`. Say so in the report in those terms. Do not write "the trading loop works", "the rig is green", or any sentence a reader could take as evidence about code written this week.

---

### Task 3: Update the board

**Files:**
- Modify: `KANZ_TASKS.md`

**Interfaces:**
- Consumes: the verified outcome from Task 2.

- [ ] **Step 1: Rewrite the dev-rig reproducibility row**

Find the existing row about the NATS bootstrap not being self-healing after data loss, and the row (or add one) covering the undeclared Postgres. The Postgres row becomes `FIXED` with the commit from Task 1 Step 6, and must record:

- what the defect was: the role and database existed only as a hand-run `psql` command from 2026-07-12; ephemeral storage destroyed them; the symptom was an authentication error that looked like a wrong password.
- what the fix does: an initdb ConfigMap so the destroying event is also the repairing one.
- the verification actually performed in Task 2 — the real pod states, not the intended ones.
- that this is the **second** instance in one day of the same class (NATS bootstrap was the first): a one-shot setup step with no re-run path. Two instances is a pattern, and the honest open question is how many more undeclared hand-run steps the rig contains. State that as an open question; do not claim it is now the last one.

Keep every row to 5 columns and use no literal `|` in prose.

- [ ] **Step 2: Add a row recording the false RLS alarm**

During this work a grep for `FORCE ROW LEVEL` (one space) matched nothing and appeared to show that no migration forces RLS — which would have meant tenant isolation was unenforced against the app role. It was wrong: every migration writes `FORCE  ROW LEVEL SECURITY` with **two** spaces, 13 occurrences across 11 migrations. Record this as a **pattern** row, not a defect row: a search whose negative result was treated as evidence of absence. The real state is that RLS is correctly forced everywhere and the arch tests covering it are accurate.

Also record what the episode did establish, since it was measured live: `FORCE` is what makes the policy bind the table's owner, and a superuser bypasses RLS regardless — which is why the dev role is `NOSUPERUSER`.

- [ ] **Step 3: Add a row for the stale rig — the finding that outlives this task**

This is the most consequential thing the work uncovered and it must not be buried in the Postgres row. Verified 2026-07-20 by inspecting the live cluster:

- All five `kanz-services` deployments were created `2026-07-12` and pin `:latest`. They run images pulled eight days ago, so **no work from the last eight days is running anywhere.**
- The live `tv-sync` still sets `TV_SYNC_PRICE_SUBJECT=market.>` — the singular env var `76e538c` renamed, carrying the exact wildcard the mark-poisoning fix removed.
- The rig's manifests are hand-stripped relative to `infra/deploy/`: `api-gateway` is missing its declared `api-gateway-secrets` CSI volume, `webhook-ingest` its `webhook-ingest-redis` volume, and `tv-sync` its `tv-sync-db` volume entirely — no db volume, no `TV_SYNC_DATABASE_URL`, no migrate init container, even though `services/tv-sync/internal/config/config.go:101` refuses to start without one. The running binary predates that requirement.

State the consequence plainly: **the rig is not running the code in this repository**, so it cannot be used as evidence that any recent change works. Every board row currently marked "IMPLEMENTED BUT UNVERIFIED" stays unverified — the cluster does not upgrade any of them. With CI halted on billing, local `go test` remains the only evidence channel.

Do not propose a fix in this row. Whether the rig gets a rebuild-and-redeploy path is a decision with real cost, and it belongs to the lead.

- [ ] **Step 4: Validate the board and check the exit code**

```
sh tools/validate-board.sh KANZ_TASKS.md; echo "exit=$?"
```

Expected: `exit=0`. A validator that prints a complaint and exits 0 is finding #8 on the board's own pattern row — check the number, not the text.

- [ ] **Step 5: Commit**

```
git add KANZ_TASKS.md
git commit -m "docs(board): close the undeclared dev Postgres row; record the stale rig and the grep false alarm

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```
