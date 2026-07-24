# Move Node — region relabel (S3b)

**Date:** 2026-07-24
**Status:** design approved, pending implementation plan
**Direction:** cluster-management slice of the TUI-based sovereign fund-management surface, after S3a (node lifecycle). Lead-committed 2026-07-24.

## Context

S3a delivered node lifecycle (cordon/uncordon/drain). This slice adds the mockup's
remaining Cluster-Management action — **Move Node**. Since the estate is ONE k3s
cluster and "clusters" are region-label groupings (F0's Clusters pane groups by
`topology.kubernetes.io/region`), "moving a node between clusters" is a **relabel** of
that region label, not a move between real clusters. (The fourth mockup action, Balance,
was dropped in S3a — Kubernetes has no built-in balance primitive.)

Two facts shape this slice, both making it small:

1. **No new RBAC.** Relabeling is a patch on `node.metadata.labels`, and the S3a
   `operator-node-writer` ClusterRole already grants `nodes: patch`. So S3b reuses the
   existing writer role and its arch guard unchanged — no RBAC, no guard change.
2. **A relabel is safe, instant, and non-destructive** — it evicts nothing and cordons
   nothing. So (unlike Drain) it needs **no confirmation prompt**; it fires directly.

One fork was resolved: the operator specifies the target region as **free-form text**
(type any region), which supports both moving into an existing grouping and creating a
new one; Kubernetes validates the label value server-side. Rejected: a picker limited to
existing regions (can't create a new one without a text fallback, and needs a picker
widget).

## Scope

**In:**

1. **`operator.v1.SetNodeRegion`** RPC (`name`, `region`).
2. **Operator:** `nodeops.SetRegion` — a strategic-merge patch setting
   `node.metadata.labels["topology.kubernetes.io/region"]` to the given value; the gRPC
   handler (reusing S3a's `nodeWrite` guard: nil ops → Unimplemented, empty name →
   InvalidArgument, k8s NotFound → NotFound, else Internal), plus rejecting an empty
   `region` as InvalidArgument.
3. **TUI:** an `m` key on the selected node opens a **single-line region input** (`Move
   <node> to region: ___  [enter] move  [esc] cancel`); Enter fires `SetNodeRegion`. Fires
   directly — no confirm (a relabel is non-destructive). The node's Region + its Clusters
   grouping update on the next poll.

**Out, recorded rather than assumed away:**

- **New RBAC / a guard change** — the writer role's `nodes: patch` already covers relabel.
- **Clearing the region** (empty region) — a Move targets a region; empty is rejected.
  Un-grouping a node is not a use case S3b serves.
- **Server-side label-value validation in the operator** — Kubernetes validates the label
  value on patch; an invalid value surfaces as an error, not re-implemented client-side.
- **Balance Cluster** — dropped (no k8s primitive; decided S3a).

## Design

### 1. `operator.v1.SetNodeRegion`

`rpc SetNodeRegion(SetNodeRegionRequest) returns (SetNodeRegionResponse)`. Request:
`name`, `region`. Response: empty ack. Additive to the proto.

### 2. Operator — `nodeops.SetRegion`

`func (o *Ops) SetRegion(ctx, name, region string) error` — a strategic-merge patch on
the node setting the region label, built with `json.Marshal` (not string formatting) so
an odd region value can't break the patch:

```
{"metadata":{"labels":{"topology.kubernetes.io/region":"<region>"}}}
```

An empty name or region is rejected before the patch (`InvalidArgument` at the handler;
`nodeops` may also guard). An unknown node returns a k8s `IsNotFound` (→ `codes.NotFound`).
An invalid label value is a k8s patch error (→ `codes.Internal`, surfaced to the TUI). The
`gRPC` handler routes through S3a's existing `nodeWrite` helper for the nil-ops / empty /
NotFound / Internal mapping, with an extra empty-region check.

The `topology.kubernetes.io/region` constant already lives in `estate` (F0) — reuse it
rather than redefine.

### 3. TUI

- An `m` key on the selected node (Nodes pane) opens a **region input** — a small text
  field prompt (`Move <node> to region: <typed>`), one line, like the drain confirm but
  editable. Enter submits `SetNodeRegion(selectedNode, typed)`; Esc cancels; runes/back-
  space edit the value. This is `universe`'s move-input mode.
- Fires directly (no `y/n` confirm — a relabel is non-destructive). On success the input
  closes; the node's Region and its Clusters-pane grouping update via the normal poll. A
  failed relabel sets `actionErr` (shown in a status line), never a crash. `render` stays
  pure.

## Testing

- **`nodeops.SetRegion`:** fake clientset — patch a node's region label, assert
  `node.Labels["topology.kubernetes.io/region"] == region`; empty name/region → error;
  unknown node → `IsNotFound`.
- **Handler:** delegation (name+region reach the stub); empty name → InvalidArgument;
  empty region → InvalidArgument; nil ops → Unimplemented.
- **TUI:** `m` opens the region input; typing edits the value; Enter fires the
  `SetNodeRegion` command with the selected node + typed region; Esc cancels. `render` pure.
- **Arch guards unchanged and still green** — no RBAC change, `TestNoSSHPlane` green (a
  k8s patch, no `crypto/ssh`), the writer guard still passes (relabel uses the already-
  granted `nodes: patch`).

## Definition of done

From `universe`, the operator selects a node, presses `m`, types a region, and Enter; on
the next poll the node's Region shows the new value and it moves to that region's grouping
in the Clusters pane. `nodeops.SetRegion` is unit-proven (patch sets the label; empty/
unknown handled); the handler validation is proven; `render` pure; `TestNoSSHPlane` +
the writer RBAC guard stay green; `govulncheck` 0-reachable. **Fully rig-provable on the
kind rig** — relabel is a real `kubectl label`-equivalent patch (no k3s needed).

## Sequencing (recorded, not part of this slice)

- **S3b (this spec)** — Move Node (region relabel). Completes the mockup's Cluster
  Management actions (Maintenance + Drain from S3a; Move here; Balance dropped).
- **S4** — API Manager (exchange keys → Vault).
- **S5** — Setup Wizard (first-node bootstrap chicken-and-egg).
- **Control-plane bring-up (infra-gated)** — a k3s server + rig for S2a's live join and
  S2b's deferred addons.
