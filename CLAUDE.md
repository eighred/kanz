# kanz

The Eighred institutional trading and risk platform. Go monorepo under `kanz/`,
Protobuf contracts under `kanz-schemas/`, the Python inference service under
`kanz-py/`.

## Where things live

Two layers. Nothing is tracked in two places.

| Layer | Home |
|---|---|
| **What to do** | **GitHub Issues** on `eighred/kanz` — milestones `M0`…`M6` carry the sequence |
| **Why it is shaped this way, and what happened** | the code (in comments beside what they explain), git history, and claude-mem |

There is no task board in this repository, and no architecture document. Both
were tried and both failed the same way.

`KANZ_TASKS.md`, `KANZ_ROADMAP.md` and the Phase 0/2/3 documents went on
2026-07-28: five documents each claimed to say what to do next and none deferred
to the others. `docs/` (63 files of plans and specs) and `KANZ_BRAIN.md` went on
2026-07-29 for the same reason one level up — a plan that outlives its work
becomes a second answer to "what should we do", and a prose description of the
system becomes a second answer to "how does it work". Both compete with a source
that is always right when they are wrong.

**Everything durable lives next to the code or in claude-mem.** That is a
working conclusion, not a preference. The explanations that survive in this
repository are the ones sitting beside what they explain — the OMS's incident
notes, the DLQ retry certifications, the tombstones in `infra/observability/`,
the arch guards in `test/arch/`. When those drift, a reader finds it, because
they are read while the code is being changed. A separate document drifts
unread: `KANZ_BRAIN.md`'s own system-shape section ended up naming a Go version
and a module path the repository had both moved away from, in one sentence,
while the code was correct throughout.

**An invariant worth keeping is a guard, not a paragraph.** The strongest form
of "we deliberately do not do X" is a test in `test/arch/` that fails when
someone does X — default-deny, with named exemptions that carry the issue
retiring them, and a dead-entry check so an exemption cannot outlive its repair.
`bus_dlq_test.go`, `observability_metrics_test.go`, `dr_postgres_coverage_test.go`
and `baseimage_mirror_test.go` are the pattern. Everything is recoverable from
git history if it is ever wanted back.

## Issue conventions

Every issue carries exactly one **kind** label — `open-work`,
`needs-verification`, `blocked-external`, `decision-needed` — one **priority**
(`P0`/`P1`/`P2`), and an **area**.

`needs-verification` means *the code is merged and nothing has run it*. It is a
distinct state from done, and it is tracked because this repository has repeatedly
found that a merged fix, a green suite and a written claim are not evidence.

An issue body states four things: **Evidence** (a `file:line`, a commit, or a
quoted command result), **Verified when** (a runnable command and its expected
result), **Blocked by**, and **Source**. An issue that cannot say how it will be
proven does not belong on the board.

## The engineering bar

The standard for what is worth building, and what "good" means here, is the
`kanz-forge:engineering-standard` skill. It is not restated in this file.

## Sequencing constraints that outlive any one issue

- **M0 first, and not partially.** Everything after it is verified by a pipeline
  that cannot presently complete a merge unaided.
- **M3 must not precede the DR-coverage issue in M0.** Placing real orders against
  a store that may not be backed up is the one ordering error with an
  unrecoverable failure mode.

## Working notes

- Go tests race on a shared `TEST_POSTGRES_URL` — run `go test -p 1 ./...`.
- Postgres-gated tests skip silently without `TEST_POSTGRES_URL`, and must run as
  a **NOSUPERUSER** role or RLS is bypassed and the isolation tests pass falsely.
- `fakeBus` does not validate envelopes, so it accepts what a real broker rejects.
  A green suite using it is not a broker proof.

## Promotion rule

A direction becomes an issue only after a ground-truth verification confirms the
gap still exists and is now buildable. Roadmaps go stale; code does not. If
verification disproves the item, it is discarded rather than tracked — and the
codebase is always the ground truth, ahead of any document including this one.
