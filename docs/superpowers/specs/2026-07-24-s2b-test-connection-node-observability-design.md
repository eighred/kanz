# Test Connection + cluster-native node observability (S2b)

**Date:** 2026-07-24
**Status:** design approved, pending implementation plan
**Direction:** third slice of the TUI-based sovereign fund-management surface, after F0+S1 (read-only spine) and S2a (node provisioning). Lead-committed 2026-07-24.

## Context

S2a shipped Add Node: the operator provisions a host into the estate via a one-shot
k3s-join Job. The original mockup's "Save" chained three more per-host steps —
WireGuard, node-exporter, auto-updates — plus a "Test Connection" pre-flight. This
slice (S2b) delivers the parts of that chain that are genuinely valuable **under the
k3s/k8s substrate we chose**, and defers the parts that are control-plane concerns.

Two forks were resolved before this design:

1. **Cluster-native, not a per-node SSH chain.** Under k3s/k8s, "install node-exporter
   / WireGuard / auto-updates on each host over SSH" redoes per-host what Kubernetes
   already does cluster-wide, adds host-level state k8s does not manage, and would
   widen the `crypto/ssh` surface S2a worked to bound. So these become cluster
   properties a joined node inherits — not steps in the provisioner Job. Rejected: the
   mockup-literal per-node SSH chain.

2. **Test Connection is a TCP reachability probe done in the operator, not an SSH auth
   probe.** The operator `net.Dial`s the target's SSH port and reports reachable +
   latency — synchronous, fast, and needing **no `crypto/ssh`** (so the operator stays
   free of it and the path-scoped ssh-plane guard is untouched). It verifies
   reachability, not the key; the full SSH+key check already happens at real provision
   time, where a bad key surfaces as a Failed provision. Rejected: a full SSH-auth probe
   via a short Job (thorough but a Job round-trip per pre-flight, and heavier).

## Scope

**In:**

1. **Test Connection** — a new `operator.v1.TestConnection` RPC; the operator service
   answers it with a bounded `net.DialTimeout` to `ip:ssh_port`; the `universe` Add Node
   form gets a `[t] test` key that shows the result inline before Save.
2. **node-exporter DaemonSet** — `infra/observability/node-exporter.yaml`: a standard
   Prometheus node-exporter DaemonSet with tolerations for every node, so a joined node
   is scraped automatically with no per-node SSH.

**Out, recorded rather than assumed away:**

- **auto-updates (system-upgrade-controller) and WireGuard (`flannel-backend=wireguard-native`)**
  — both are k3s **control-plane** concerns (the upgrade controller drives the server's
  k3s version; the flannel backend is a server flag agents inherit), not per-node steps,
  and neither means anything without a k3s control plane, which does not exist here.
  Deferred to a **control-plane bring-up** slice, alongside S2a's live-join proof, when a
  k3s rig lands. Shipping them now would be unprovable YAML against absent infra.
- **A full SSH-auth pre-flight.** Test Connection is reachability only (see fork 2).
- **Deploying Prometheus itself.** The DaemonSet exposes metrics; wiring a Prometheus to
  scrape it is infra (none exists in-repo). The DaemonSet is correct-by-construction; its
  *usefulness* is infra-gated, like S2a's live join.

## Design

### 1. Test Connection

- **`operator.v1`:** `rpc TestConnection(TestConnectionRequest) returns (TestConnectionResponse)`.
  Request: `ip`, `ssh_port` (0 ⇒ 22). Response: `reachable` (bool), `latency_ms` (int64),
  `message` (string — the dial error when unreachable). Additive to the proto.
- **Operator handler** (`grpcsrv`): `net.DialTimeout("tcp", net.JoinHostPort(ip, port), 5s)`,
  measuring elapsed time; close the conn immediately (a reachability probe, not a session).
  On success: `reachable=true`, `latency_ms`, empty message. On dial error: `reachable=false`,
  `latency_ms=0`, `message` = a trimmed dial error. It returns `codes.InvalidArgument` for an
  empty `ip`. **No `crypto/ssh`** — the operator must not import it (the ssh-plane guard would
  fail); this is a plain TCP dial.
  - *SSRF note, accepted:* the caller can make the operator dial an arbitrary `ip:port`. The
    caller is `root@universe` reaching the operator only through a kubeconfig-gated
    port-forward (already able to reach anything a cluster operator can), the probe returns
    only reachable/latency (never response content), and the timeout is bounded — so the
    surface is a trusted operator with no data-exfiltration channel. Recorded, not mitigated
    further.
- **TUI** (`cmd/universe`): a `[t] test` key on the Add Node form fires `TestConnection`
  off the UI thread (a `tea.Cmd`, like submit) with the form's `ip`/`ssh_port`, and renders
  the result inline on the form: `✓ reachable (12ms)` or `✗ unreachable: <message>`. The
  private key is not involved. `render` stays pure (the result is a string field set by the
  message handler, computed off-thread).

### 2. node-exporter DaemonSet

- `infra/observability/node-exporter.yaml`: a DaemonSet running
  `quay.io/prometheus/node-exporter` on every node (`tolerations` with `operator: Exists` so
  it also lands on tainted/control-plane nodes), `hostNetwork`/`hostPID`, the standard
  read-only host mounts (`/proc`, `/sys`, root filesystem) with node-exporter's
  `--path.{procfs,sysfs,rootfs}` args, a hardened container securityContext, and the
  Prometheus discovery hints (`prometheus.io/scrape: "true"`, port `9100`) so a Prometheus
  with pod-annotation discovery finds it. Placed in the repo's observability/monitoring
  namespace convention (the plan confirms the exact namespace against existing infra).
- Applied once; every node that S2a provisions gets a node-exporter pod automatically —
  observability is a cluster property, not a provisioning step.

## Testing

- **TestConnection handler:** unit tests — dial a live `net.Listener` on a random port
  (reachable → `true` + a non-negative latency) and a closed/unused port (unreachable →
  `false` + a non-empty message); empty `ip` → `InvalidArgument`. No cluster, no crypto/ssh.
- **The ssh-plane guard stays green:** `TestNoSSHPlane` must still pass — the operator gains
  a TCP dial, NOT `crypto/ssh`. (A regression here would mean someone reached for an SSH
  library; the guard catches it.)
- **TUI:** form tests — the `[t]` key produces a `testConnResultMsg`, and `render` shows the
  reachable/unreachable line; `render` remains pure.
- **node-exporter manifest:** valid multi-doc YAML (parseable), the DaemonSet present with
  all-node tolerations. The live "a pod runs per node and is scraped" proof is infra-gated
  (needs a real cluster + Prometheus).

## Definition of done

From `universe`, an operator opens Add Node, enters a host, presses `[t]`, and sees a
reachable/unreachable result with latency before deciding to Save; the operator handler is
unit-proven for both outcomes; `TestNoSSHPlane` is still green (no `crypto/ssh` added to the
operator); and the node-exporter DaemonSet is a valid manifest under `infra/observability/`.
The live node-exporter scrape and any control-plane addon are infra-gated (a real cluster /
a k3s rig), and recorded as such.

## Sequencing (recorded, not part of this slice)

- **S2b (this spec)** — Test Connection + node-exporter DaemonSet.
- **Control-plane bring-up (infra-gated)** — a k3s control plane + rig; then
  `system-upgrade-controller` + a k3s upgrade Plan (auto-updates), `flannel-backend=wireguard-native`
  (the encrypted mesh), Prometheus scraping node-exporter, and S2a's live-join proof.
- **S3** — cluster management: Move / Balance / Drain / Maintenance (k8s cordon/drain/labels).
- **S4** — API Manager (exchange keys → Vault).
- **S5** — Setup Wizard (owns the first-node bootstrap chicken-and-egg).
