# kanz

The Eighred institutional trading and risk platform. Go monorepo under `kanz/`,
Protobuf contracts under `kanz-schemas/`, the Python inference service under
`kanz-py/`.

## Where things live

Four layers. Nothing is tracked in two places.

| Layer | Home |
|---|---|
| **What to do** | **GitHub Issues** on `eighred/kanz` — milestones `M0`…`M6` carry the sequence |
| **Why it is shaped this way** | `KANZ_BRAIN.md` — durable architectural decisions and anti-decisions |
| **What happened** | git history and claude-mem |
| **Specs and plans** | `docs/superpowers/{specs,plans}/` |

There is no task board in this repository. `KANZ_TASKS.md`, `KANZ_ROADMAP.md` and
the Phase 0/2/3 documents were deleted on 2026-07-28; five documents each claimed
to say what to do next and none deferred to the others.

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
