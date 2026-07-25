# TUI end-to-end workflow verification (OPS-M2e)

Slice 1 of five in the operational-console programme. It builds the harness that
proves the operator TUI's workflows and fixes what that harness finds. It adds no
feature.

## Context

OPS-M2c was reported as verified because the TUI authenticated to the api-gateway with
no kubeconfig and rendered a real node. That proved the **communication path**. Not one
keypress had been sent, so no operator workflow — navigation, Add Node, cordon,
uncordon, drain, move region, venue keys, the confirm dialog, the live poller — had
been exercised at all. `SetVenueKeys` was driven over HTTP with curl, not through the
form that is supposed to call it.

The readiness board now records that distinction and carries the rule it produced: *a
working transport is not evidence; reaching the server is a precondition, not the
proof.* This slice discharges it.

Two board rows have also sat unexercised since they were written, both with a
"next action" describing the `kubectl port-forward` path that OPS-M2c deleted:

- **S2a node provisioning** — `AddNode` has never been invoked by any client, and the
  live k3s join was infra-gated because the rig's single node is already a k3s server.
- **S2b Test Connection** — `ctrl+t` has never been pressed.

This slice closes both.

### Why this comes before the UX work

The programme's later slices rewrite the interface: a command palette, a panel
architecture, an activity view, a new auth model, a new entry point. Re-plumbing six
workflows that have never run would make every failure ambiguous — new shell, or a
latent bug that was always there? The harness built here is what makes those slices
safe, and it is cheapest to build against the interface as it stands.

## Scope

**In:** a PTY harness and a model-level test expansion; the seven workflow proofs
below; a second cluster node so drain and the live join are provable; fixes for
whatever the harness finds.

**Out, and deliberately:** the command palette, the panel architecture, live log
streaming, progress indicators, the activity view, the authentication replacement, the
`kanz` entry point, and every workflow that does not exist yet (logs, image digests,
rollback, restart, backups). Those are slices 2–5. Adding any of them here would mean
verifying a moving target.

## Prerequisites

1. **A second host.** Operator-supplied: a small instance, SSH-reachable, with the
   public half of a key the operator holds in its `authorized_keys`. It is NOT
   pre-joined to the cluster — the point is that the TUI joins it.
2. **The `operator-k3s-join` Secret** in `kanz-operator`, holding `server_url` and
   `token`. `operator-deploy.yaml` already references it with `optional: true`, so the
   operator currently starts without it and `AddNode` would create a Job that cannot
   join. The token is read from node 1 at `/var/lib/rancher/k3s/server/node-token`.
3. **`github.com/creack/pty`** as a test-only dependency. `go.sum` carries only its
   go.mod hash today, so it needs fetching. It does not enter any service binary's
   dependency graph; the "no vendor dependency in the default build" rule is about
   shipped binaries and `go list -deps` on those is unchanged.

## Design

### 1. Two harnesses, split by what they can prove

Neither alone is sufficient, and the split is along a real seam rather than
convenience.

**PTY harness — `kanz/test/e2e/tui/`.** Spawns the real binary in a pseudo-terminal
against the real gateway and cluster. This is the only thing that can prove a keypress
reaches Kubernetes. It is slow and needs infrastructure, so it covers the seven
workflows and nothing else.

**Model-level tests — `cmd/universe`, extending the existing `model_test.go`.** Drive
`Update()` with synthesized `tea.KeyMsg` and assert on `render()`. Milliseconds, no
cluster, no TTY. They cover the combinatorial surface the PTY harness must not: every
navigation permutation, every validation branch, every error path.

### 2. The PTY driver

A small package, not a framework. Four operations:

- `Start(env)` — spawn the binary in a pty at a fixed 120x40, returning a handle.
- `Send(keys)` — write keystrokes, including control sequences.
- `WaitFor(substring, timeout)` — read frames until one contains the substring, or
  fail with the last frame attached to the error.
- `Close()` — send `q`, then kill on timeout.

**Assertions match stable substrings, never layout.** Slice 2 rewrites every frame in
this program; a harness coupled to column positions or box-drawing characters would
break on cosmetics and be deleted within a week, taking the regression net with it.
What is stable is domain text: a node name, `Ready`, `Failed`, an error message.

### 3. Gating, so a cluster-less run is honest

The PTY tests are skipped unless `KANZ_E2E_GATEWAY`, `KANZ_E2E_TOKEN` and
`KANZ_E2E_NODE` are set. A skip is not a pass: the suite prints what it skipped and
why, and the readiness board records these workflows as verified only against a run
where they executed. This mirrors the existing `TEST_POSTGRES_URL` and
`TEST_KAFKA_BROKERS` convention.

### 4. The seven proofs, each with an oracle that is not the TUI

The oracle is always `kubectl` or the cluster API. A UI that renders its optimistic
intent rather than observed state would pass a screen-only check, which is the failure
mode this whole slice exists to rule out.

| # | Workflow | Drive | Oracle |
|---|---|---|---|
| 1 | Navigation | `tab` ×4, `q` from each pane and mid-form | frames show all three panes; process exits 0; mid-form `q` does not submit |
| 2 | Add Node + Test Connection | `[a]`, fill the form, `ctrl+t`, save | `ctrl+t` shows reachable for node 2's SSH port and unreachable for a closed port on the same host; a provisioning Job is created; **the Job-owned SSH-key Secret** (the transient one the operator mints per provision — not the long-lived `operator-k3s-join`) carries an owner reference and is GC'd when the Job is deleted; the provisioning strip advances; **node 2 reaches Ready in `kubectl get nodes`** |
| 3 | Cordon / uncordon | `[c]` then `[u]` on node 2 | `.spec.unschedulable` flips true then false |
| 4 | Drain, both branches | `[d]` then `n`; `[d]` then `y` | on `n`, nothing evicted and the node stays schedulable; on `y`, node 2's pods are evicted, PodDisruptionBudgets are honoured, and no pod on node 1 moves |
| 5 | Move region | `[m]` on node 2 | `topology.kubernetes.io/region` changes |
| 6 | Venue keys | `[k]`, submit a dummy credential | Secret `venue-binance-keys` exists with **hyphenated** `api-key`/`api-secret`; the credential string appears in none of the captured PTY frames, and in neither the gateway's nor the operator's logs for the window (asserted by searching both, so the leak check is executed rather than assumed) |
| 7 | Live updates | `kubectl cordon` node 2 out of band | the TUI reflects it without a restart, within a bound of two poll intervals plus one — 12s at the 3s default — so the assertion fails on a stalled poller rather than waiting indefinitely |

Test 4 runs against **node 2 only**, which is why the second host is a prerequisite
rather than a nicety: draining the rig's single node evicts the trading loop, NATS and
Postgres, and mass rescheduling on a 2-vCPU box is a real risk to a working demo.

Test 2 is the one that closes S2a's live join and S2b's `ctrl+t` together.

### 5. Fixes are in scope

Nothing here has ever run, so some of it will not work. A failure found by this harness
is fixed in this slice, with the fix covered by the test that caught it. The slice is
not "write tests"; it is "make the workflows work, proven".

### 6. What the harness must not become

No retry loops around flaky assertions, no sleeps standing in for `WaitFor`, no
screen-scraping of layout. If a test needs any of those to pass, the finding is about
the TUI's feedback — a workflow whose completion cannot be observed deterministically
is a workflow an operator cannot trust either, and that is a defect to fix rather than
paper over.

## Testing

The harness is the test. Beyond it:

- Model-level tests for navigation, validation and error permutations.
- Each PTY test asserts its oracle, not its own rendering.
- The suite runs on the VDS, where the cluster is. It is not part of the default
  `go test ./...` run unless the environment is set.

## Definition of done

1. All seven proofs execute and pass on a real two-node cluster, with output captured.
2. Every failure the harness found is fixed, each with a covering test.
3. The board's S2a and S2b rows close, and the OPS-M2c row moves from
   TRANSPORT VERIFIED ONLY to verified per workflow — listing any that remain gated
   and why.
4. Model-level coverage exists for navigation, validation and error paths.
5. The captured run is the demonstration: every existing operator workflow driven from
   the TUI, with no curl and no kubectl except as the oracle.

**Not claimed by this slice:** that the TUI is a modern operational console, that
authentication is production-shaped, or that routine administration needs no shell.
Those are slices 2–5, and the honest summary after this one is "every existing workflow
works and is proven" — not "the TUI is complete".

## Sequencing (recorded, not part of this slice)

- **Slice 2 — UX architecture.** Command palette and searchable actions, a
  context-aware panel system replacing fixed panes, a unified activity view, progress
  indicators, inline validation, consistent layout primitives.
- **Slice 3 — `kanz` entry point.** One cross-shell binary that launches the console by
  default, with config discovery and startup diagnostics; subcommands for automation.
- **Slice 4 — Authentication.** Replaces the static bearer token with the intended
  model. **Externally blocked:** the gateway authenticates via OIDC against
  `login.eighred.com`, which is not reachable from this environment — the same class of
  blocker as the CI billing halt. The design work can be done; the verification cannot,
  until that IdP exists.
- **Slice 5 — Missing workflows.** Live log streaming, service restart, image-digest
  rollout and rollback (OPS-M4), health, cluster status, per-service DSNs, backup and
  restore (OPS-M5).
