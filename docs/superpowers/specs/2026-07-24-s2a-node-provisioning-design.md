# Node provisioning — Add Node via an ephemeral k3s-join Job (S2a)

**Date:** 2026-07-24
**Status:** design approved, pending implementation plan
**Direction:** the write path of the TUI-based sovereign fund-management surface. Second slice (S2a) after F0+S1 (operator spine + read-only Universe TUI). Lead-committed 2026-07-24.

## Context

F0+S1 shipped the read-only foundation: an in-cluster `operator` service listing the
Kubernetes node estate over `operator.v1`, and a credential-free `universe` TUI that
renders it. S2a adds the platform's **first write path** — "Add Node": an operator
provisions a new host into the estate from the TUI, and the node then appears in the
same Nodes pane F0 already renders (the write path feeds the read path).

Two forks were resolved before this design, both toward the architecture the estate
already has:

1. **Provisioning runs in an ephemeral Kubernetes Job, not a long-lived service.** A
   new operator write-RPC creates a **one-shot Job** that SSHes to the target, installs
   the k3s agent, joins, reports status, and exits; the operator service then deletes
   the Job and its Secret. Rejected: the long-lived operator service holding an SSH
   client and provisioning keys continuously (a permanent in-cluster SSH egress and the
   largest standing attack surface), and a cloud-init "pull" model (no SSH at all, but
   not the push UX the mockup drew). The Job model delivers the drawn UX while keeping
   `crypto/ssh` out of every long-lived process — it mirrors the SEC-M3c operator-Job
   pattern (`kanz-halt`), where an operator acts through a short-lived in-cluster Job.

2. **First slice is the join only.** ssh-once → install the k3s agent → join → the node
   appears in `universe`. WireGuard mesh, node-exporter, and auto-updates are S2b;
   Delete/Edit/Drain are S3.

This slice **reactivates `crypto/ssh`**, which F0+S1 and the whole estate kept out. The
`test/arch/ssh_plane_test.go` guard forbids any import whose path contains "ssh"
(deny-by-default, written-reason allowlist), and its own failure message states:
*"Reviving the SSH plane is a lead decision and amends `KANZ_BRAIN.md` — it is not a
test edit."* That lead decision exists (the hybrid k3s substrate), and `KANZ_BRAIN.md`
is amended (the "estate is Kubernetes-managed" entry now records SSH returning at the
node-join handshake). S2a is the bounded reactivation that decision anticipated.

## Scope

**In — the join, and only the join:**

1. A new `operator.v1` **write RPC** `AddNode` (async — creates the Job, returns a
   `provision_id`) plus a `ListProvisions` read RPC surfacing provisioning status.
2. A new **`kanz-provisioner` Job binary** — the ONLY place `crypto/ssh` lives. It SSHes
   once to the target with the supplied bootstrap key, installs the k3s agent against the
   estate's control-plane URL + node-token, reports status, and exits.
3. **Operator service** additions: the `AddNode` handler that creates the transient
   Secret + the Job and tracks status, and the `ListProvisions` handler.
4. **RBAC growth, bounded:** a namespaced Role in `kanz-operator` for `create`/`delete`
   on `jobs` + `secrets` and `get` on `jobs`/`pods`; a dedicated provisioner
   ServiceAccount with **no cluster access**; a NetworkPolicy scoping the Job's egress.
5. **The SSH-plane guard becomes path-scoped:** `crypto/ssh` allowed **only** under the
   provisioner package, offending everywhere else.
6. **`x/crypto` pinned** past the 7 Critical advisories; `govulncheck` must read
   **0 reachable** with `crypto/ssh` now genuinely called.
7. **TUI:** an "Add Node" form (hostname, IP, SSH port, user, private key) + Save
   (fires `AddNode`) + a provisioning-status strip.

**Out, recorded rather than assumed away:**

- **WireGuard mesh, node-exporter, auto-updates** — S2b. The Job installs k3s and joins;
  nothing else.
- **`ssh-copy-id` / persistent SSH access.** Deliberately NOT done. SSH is used **once**,
  to bootstrap the join; post-join the node is managed by Kubernetes, never SSH again —
  that is precisely what "SSH bounded to the node-join handshake" means. The supplied key
  is a one-time bootstrap credential, never persisted, no persistent key installed.
- **Delete / Edit / Drain / Maintenance** node operations — S3.
- **"Test Connection"** (a pre-flight SSH reachability probe) — S2b polish.
- **Full rig-proof of the k3s join.** The dev rig is `kind`, not k3s, so the real join
  cannot be exercised here; the join is built behind a mockable seam and its live proof
  is infra-gated (a k3s rig), exactly as F0's cluster-dependent bits were.
- **Standing up the k3s control plane itself.** S2a assumes the estate cluster exposes a
  join token + server URL (injected into the operator at deploy time); creating that
  control plane is infra, not this slice.

## Design

### 1. Flow

```
universe (Add Node form)  ──AddNode RPC──▶  operator service (kanz-operator ns)
                                              1. validate
                                              2. create short-lived Secret (bootstrap key)
                                              3. read k3s node-token + server URL (config)
                                              4. create one-shot Job (owns the Secret)
                                                     │
                                                     ▼
                                              kanz-provisioner Job  ── crypto/ssh (bounded)
                                              SSH once → install k3s agent → join → status → exit
                                                     │
                              operator svc deletes Job + Secret on completion
                                                     ▼
                              new node runs `k3s agent` → registers → F0 Nodes pane lists it
```

### 2. `operator.v1` additions

- `rpc AddNode(AddNodeRequest) returns (AddNodeResponse)` — **async**. Request carries
  `hostname`, `ip`, `ssh_port` (default 22), `ssh_user`, and `ssh_private_key` (PEM). The
  handler validates, creates the Secret + Job, and returns a `provision_id` (the Job name)
  and initial status. It does **not** block on provisioning.
- `rpc ListProvisions(ListProvisionsRequest) returns (ListProvisionsResponse)` — returns
  the in-flight/recent provisions with a `ProvisionStatus` enum:
  `PENDING` / `SSH_OK` / `INSTALLING` / `JOINED` / `FAILED`, plus a short message.
- `ssh_private_key` is **write-only in spirit**: it is never returned by any RPC, never
  logged, and lives only in the transient Secret.

Additive to `operator/v1` (new RPCs + messages; no existing type changed). Generated
through the existing `buf` pipeline.

### 3. The `kanz-provisioner` Job binary

- New package `services/operator/cmd/provisioner` (or `cmd/kanz-provisioner`) — **the sole
  importer of `golang.org/x/crypto/ssh` in the module.**
- Reads its inputs from the mounted Secret + env: target `ip:port`, `ssh_user`, the
  bootstrap private key, the k3s `server_url` + `node_token`.
- **A `Joiner` seam** decouples the remote work from the transport so it is testable:
  - `type Joiner interface { Join(ctx, target Target, k3s K3sJoin) (Result, error) }`
  - the real impl opens an SSH session (`crypto/ssh`), runs the k3s-agent install command
    (`K3S_URL=<server> K3S_TOKEN=<token>` install), and returns success once the agent
    reports up; a fake impl drives the status transitions for unit tests with no network.
- **Status reporting:** the Job writes its status transitions back to a status surface the
  operator service reads for `ListProvisions` — a Secret/ConfigMap it owns, or an
  annotation on its own Pod/Job. (The plan picks the concrete mechanism; a k8s object the
  operator already watches is preferred over a new store.)
- Exit 0 on join success, non-zero with a reason on failure. The SSH connection is closed
  and the key zeroed as soon as the install command is dispatched — the key never outlives
  the one connection.

### 4. RBAC + isolation (bounded growth)

- The read-only **`operator-node-reader` ClusterRole is untouched** (still `get`/`list`
  nodes).
- **New namespaced Role** `operator-provisioner` in `kanz-operator`: `create`/`delete` on
  `jobs` and `secrets`, `get`/`list` on `jobs` and `pods` (status). Namespaced, never
  cluster-scoped; no verb on `nodes` (a joining node self-registers — the operator never
  writes node objects).
- **Provisioner ServiceAccount** `kanz-node-provisioner`: bound to **nothing** in-cluster
  — it needs no Kubernetes API access, only outbound SSH. A NetworkPolicy permits egress
  to the target host's SSH port + the k3s server + DNS, and denies the rest. Ephemeral:
  the Job and SA-scoped access exist only for the provisioning window.
- The **transient Secret is owned by the Job** via `ownerReferences`, so Kubernetes
  garbage-collects it with the Job even if the operator service crashes mid-flight — the
  bootstrap key can never be orphaned.

### 5. The SSH-plane guard change (path-scoped)

`test/arch/ssh_plane_test.go` currently offends on ANY import path containing "ssh"
(allowlist empty). S2a changes it to a **path-scoped** allow: `golang.org/x/crypto/ssh` is
permitted **only** when the importing file is under the provisioner package
(`services/operator/cmd/provisioner/...` or wherever it lands); the same import anywhere
else still offends. The written reason records: the hybrid-k3s lead decision, the BRAIN
entry, and that SSH is bounded to node-join. The guard keeps its non-vacuity and anti-rot
checks (a dead scope or a moved provisioner must fail, not silently pass).

### 6. `x/crypto` + govulncheck

`crypto/ssh` becomes genuinely reachable, so the 7 Critical `x/crypto/ssh` Dependabot
alerts are now live. `golang.org/x/crypto` is pinned to a version at/after the patched
release, and `govulncheck ./...` must report **0 reachable** — the honest gate replacing
"unreachable so ignored." This is a required verification step in the plan.

### 7. TUI: Add Node

- A new `[A]dd` key on the Nodes pane opens an **Add Node form** (Bubble Tea text inputs:
  hostname, IP, SSH port, user, private key). This is `universe`'s first write interaction
  — the model gains a form mode and input handling, kept isolated from the read panes.
- **Save** fires `AddNode` over the same kubeconfig-gated port-forward; the private key is
  sent once and never echoed/stored client-side.
- A **provisioning-status strip** renders `ListProvisions` output (pending/installing/
  joined/failed) so a failure is visible without leaving the TUI; the happy-path
  confirmation is the node appearing in the Nodes pane.

## Testing

- **Provisioner:** unit tests over the fake `Joiner` — status transitions, a failed SSH
  dial → `FAILED` with a reason, key-never-logged (assert the key bytes never reach the
  status/log surface). The real SSH+k3s impl is exercised against a local `sshd` fixture
  where feasible; the k3s install itself is the infra-gated seam.
- **Operator `AddNode`/`ListProvisions`:** against a fake clientset — Secret + Job created
  with correct `ownerReferences`, status folded from the Job/status surface, the key
  written only into the Secret (never into the Job spec/env in plaintext, never returned).
- **Arch guards:** the path-scoped `ssh_plane_test` (mutation-proven: `crypto/ssh` in a
  non-provisioner file offends; in the provisioner it passes; a moved provisioner fails
  non-vacuously); the extended `operator_rbac_test` (the new Role is namespaced, verbs
  bounded to jobs/secrets, no node writes, no cluster scope).
- **`govulncheck ./...` = 0 reachable** after the `x/crypto` pin.
- **TUI:** form input + Save wiring against a stub client; the status strip renders each
  `ProvisionStatus`. Render stays pure.

## Definition of done

From `universe`, an operator opens Add Node, enters a reachable host's IP + a bootstrap
SSH key, and Save starts provisioning; `ListProvisions` shows the status advancing; on a
k3s-capable target the node joins and appears in the Nodes pane. `crypto/ssh` is imported
**only** by the provisioner (guard-enforced), `govulncheck` is 0-reachable, the transient
Secret is Job-owned and deleted on completion, and unit/arch tests + `go build`/`vet`/
`gofmt` are clean. The live k3s join is proven when a k3s rig exists; until then the join
seam is unit-proven and the SSH transport is fixture-proven.

## Sequencing (recorded, not part of this slice)

- **S2a (this spec)** — Add Node → ephemeral Job → k3s join → node visible.
- **S2b** — WireGuard mesh, node-exporter, auto-updates (extend the provisioner chain);
  "Test Connection" pre-flight.
- **S3** — cluster management: Delete/Edit/Drain/Maintenance (k8s cordon/drain/labels).
- **S4** — API Manager (exchange keys → Vault).
- **S5** — Setup Wizard (owns the first-node bootstrap chicken-and-egg).
- **Infra gate:** a k3s control plane + a k3s rig, to live-prove the join. Until then the
  join is the mockable seam and rig-proof stops at "the Job runs and reports status."
- **Follow-up from F0+S1, still open:** the NetworkPolicy on the operator pod's
  unauthenticated `0.0.0.0:9090` gRPC listener — fold it in here, since S2a is already
  adding NetworkPolicies to `kanz-operator`.
