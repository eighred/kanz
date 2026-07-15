# DATA-M3 — Archiver Consume↔Topic Contract Guard

**Date:** 2026-07-15
**Status:** Approved design, pre-implementation
**Scope:** Add a build-time guard that the DATA-M1 archiver's consume-set (`DefaultSubjects`) does not drift ahead of the provisioned Kafka topic topology, and make the three currently-unbacked subjects (`risk.exposure/signal/command.>`) a deliberate, documented exemption rather than a silent latent trap. One guard + an allowlist. No runtime change.

---

## 1. Problem & ground truth

The archiver drains NATS `DefaultSubjects` (`services/archiver/internal/config/config.go:19-35`) into Kafka, deriving each event's topic from its `event_type`. Kafka auto-create is disabled, so a subject the archiver consumes but has no provisioned topic for is a **fail-closed NACK-loop** the moment anything publishes on it.

Verified 2026-07-15: `DefaultSubjects` lists seven `{domain}.{entity}.>` subjects; six months of topology has topics for four of them, but **`risk.exposure`, `risk.signal`, `risk.command` have no Kafka topic** (`infra/kafka/topics-job.yaml` backs only `risk.portfolio`, `risk.position`). Nothing publishes those three today (test fixtures only), so it is dormant. The existing `TestEverySubjectHasAKafkaTopic` (`test/arch/`) checks *published* subjects, not the archiver's *consume-set*, so **neither existing arch guard catches this** — the archiver's consume-set can silently drift ahead of the topology.

## 2. Design

### 2.1 The invariant, and why it is fail-loud not fail-remove

A new guard **`TestArchiverConsumeSetHasBackingTopics`** in `test/arch/` (package `arch`): every archiver `DefaultSubjects` subject of the form `{domain}.{entity}.>` must have a provisioned Kafka topic named `{domain}.{entity}`, **or** be on an explicit allowlist with a written reason.

**`DefaultSubjects` is left unchanged — the three unbacked subjects stay subscribed, by design.** This is the archiver's own fail-loud-never-silent rule (`KANZ_BRAIN.md`: "acking an event you failed to archive is the silent permanent loss the service exists to end"). If the archiver STOPPED subscribing `risk.exposure.>`, then a future publisher added without re-subscribing the archiver would be **silently never archived** — a silent gap in the log of record, the cardinal sin for a durable log. Keeping the subscription means a premature publisher hits a **loud NACK-loop** — discoverable, fixable by provisioning the topic. The guard's job is therefore NOT to remove the trap (that loud failure is the safety mechanism) but to make the current unbacked state **explicit** and to force **topic-first** for anything *new*.

### 2.2 Subject → topic mapping (the precise, honest check)

`DefaultSubjects` entries are NATS wildcard patterns, not topic names. Only **two-segment** subjects map to a single topic:

- `{domain}.{entity}.>` (two segments before `>`, e.g. `risk.exposure.>`) ⇒ the topic is `{domain}.{entity}` — **checked**.
- `{domain}.>` (one segment, a domain wildcard, e.g. `order.>`) ⇒ covers many entities, no single topic — **skipped** (these are already covered from the publisher side by `TestEverySubjectHasAKafkaTopic`).

So the guard checks exactly the seven two-segment subjects (`compliance.breach`, `compliance.mandate`, `risk.portfolio`, `risk.exposure`, `risk.signal`, `risk.command`, `risk.position`) and flags the three without a topic.

### 2.3 The allowlist

An in-test map, the `deployability_test.go` "exempt-with-a-written-reason" shape:

```
unbackedByDesign = {
  "risk.exposure": "no publisher yet — the RISK stream carries it but only fixtures emit it; provision a topic matched to real volume AND remove this line when a publisher is designed (DATA-M3)",
  "risk.signal":   "same — no publisher yet (DATA-M3)",
  "risk.command":  "same — no publisher yet (DATA-M3)",
}
```

A `{domain}.{entity}.>` subject with no topic AND not on this allowlist ⇒ the build fails. Adding a *new* unbacked two-segment subject to `DefaultSubjects` therefore fails the build (topic-first enforced); the three known ones are deliberate and documented; and if any of the three *gains* a topic, the guard should also flag that it is now redundantly allowlisted (an allowlist entry whose topic now exists is stale) — keeping the allowlist from rotting.

### 2.4 Implementation

Reuse the existing package-`arch` helpers: `moduleRoot(t)`, `provisionedTopics(t, path)` (both present), and the `go/parser`+`go/ast` machinery already used by `declaredSubjects` (`subject_topology_test.go`). Add one helper `archiverDefaultSubjects(t, root) []string` that parses `services/archiver/internal/config/config.go`, finds the `DefaultSubjects` `var` declaration, and returns its string-literal elements. (`test/arch` cannot import the archiver's `internal/config` — Go's internal rule — so it reads the source, consistent with how the file already AST-walks the code.)

## 3. Testing

The guard IS the test. Its correctness is verified by:
- It **passes** on the current tree (the three unbacked subjects are allowlisted; the four backed ones resolve to topics; domain wildcards are skipped).
- A **mutation check** (manual, during implementation, reverted before commit): removing a `risk.exposure` allowlist entry ⇒ the guard FAILS naming `risk.exposure`; adding a fake two-segment `foo.bar.>` to a copy of `DefaultSubjects` (or asserting via a table) ⇒ FAILS. Confirm the guard bites, not just passes vacuously — assert the parsed `DefaultSubjects` is non-empty and includes a known entry, so a parse failure cannot pass silently.
- `go test ./test/arch/` green; `go build ./... && go vet ./...` clean.

## 4. Scope boundary & non-goals

- **In:** the `TestArchiverConsumeSetHasBackingTopics` guard + `archiverDefaultSubjects` helper + the reasoned allowlist; the stale-allowlist-entry check.
- **Out (deliberate):** provisioning topics for `risk.exposure/signal/command` (speculative — guessing partition/retention shapes for a nonexistent publisher; do it WITH the publisher); removing subjects from `DefaultSubjects` (would trade a loud NACK-loop for a silent-loss path); any runtime/archiver-code change; any change to the existing published-subjects guard.

## 5. Files touched

- `test/arch/archiver_topology_test.go` (new) — the guard, the allowlist, and `archiverDefaultSubjects`.

One focused change (~0.5–1 day): a single build-time guard that turns a silent consume↔topic drift into a build failure, and makes today's three unbacked subjects a documented, deliberate exemption — on the durable-log producer this session made foundational.
