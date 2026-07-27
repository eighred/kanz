# DR spine reconstruction — `nats-rebuild` image, topic correctness, and digest enforcement

**Date:** 2026-07-27 · **Priority:** P0 (DR) + P1 (image integrity) · **Status:** design approved, not implemented

> The disaster-recovery path cannot execute. Its container image has never been
> built, and its topic list names almost none of the data it exists to restore.
> Publishing the image without correcting the topic list would produce a DR run
> that exits 0 having restored nothing — which is worse than the current failure,
> because the current failure is loud.

---

## 1. Functional scope of `nats-rebuild`

### What it is

`nats-rebuild` (DR-01c) reconstructs the **NATS live spine** after a region
failover. NATS is the short-retention hot tier and is **not a system of record**
(`KANZ_BRAIN.md` → Event platform); Kafka is the durable log. A DR region comes
up with an empty NATS cluster, so the spine must be repopulated from the
DR-replicated Kafka log (DR-01a) before consumers start.

Its place in the cutover (DR-01d) is fixed and ordered:

1. NATS up, streams provisioned (`nats-bootstrap` Job)
2. **`nats-rebuild`** — this Job
3. Start consumers

### What it restores, and why

It drains a bounded recent window of each **archived** Kafka topic and
republishes each event onto its **original live subject**, envelope unchanged.

Three properties make that safe, and each is load-bearing:

- **It is not replay.** The EVT-20 `replay.Reader` is reused as the source
  (no consumer group, so no live consumer's offsets are mutated), but replay
  republishes onto the isolated `replay.{run}.*` namespace and stamps
  `QUALITY_FLAG_REPLAYED` so live sinks reject it. Reconstruction is the inverse:
  the original subject, no flag, so live consumers accept the events normally.
- **Routing comes from the envelope, not the topic name.** `Pipeline.Run`
  (`rebuild.go:88`) uses `ev.Envelope.GetEventType()` — the three-segment logical
  subject. The topic list therefore answers only *"which Kafka topics do we
  read?"*, never *"where does this event go?"*.
- **Re-running is safe.** `Nats-Msg-Id` propagates in the headers, so the
  stream's broker-side dedup window collapses duplicates, and handlers are
  idempotent by platform premise (DEBT-02). A partial rebuild is re-run, not
  repaired.

### What it must restore

The **archiver's produced set**: every Kafka topic the archiver writes. That is
what can be rebuilt from Kafka, by definition — nothing else is there.

Explicitly out of scope, and each for a stated reason:

| Excluded | Why |
|---|---|
| `market.book`, `market.crypto` | Deliberately not archived (DATA-M1 scope): highest-volume topics by far and re-fetchable from the venue, unlike a fill. |
| `dlq.archiver` | Poison messages that could not be turned into state. Republishing them onto the live spine would re-inject exactly the events that already failed. |
| `wealth.household`, `alternatives.commitment` | Provisioned, but the archiver subscribes no `wealth.>` or `alternatives.>` subject, so nothing was ever written to them. |
| `risk.exposure`, `risk.signal`, `risk.command` | Archiver subscribes them; no publisher exists and no topic is provisioned (`unbackedByDesign`). |

---

## 2. The defects

### 2.1 The image has never existed

`kanz/tools/natsrebuild/` contains `main.go`, `rebuild.go`, `rebuild_test.go`
and **no Dockerfile**. `nats-rebuild` appears in neither `build.yml`'s
25-service matrix nor `release.yml`'s. `rebuild-job.yaml:46-48` says so in its
own comment — *"add it to the CICD-01c image matrix + a distroless Dockerfile
(follow-up)"* — and neither happened.

Consequence: `infra/dr/failover.sh:59` waits 600s on `job/nats-rebuild`, which
`ImagePullBackOff`s. **The DR cutover cannot complete.**

### 2.2 The topic list is the dead table — this is the severe one

`rebuild-job.yaml:55` sets `NATS_REBUILD_TOPICS` to twelve topics. Eight
**do not exist**: `market.equity`, `market.option`, `execution.order`,
`execution.order.snapshot`, `risk.portfolio.snapshot`, `platform.model`,
`platform.config`, `data.market_stream`. Those are verbatim the pre-`36a9ba1`
table that `topics-job.yaml:40-46` documents as *"NOT ONE of which is published
by any service."*

Of the fourteen topics the archiver actually writes, the DR Job names **four**
(`risk.portfolio`, `inference.feature`, `inference.prediction`, `data.feature`).
It names **no money-path topic at all** — no `order.order`, no
`accounting.balance`, no `settlement.instruction`, no `compliance.breach`, no
`compliance.mandate`, no `risk.position`.

So a DR rebuild today would restore no orders, no fills, no accounting, no
positions and no mandates, then log `nats-rebuild complete` and exit 0. A
mandate that does not come back is a compliance control that returns
**DISARMED** — the failure `topics-job.yaml:47-51` warns about in writing.

This is a **fourth copy** of the topic table. The 2026-07-21 sweep (`36a9ba1`)
found three — `topics-job.yaml`, `tenancy.yaml`, `infra/kafka/README.md` — and
missed this one because it lives in an env var in a DR manifest.

### 2.3 A time-only window cannot restore compacted state

`NATS_REBUILD_SINCE` defaults to `24h` and is applied uniformly
(`main.go:70-78`: `StartTime = now-since`, `EndTime = now`).

Three of the archived topics are **compacted, retention −1** —
`compliance.mandate`, `risk.position` (and `wealth.household`, unarchived).
Compaction retains the latest record per key **regardless of age**. A mandate
armed three days ago and never changed has a three-day-old timestamp, falls
outside a 24-hour window, and is **not read**. The Job would skip precisely the
state the compaction exists to preserve.

The time window is correct for event topics (bounded recent history, matching
the live tier's longest max-age) and wrong for state topics, which must be read
in full.

### 2.4 `LAKE_SINK_TOPICS` is unguarded, and the board records the opposite

`KANZ_TASKS.md` row 58 marks lake-sink topic drift **"REFUTED — both false…
guarded by two arch tests."** `grep -rn LAKE_SINK_TOPICS kanz --include=*.go`
returns one hit: `lake-sink/internal/config/config.go:48`, reading the env var.
**No arch test references it.** The neighbouring guard,
`TestArchiverConsumeSetHasBackingTopics`, checks archiver subjects against
provisioned topics — not lake-sink's list.

A tenth entry in this repo's "guards that did not guard" catalogue, and the
first one asserted as *refuted* on the board.

### 2.5 `:latest` in a production manifest, with no guard

`ghcr.io/eighred/nats-rebuild:latest` is the last mutable tag in `infra/`
(41 refs are `@sha256:`; the only others are `:pr` preview tags). No arch test
forbids mutable tags — I searched every `func Test*` in `kanz/test/arch/`.
OPS-M4a's stated deliverable was that guard, in those words: *"add an arch guard
that fails the build on any mutable tag in a production manifest. The guard is
the point."* The pinning shipped; the guard did not, and 2.2 is the proof it
already drifted.

---

## 3. Canonical source of the archived topic set

### The source

**`infra/kafka/topics-job.yaml`'s provisioned table, intersected with the
archiver's `DefaultSubjects` coverage.** No new list is introduced anywhere.

- `topics-job.yaml` is already the single source of truth for what topics exist
  (`36a9ba1` made it so and `TestEverySubjectHasAKafkaTopic` holds it to the code).
- `services/archiver/internal/config/config.go:19` `DefaultSubjects` is already
  the single source of truth for what the archiver consumes.
- Their intersection is, exactly, what the archiver writes to Kafka — and
  therefore exactly what can be rebuilt from Kafka.

### How the derived set is computed

For each topic `{domain}.{entity}` in the provisioned table, include it if any
archiver `DefaultSubjects` entry covers it — that is, if the subject is
`{domain}.>` or `{domain}.{entity}.>`. Exclude `dlq.*` (reserved prefix; not a
`{domain}.{entity}` pair and never republishable).

Both helper functions already exist in `test/arch` and are reused, not rewritten:
`provisionedTopics()` (parses the table) and `archiverDefaultSubjects()` (parses
the Go slice) — both from `archiver_topology_test.go`, which performs this same
computation in the opposite direction.

### The result, computed by hand and verified against the deployed manifest

| Provisioned topic | Covering subject | In set |
|---|---|---|
| `order.order` | `order.>` | ✅ |
| `strategy.signal` | `strategy.>` | ✅ |
| `accounting.balance` | `accounting.>` | ✅ |
| `settlement.instruction` | `settlement.>` | ✅ |
| `compliance.breach` | `compliance.breach.>` | ✅ |
| `compliance.mandate` | `compliance.mandate.>` | ✅ |
| `risk.portfolio` | `risk.portfolio.>` | ✅ |
| `risk.position` | `risk.position.>` | ✅ |
| `platform.authz` | `platform.>` | ✅ |
| `platform.compliance` | `platform.>` | ✅ |
| `platform.mode` | `platform.>` | ✅ |
| `inference.feature` | `inference.>` | ✅ |
| `inference.prediction` | `inference.>` | ✅ |
| `data.feature` | `data.>` | ✅ |
| `wealth.household` | — | ❌ |
| `alternatives.commitment` | — | ❌ |
| `market.book`, `market.crypto` | — | ❌ |
| `dlq.archiver` | reserved prefix | ❌ |

**Fourteen topics** — set-identical to the deployed `LAKE_SINK_TOPICS`
(`lake-sink-deploy.yaml:138`), which is independent corroboration that the
derivation is right: lake-sink and nats-rebuild are two consumers of one
archived set, for different purposes (permanent lakehouse vs. spine
reconstruction), and they must agree.

### How drift is prevented — guards, not a new list

Both manifests keep their explicit, operator-readable lists. Neither may drift:

**`TestArchivedTopicConsumersMatchTheArchiverProducedSet`** computes the derived
set and asserts that **both** `LAKE_SINK_TOPICS` and `NATS_REBUILD_TOPICS` equal
it exactly — set-difference empty in both directions — with a named-exemption
map carrying written reasons and a dead-exemption arm, matching the established
pattern in `supplychain_test.go` and `archiver_topology_test.go`.

This follows how `36a9ba1` resolved the identical problem: delete the copies you
can, and *hold the ones that must exist together with a guard*. It adds no
manually maintained source of truth, and it closes §2.4 in the same stroke.

---

## 4. Image lifecycle, end to end

| Stage | Mechanism | Artifact |
|---|---|---|
| **1. Build** | `kanz/tools/natsrebuild/cmd/nats-rebuild/Dockerfile`, modelled on `cmd/kanz-halt/Dockerfile`: `golang:1.26.5` builder, `GOFLAGS=-mod=mod`, build context the **repo root** (the module `replace`s `kanz-schemas-go => ../kanz-schemas/gen/go`, generated-not-committed per EVT-15a), `CGO_ENABLED=0 -trimpath -ldflags="-s -w"`, `gcr.io/distroless/static:nonroot`, `USER nonroot:nonroot`. | static binary |
| **2. CI build** | `build.yml` matrix entry `{service: nats-rebuild, dockerfile: kanz/tools/natsrebuild/cmd/nats-rebuild/Dockerfile}`. Matrix 25 → 26. Pushes on `push: main`. | `ghcr.io/eighred/nats-rebuild:sha-<commit>` |
| **3. Release** | `release.yml` matrix, same entry. 25 → 26. `TestReleaseMatrixCoversEveryBuiltService` holds the two matrices equal, so a one-sided edit fails the build. Trivy scan (fail on CRITICAL), cosign keyless sign, SBOM — all pinned to the build digest. | signed, scanned, attested digest |
| **4. Digest pinning** | `release.yml`'s `pin-digests` job: each matrix job writes `digests/<service>`, the job downloads all of them, runs `cmd/kanz-pin-digests -digests ../digests -infra infra`, re-renders `oms-acme.yaml` via `kanz-tenantgen`, and opens a `release/pin-<tag>` PR. Already built; `nats-rebuild` joins it automatically once it is in the matrix. | `rebuild-job.yaml` image → `@sha256:` |
| **5. Runtime** | `failover.sh` deletes and re-applies the Job; the kubelet pulls the pinned digest using `imagePullSecrets: ghcr-pull`, already present on the `nats-rebuild` ServiceAccount (`rebuild-job.yaml:22-23`). The Job mounts its SPIFFE SVID, publishes over mTLS, drains each topic, logs `nats-rebuild complete`, exits. | rebuilt spine |
| **6. Enforcement** | `TestProductionManifestsPinImagesByDigest` fails the build if any `ghcr.io/eighred/` image in `infra/` is not `@sha256:`. | drift impossible |

**Sequencing constraint, and it is real:** step 6 cannot go green before step 4
has run once — there is no digest to pin to until the image is published. So
`nats-rebuild` lands with a **named exemption stating exactly that reason**; the
first `pin-digests` run supplies the digest, and the guard's dead-exemption arm
then fails the build until the exemption is deleted. Self-cleaning, rather than
a TODO nobody returns to.

---

## 5. Changes

1. **`rebuild-job.yaml`** — replace `NATS_REBUILD_TOPICS` with the derived
   14-topic set; split the read window so compacted topics are read in full
   (§2.3); image stays `:latest` only until step 4 pins it.
2. **`main.go`** — a per-topic read window. `replay.Range` already supports it:
   `StartOffset: 0` (earliest retained) with `EndTime: now` for state topics,
   `StartTime: now-since` with `EndTime: now` for event topics. `StartOffset`
   and `StartTime` are mutually exclusive (`reader.go:39`), so this is a
   selection, not a new bound.

   **The compacted classification is not a new hand-maintained list.** It is
   already declared, once, in `topics-job.yaml`'s `cleanup` column
   (`compact` vs `delete`) — the same table §3 derives the topic set from. The
   manifest carries it as a second explicit, operator-readable env var
   (`NATS_REBUILD_STATE_TOPICS`), and the **same guard** that checks the topic
   set also asserts this list equals exactly the `compact` rows of the derived
   set. A topic in neither class is refused at startup (fail-closed), so a new
   topic cannot be read under the wrong window by default.
3. **New Dockerfile** (§4 step 1).
4. **`build.yml` + `release.yml`** — matrix entries, together.
5. **New guard** `TestArchivedTopicConsumersMatchTheArchiverProducedSet`.
6. **New guard** `TestProductionManifestsPinImagesByDigest`.
7. **`KANZ_TASKS.md`** — correct row 58's false "guarded by two arch tests"
   claim; record OPS-M4a's guard as delivered.

### Rejected alternatives

- **Runtime topic discovery from the Kafka cluster.** Would drain
  `dlq.archiver` (poison messages back onto the live spine) and
  `market.book`/`market.crypto` (unarchived, highest volume). Filtering that
  requires a list again, so it removes nothing and adds a failure mode.
- **A shared Go constant, env var deleted.** Removes per-cluster override during
  an incident — the worst time to need a rebuild and be unable to scope it — and
  `topics-job.yaml` still needs guarding, so it reduces no guards.
- **Fixing the topic list as a separate task.** An image that publishes cleanly
  and restores nothing is a worse state than a missing image, because the
  missing image fails loudly and the empty restore does not.

---

## 6. Testing

- **`natsrebuild` package** — extend `rebuild_test.go` for the per-topic window
  selection: a compacted topic reads from offset 0, an event topic reads from
  `now-since`, an unclassified topic is refused at startup. Tests written before
  the code (TDD), each watched failing first.
- **Both arch guards** — mutation-proven, **every arm tripped individually**:
  drop a topic from `NATS_REBUILD_TOPICS` → red naming that topic; add a topic
  that is not in the derived set → red; same both ways for `LAKE_SINK_TOPICS`;
  revert one manifest image to `:latest` → red; a stale exemption → red.
  Non-vacuity: an empty parse must `t.Fatal`, not pass.
  Per the board's standing rule, no guard is recorded as mutation-proven unless
  every arm was individually tripped and observed to fail.
- **Docker build** — built locally against the repo root; the binary runs and
  exits 2 on missing `NATS_REBUILD_TOPICS` (fail-closed, `main.go:41-44`).
- **Whole suite** — `go build ./...`, `go vet ./...`, `go test -p 1 ./...`
  (`-p 1` is required with a live Postgres; see the board's REFERENCE row).

---

## 7. Verification

### Completed locally *(what this box can prove)*

- [ ] Derived topic set computed by the guard matches the 14 in §3
- [ ] `TestArchivedTopicConsumersMatchTheArchiverProducedSet` red → green, every arm mutated
- [ ] `TestProductionManifestsPinImagesByDigest` red → green, every arm mutated
- [ ] `natsrebuild` unit tests for per-topic windows, TDD, each seen failing first
- [ ] `docker build -f kanz/tools/natsrebuild/cmd/nats-rebuild/Dockerfile .` succeeds
- [ ] `go build` / `go vet` / `go test -p 1 ./...` clean
- [ ] `TestReleaseMatrixCoversEveryBuiltService` green at 26/26

### Pending CI / runtime *(cannot be proven on this box today)*

CI is halted: the repository is private and the 09:59 scheduled `security` run
died in 3s with the billing annotation, all jobs `steps=0`. Per the owner's
decision, visibility flips to public at end of day for the final push, and these
close then — **not before, and they are not to be marked complete without the
evidence**.

- [ ] Green `kanz-build` on a push to `main` with the 26-image matrix
- [ ] `nats-rebuild` published to `ghcr.io/eighred/nats-rebuild`
- [ ] Green `release.yml` including trivy, cosign and SBOM for this image
- [ ] `pin-digests` PR rewrites `rebuild-job.yaml` to `@sha256:`
- [ ] Temporary pin exemption removed; dead-exemption arm confirms it
- [ ] `cosign verify` succeeds against the digest **from a clean machine**
- [ ] **Full DR workflow on a real cluster**: `failover.sh` runs end to end, the
      Job pulls its digest with `ghcr-pull`, publishes over mTLS with its SVID,
      and the rebuilt spine is asserted **by content, not by exit code** — a
      `compliance.mandate` written >24h before the run is present after it
      (this is the §2.3 regression, and an exit-0 assertion would miss it)

---

## 8. P0 completion criteria

P0 is **not complete** until all of the following hold. Partial completion is
recorded as partial.

1. The `nats-rebuild` image is **published** to `ghcr.io/eighred/nats-rebuild`
   by `release.yml`, signed and scanned.
2. `rebuild-job.yaml` is **digest-pinned** (`@sha256:`), with no exemption
   remaining.
3. **Both guards pass in CI** — the topic-set guard and the digest guard — each
   mutation-proven.
4. The **full DR workflow is validated on a real cluster**, and the assertion is
   on restored content, including at least one compacted-topic record older than
   the event window.

Until (4), the DR path is *implemented and unverified* — the status this board
reserves for code nothing has run.

---

## 9. Known risks and limitations

- **The derivation depends on the archiver's `DefaultSubjects` remaining the
  definition of "archived".** If a service ever writes to Kafka without going
  through the archiver, the derived set silently under-covers. Today the
  single-writer property is enforced (`archiver-deploy.yaml:67-69`,
  `replicas: 1` + `Recreate`) and DATA-M1 makes Kafka derived-from-NATS by
  construction, so this holds — but it is an assumption, and it is the one to
  re-check if a second Kafka producer is ever proposed.
- **Rebuilding republishes real events onto live subjects.** That is the design
  (idempotent handlers, dedup window), but it means running this Job against a
  *healthy* spine re-drives recent history. It is a DR tool; nothing currently
  prevents an operator running it at the wrong time. Out of scope here, worth
  its own row.
- **`market.book`/`market.crypto` are not restorable.** After a failover the
  book is cold until the venue feeds refill it. Deliberate (DATA-M1 scope), but
  it means the spine is not fully warm at cutover, and the OMS's mark-based
  MARKET/STOP admission refuses until it warms (COMP-M2 cold start).
- **`NATS_REBUILD_SINCE=24h` is a policy number.** It matches the live tier's
  longest max-age. If a stream's retention changes, this must change with it —
  no guard couples them today.
