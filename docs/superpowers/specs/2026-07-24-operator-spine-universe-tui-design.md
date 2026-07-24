# The operator spine and the Universe TUI (read-only F0+S1)

**Date:** 2026-07-24
**Status:** design approved, pending implementation plan
**Direction:** first slice of the TUI-based sovereign fund-management surface — a
read/write operator control plane fronted by a terminal UI. Lead-committed 2026-07-24.

## Context

The lead has committed to a TUI-based fund-management surface: an operator (`root@universe`)
manages the estate — nodes, clusters, exchange API keys, funds — from a terminal UI
rather than a web SPA. The full vision is five subsystems (Setup Wizard, Node
Management + provisioning, Cluster Management, API Manager, and the TUI shell that
hosts them). This spec covers **only the first slice** — the foundation the other
four stand on.

Two foundational decisions were made before any design, because they gate everything
downstream. Both were resolved toward the architecture that already exists:

1. **Provisioning substrate — hybrid TUI + k3s join.** The lead's "Add Node" flow
   (type an IP + SSH key, auto-install) is preserved, but it provisions a **k3s
   agent that joins the existing Kubernetes estate**, not a raw Docker host. This
   *refines* `KANZ_BRAIN.md:143` ("THE ESTATE IS KUBERNETES-MANAGED; SSH-pushed
   hosts are CANCELLED") rather than reversing it: there remains **one orchestration
   substrate**, and `crypto/ssh` re-enters only at node-join time (subsystem S2), not
   across the trading data plane. Reactivating SSH provisioning as a *parallel* host
   fleet was rejected — it is the parallel system this repo forbids, and it would make
   the 13 dismissed `x/crypto/ssh` Dependabot alerts (7 Critical) live and reachable
   across the platform.

2. **Control-plane shape — thin TUI, in-cluster operator service.** All privileged
   work (k3s join-token minting, Vault key writes, k8s cordon/drain) runs in a new
   **in-cluster operator service** on the `root@universe` plane. The TUI is a thin
   client that holds **no** credentials. This matches `KANZ_BRAIN.md:143` ("the
   operator TUI, if it is ever built, is a client of the control plane") and the
   two-planes rule (`KANZ_BRAIN.md:144`). A fat TUI holding SSH keys + a Vault token +
   a kubeconfig on the laptop was rejected: it puts `root@universe` credentials on a
   laptop and re-opens "exchange keys reachable from a client path" (`KANZ_BRAIN.md:141`).

3. **Operator authentication — kubeconfig-gated.** The laptop TUI reaches the
   in-cluster operator service through the Kubernetes API (port-forward); the
   operator's **kubeconfig is the credential**, scoped by RBAC. This reuses the exact
   mechanism `kanz-halt` operators already use (kubeconfig → schedule a Job → the
   Job's ServiceAccount gets an SVID in-cluster). A SPIFFE SVID cannot be held by a
   laptop — SVIDs are issued to in-cluster workloads — so a dedicated operator PKI was
   the only alternative, and it was rejected as a second identity system to issue,
   rotate, and revoke. The in-cluster service still acts under its own SVID.

Nothing today provides an operator control-plane surface. `root@universe` exists
(SEC-M3c: a `kanz-operator` namespace + `kanz-halt` ServiceAccount) but reaches only
the broker to fire the kill-switch. The read-only TUI half of the estate exists
(`kanz-monitor`, a Bubble Tea bus observer under the steal-nothing arch guard) but it
watches the *trading loop*, not the *operator plane*.

## Scope

**In — the read-only spine, and only that:**

1. A new in-cluster **operator service** (`services/operator` + `cmd/operator`),
   long-running gRPC over mTLS, in the `kanz-operator` namespace, holding a
   least-privilege ServiceAccount (`get`/`list` nodes, nothing else).
2. A new `operator/v1` proto with `OperatorService` exposing two read RPCs —
   `ListNodes` and `ListClusters` — generated through the existing `buf` SDK pipeline.
3. A new terminal UI binary, **`universe`** (`cmd/universe`), a thin gRPC client that
   lists the estate read-only in two panes (Nodes, Clusters), reusing `kanz-monitor`'s
   Bubble Tea patterns and shared components.

**Out — recorded rather than assumed away:**

- **All writes, provisioning, and the wizard.** No "Add / Edit / Delete / SSH" keys,
  no k3s join, no Vault writes. Those are S2 (node provisioning), S3 (cluster ops),
  S4 (API manager), S5 (setup wizard), each its own spec.
- **`crypto/ssh`.** This slice introduces **zero** SSH surface. Reactivation is
  isolated to S2, by design.
- **The `CPU` / `RAM` columns** from the lead's mockup. These need metrics-server;
  v1 shows only what the k8s API itself knows about a node. Wired when metrics land.
- **The `Profit` column.** That is a per-node fund-P&L join and cannot exist until
  funds do. It arrives with the fund surface, not the spine.
- **A persistent operator identity beyond kubeconfig.** Break-glass reachability
  (`KANZ_BRAIN.md:177`) is unchanged by this slice and is not re-opened here.

## Design

### 1. Architecture and data flow

```
Laptop:  universe (Bubble Tea TUI, thin gRPC client)
            │  kubeconfig-gated port-forward → gRPC/mTLS
            ▼
Cluster (ns kanz-operator):  operator service (SVID, client-go)
            │  ServiceAccount RBAC: get/list nodes
            ▼
         Kubernetes API  →  Node objects, labels, conditions
```

The operator service is a **long-running gRPC service**, not a one-shot Job like
`kanz-halt`. The distinction is deliberate: `kanz-halt` fires once and exits; a TUI
refreshes interactively, so spawning a Job per refresh would be the wrong shape. The
service reads the k8s API through `client-go` and returns a stable projection to the
TUI — the same "fold the source into a bounded read model" discipline the rest of the
estate uses, applied to the node inventory.

### 2. The `operator/v1` proto

A new `operator/v1/operator.proto` defines:

- `OperatorService.ListNodes(ListNodesRequest) returns (ListNodesResponse)` — each
  `Node` carries: `name`, `status` (an enum mapped from the `Ready` condition:
  `READY` / `NOT_READY` / `UNKNOWN`), `roles` (repeated, from
  `node-role.kubernetes.io/*` labels), `region` (`topology.kubernetes.io/region`),
  `kubelet_version`, and `age` (derived from `CreationTimestamp`, rendered
  client-side).
- `OperatorService.ListClusters(ListClustersRequest) returns (ListClustersResponse)`
  — each `Cluster` is a region grouping: `name` (the region label value), and
  `online` / `offline` node counts. This is the Europe / Asia / USA view.

Generated through the existing `buf` pipeline into the local SDK — no parallel
codegen path. The proto is additive; it touches no existing package.

### 3. The operator service

- **Package:** `services/operator`, binary `cmd/operator`. gRPC server with mTLS,
  presenting the `kanz-operator` SVID (the identity SPIRE already issues in that
  namespace).
- **RBAC:** a ServiceAccount bound to a Role granting exactly `get` and `list` on
  `nodes` — nothing else. Least privilege is enforced from the first commit, not
  retrofitted.
- **Read model:** on each RPC the service lists Nodes via `client-go`, maps the
  `Ready` condition to the status enum, extracts roles/region from labels, and groups
  for `ListClusters`. No caching in v1 — the node inventory is small and changes
  slowly; a cache is premature until a real refresh rate demands it.
- **Deny-by-default status mapping:** a node whose `Ready` condition is absent or
  `Unknown` maps to `UNKNOWN`, never silently to `READY`. An offline node must read as
  offline.

### 4. The `universe` TUI

- **Binary:** `cmd/universe`, named for the lead's own "Welcome to Universe."
- **Reuse, not fork:** it reuses `kanz-monitor`'s Bubble Tea patterns and shared
  table/pane components. It is a **separate binary**, because `kanz-monitor` is a
  read-only *bus observer* (bound by the steal-nothing arch guard,
  `TestReadOnlyObserversNeverJoinAQueueGroup`) while `universe` is a *gRPC client* of
  the operator service — a different plane, kept architecturally distinct so neither
  guard nor identity leaks across.
- **Transport:** the TUI establishes a kubeconfig-gated port-forward to the operator
  pod and speaks gRPC over it. The operator's kubeconfig RBAC is the authorization
  gate; the TUI presents no credential of its own.
- **Panes:** two, matching the mockups — **Nodes** (name, status, roles, region,
  version, age) and **Clusters** (region, online/offline counts). Read-only: the
  `[A]dd` / `[E]dit` / `[D]elete` / `[S]SH` keys from the mockup are **not** wired in
  this slice.

### 5. Security posture

- **No new identity system.** kubeconfig in, SVID in-cluster — both already exist.
- **No credential on the laptop** beyond the kubeconfig the operator already holds.
- **No write path, no SSH, no Vault reach** in this slice. The most dangerous
  surfaces (provisioning, key material) are deliberately deferred to later specs so
  the spine can be proven safe first.
- **Two planes preserved:** the operator service is SVID-only in-cluster and reachable
  only via kubeconfig-gated k8s API access; no OIDC `/v1` principal can reach it, and
  the operator plane grants no `Trade`.

## Testing

- **Operator service:** unit tests over `client-go`'s `fake.NewSimpleClientset` —
  assert node→cluster region grouping, the `Ready`→status enum mapping (including the
  deny-by-default `UNKNOWN` for absent/unknown conditions), and empty-estate behavior
  (zero nodes → empty responses, not an error).
- **Arch guard:** a build-time test asserting the operator service's RBAC declares
  **only** `get`/`list` on `nodes` — mirroring the repo's habit of guarding privilege
  at build time (SUPPLY-M1, the capability model, steal-nothing). A write verb
  appearing in the operator Role must fail the build.
- **TUI:** render tests against a stub `OperatorService` implementation — no live
  cluster required — asserting both panes render the projected fields.

## Definition of done

`universe`, run on the operator's laptop against the dev rig with a kubeconfig-gated
port-forward, lists the rig's real nodes and their region groupings — read-only, over
an SVID-authed operator service scoped to `get`/`list` nodes, with unit tests, the
RBAC arch guard, and TUI render tests green, and `go build`/`vet`/`gofmt` clean.

## Sequencing (recorded, not part of this slice)

This slice is F0+S1 of a five-subsystem platform. Dependency order, lead-approved
2026-07-24:

1. **F0+S1 (this spec)** — operator spine + read-only Universe TUI.
2. **S2** — node provisioning (k3s join over SSH); `crypto/ssh` re-enters here, bounded.
3. **S4** — API Manager (exchange keys → Vault CSI, via the operator service).
4. **S3** — Cluster Management (move / balance / drain / maintenance → k8s primitives).
5. **S5** — Setup Wizard, which also owns the first-node **bootstrap chicken-and-egg**:
   the first node cannot be provisioned *by* an in-cluster service that does not yet
   exist, so first-launch is a one-shot local installer, after which the operator
   service drives every node #2..N.
