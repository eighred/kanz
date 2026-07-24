# Node lifecycle — Maintenance (cordon/uncordon) + Drain (S3a)

**Date:** 2026-07-24
**Status:** design approved, pending implementation plan
**Direction:** cluster-management slice of the TUI-based sovereign fund-management surface, after F0+S1 / S2a / S2b. Lead-committed 2026-07-24.

## Context

The operator can see the estate (F0), provision nodes into it (S2a), and probe/observe
them (S2b). S3 is the first slice that lets the operator **change running nodes** — the
mockup's Cluster Management actions (Move / Balance / Drain / Maintenance). Mapped
against ground truth (the estate is ONE k3s cluster; "clusters" are region-label
groupings, F0's Clusters pane groups by `topology.kubernetes.io/region`), and scoped:

- **Maintenance Mode = cordon/uncordon** — mark a node unschedulable and reverse it.
- **Drain = cordon + evict** — take a node out of service, evicting its pods via the
  Kubernetes Eviction API, which enforces PodDisruptionBudgets.
- **Move Node** ("between clusters") = a `topology.kubernetes.io/region` **relabel** —
  deferred to S3b (topology, not lifecycle).
- **Balance Cluster** — **dropped**: Kubernetes has no built-in balance primitive
  (rebalancing is the descheduler, a separate operator, or cordon-and-drain by hand),
  and it is near-meaningless on the current estate. Not shipping a fake button.

Three forks were resolved before this design:

1. **Scope = Cordon/Uncordon + Drain** (node lifecycle). Move (relabel) → S3b; Balance
   dropped.
2. **Drain is async, and its status is DERIVED from live node/pod state — no stored
   drain record.** A node is `Schedulable` (not cordoned), `Draining` (cordoned + evictable
   pods remain), or `Drained` (cordoned + none remain). This mirrors how `ListProvisions`
   derives status from Job state — stateless and restart-safe (cordon persists; a
   re-issued Drain resumes). Rejected: a stored/Job-backed drain record (needless state
   for something the cluster already reports).
3. **Node writes go in a NEW `operator-node-writer` ClusterRole; the read-only
   `operator-node-reader` (and its guard) stays untouched.** Rejected: widening the reader
   role with write verbs (it would break the read-only invariant the guard exists to
   protect).

These are all **plain Kubernetes API calls made IN the operator service** — no SSH, no
Jobs (unlike S2a). So `crypto/ssh` stays out of the operator, and `TestNoSSHPlane`
stays green.

## Scope

**In:**

1. **`operator.v1`:** `Cordon`, `Uncordon`, `Drain` RPCs; the `Node` read model gains
   `schedulable` (bool) + `evictable_pods` (int32).
2. **Operator:** cordon/uncordon (patch `node.spec.unschedulable`); Drain (cordon + a
   background eviction loop over the node's evictable pods via the Eviction API,
   PDB-respecting, backing off on the 429 the API returns when eviction would breach a
   budget); the node read model extended to count evictable pods per node.
3. **RBAC:** a new **ClusterRole** `operator-node-writer` (nodes are cluster-scoped)
   granting exactly `nodes: patch` + `pods/eviction: create`; the read-only
   `operator-node-reader` gains `pods: list` (still
   read-only — it now reads pods to count evictable ones), and its guard is updated to
   allow `{nodes, pods}` read while still rejecting any write verb.
4. **TUI:** the Nodes pane gains **row selection** (up/down) and `[c]ordon` / `[u]ncordon`
   / `[d]rain` keys on the selected node; **Drain is behind a `y/n` confirm prompt**
   (Cordon/Uncordon fire directly — reversible). The status column shows
   `Schedulable` / `Cordoned` / `Draining (N)` / `Drained`.
5. **Arch guard** for the `operator-node-writer` ClusterRole (mutation-proven: no node
   `create`/`delete`, only `patch` + `pods/eviction: create`).

**Out, recorded rather than assumed away:**

- **Move Node (region relabel)** — S3b.
- **Balance Cluster** — dropped (no k8s primitive).
- **Node `create`/`delete`** — a joining node self-registers (S2a); node deletion is a
  separate, more dangerous op, not in S3a.
- **Draining DaemonSet/mirror pods** — standard drain skips them (they can't move); only
  ordinary pods are evicted.

## Design

### 1. Cordon / Uncordon

Patch `node.spec.unschedulable` (true / false) via a JSON/strategic-merge patch on the
node. Instant, idempotent (cordoning a cordoned node is a no-op). `codes.NotFound` for an
unknown node, `codes.InvalidArgument` for an empty name.

### 2. Drain

`Drain(node)` first cordons (so nothing reschedules onto it), then returns immediately
(accepted) and runs the eviction in a **background goroutine** bound to an app-scoped
context (not the request ctx — a drain outlives the RPC):

- List the node's pods (`fieldSelector spec.nodeName=<node>`).
- Skip: DaemonSet-owned pods (`ownerReferences` kind `DaemonSet`), mirror/static pods
  (the `kubernetes.io/config.mirror` annotation), and already-terminating pods.
- For each remaining pod, create a `policy/v1` `Eviction`. The Eviction API returns
  **429 TooManyRequests** when the eviction would violate a PodDisruptionBudget; on 429,
  back off and retry (a pod stays until another replica is ready) — the standard,
  PDB-safe drain behavior. A `404` (pod already gone) is success.
- The loop is bounded by a generous overall deadline; a pod that never becomes evictable
  (a permanently-blocking PDB) simply leaves the node `Draining`, which the operator can
  see and act on. Drain never force-deletes below a PDB.

### 3. Drain status — derived, not stored

The `Node` read model (F0 `estate`) gains `schedulable` (= `!node.spec.unschedulable`)
and `evictable_pods` (count of the node's ordinary — non-DaemonSet, non-mirror,
non-terminating — pods). The TUI derives the label:
`!schedulable && evictable_pods>0 → Draining (N)`, `!schedulable && evictable_pods==0 →
Drained`, `schedulable → Schedulable/Ready`. No drain record is stored anywhere.

### 4. RBAC (a new writer role; reader stays read-only)

- **`operator-node-reader` (existing, still read-only):** now `nodes: get,list` **+
  `pods: list`** (to count evictable pods for status). Its guard is updated to allow
  `{nodes, pods}` as read resources and still fail on ANY write verb.
- **`operator-node-writer` (new ClusterRole):** `nodes: patch` (cordon/uncordon) +
  `pods/eviction: create` (drain). NO node `create`/`delete`, NO pod `delete` (eviction
  only — so PDBs are always honored). Bound to the `operator` ServiceAccount. Guarded by a
  new arch test (mutation-proven).

### 5. TUI

- The Nodes pane gains a **selected row** (up/down keys move it; the selected node is
  highlighted). Keys on the selected node: `c` cordon, `u` uncordon, `d` drain.
- **`d` opens a confirm prompt** ("Drain <node>? evicts N pods [y/n]"); `y` fires
  `Drain`, `n`/`esc` cancels. `c`/`u` fire directly (reversible).
- The status column renders `Schedulable` / `Cordoned` / `Draining (N)` / `Drained` from
  the widened node fields. Results of an action surface via the normal poll (the node's
  status updates on the next fetch). `render` stays pure.

### 6. Safety

Drain evicts running workloads; on a live trading estate that is disruptive even with
PDBs (OMS at 2 replicas, venue adapters at 1). The confirm prompt makes it a deliberate
act; the Eviction API + PDBs prevent evicting below `minAvailable`; and the operator plane
is `root@universe` (authenticated, deliberate). The operator never force-deletes a pod.

## Testing

- **Cordon/Uncordon:** fake clientset — assert `node.spec.unschedulable` flips; unknown
  node → `NotFound`; empty name → `InvalidArgument`; idempotent.
- **Drain:** fake clientset with a **reactor capturing `create` on the `pods/eviction`
  subresource** — assert the node is cordoned first, that ordinary pods are evicted and
  DaemonSet/mirror/terminating pods are NOT, and that a 429 (PDB) reactor causes a
  retry/backoff rather than a force-delete.
- **Estate read model:** `evictable_pods` counts ordinary pods on a node and excludes
  DaemonSet/mirror ones (fake clientset with mixed pods).
- **Arch guards:** the updated `operator-node-reader` guard (now allows `{nodes, pods}`
  read, still rejects writes — mutation-proven on a planted write verb); the new
  `operator-node-writer` guard (only `nodes: patch` + `pods/eviction: create`;
  mutation-proven on a planted `nodes: delete`). `TestNoSSHPlane` stays green (no
  `crypto/ssh` — these are k8s API calls).
- **TUI:** row selection moves with up/down; `d` opens the confirm; `y` fires Drain via a
  stub source; the status column renders each state. `render` pure.

## Definition of done

From `universe`, the operator selects a node, presses `c` (it shows `Cordoned` on the next
poll), `u` (back to `Schedulable`), and `d` → confirm → the node shows `Draining (N)` then
`Drained` as pods evict. The operator handlers are unit-proven (cordon flip, drain evicts
only ordinary pods, PDB-429 retries); the two RBAC guards are green and mutation-proven;
`TestNoSSHPlane` stays green; `render` pure; `govulncheck` 0-reachable. **Fully
rig-provable on the kind rig** — cordon/drain are real k8s operations there (no k3s
needed), so unlike S2a this slice's core is live-provable here.

## Sequencing (recorded, not part of this slice)

- **S3a (this spec)** — Maintenance (cordon/uncordon) + Drain.
- **S3b** — Move Node (region relabel: `topology.kubernetes.io/region` patch), and any
  reframe of "Balance".
- **S4** — API Manager (exchange keys → Vault).
- **S5** — Setup Wizard (first-node bootstrap chicken-and-egg).
- **Control-plane bring-up (infra-gated)** — a k3s server + rig: S2a's live join, S2b's
  deferred addons, node-exporter scraping.
