package provision

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PLACEMENT IS A PRECONDITION, AND IT IS THE ONE THAT GOES SILENT (#207).
//
// The provisioning Job is pinned to the control plane — see controlPlaneOnly for the
// credential-confinement argument that puts it there and keeps it there. This estate
// has exactly ONE control-plane node, so "runs only on the control plane" and
// "unavailable while the control plane is drained" are the same sentence, and nothing
// in this repository has ever said so out loud at the moment it mattered.
//
// What an operator saw before this file existed: AddNode returned 200 with a provision
// id, and the Job then sat Pending for the full ActiveDeadlineSeconds (900s) with the
// bootstrap SSH key and K3S_TOKEN mounted on a pod that would never start, because the
// only node its nodeSelector admits was cordoned by the drain they had started five
// minutes earlier. Nothing in the answer, in ListProvisions, or in the logs named the
// drain. That is exactly the failure CLAUDE.md's "nothing configured and checked-and-fine
// must never look the same" rule is about, one level up: accepted-and-doomed looked
// identical to accepted-and-working.
//
// THIS IS LEGIBILITY, NOT A GUARANTEE. The check is a cluster read, and the estate can
// be drained one millisecond after it returns; it cannot promise the Job schedules. It
// converts the deterministic, routine case — a drain is in progress WHEN the request
// arrives — from a silent Pending into a refusal that names the node. A second copy of
// the check inside AddNode would not close that race either, so there is one, at the
// RPC boundary, where the operator can still act on what it says.
//
// It is deliberately NOT built on estate.Reader, which already lists nodes. That read
// model projects roles as label-key SUFFIXES and drops the label VALUE, and the value
// is the whole difficulty here: a nodeSelector is an exact string match, k3s writes
// "true" and kubeadm/kind write "", so a read model that cannot tell those apart cannot
// tell "the pin matches this node" from "the pin will never match anything". This
// matches against controlPlaneOnly itself — the same map jobSpec stamps on the pod — so
// the check and the pin cannot drift.

// PlacementVerdict answers one question: could the provisioning Job be scheduled right
// now, on this estate, as it is? Its zero value is NOT schedulable, deliberately — a
// verdict nobody computed must never read as "fine".
type PlacementVerdict struct {
	// Selector is the pin the verdict was computed against (controlPlaneOnly). Carried
	// so a refusal can quote the exact label a reader has to go and look at.
	Selector map[string]string
	// Eligible are matching nodes that could run the Job now.
	Eligible []string
	// Cordoned are matching nodes marked unschedulable. THIS IS WHAT A DRAIN LOOKS
	// LIKE: nodeops.Drain cordons first and then evicts, so a drain in progress and a
	// drain that finished are both this.
	Cordoned []string
	// NotReady are matching, schedulable nodes whose kubelet is not reporting Ready.
	// A distinct diagnosis from a drain: nobody asked for it, and uncordoning fixes
	// nothing.
	NotReady []string
	// Tainted are matching, schedulable, Ready nodes carrying a taint that the Job's
	// tolerations do not cover. Each entry reads `name (key=value:Effect)`.
	Tainted []string
	// Mismatched are nodes carrying the pin's label KEY with a different VALUE. They
	// are not candidates — an exact-match selector rejects them — and they are called
	// out separately because "the estate is labelled for another distribution" and
	// "there is no control-plane node" are repaired differently.
	Mismatched []string
	// Total is how many nodes were read, so a refusal can distinguish "no node matches
	// the pin" from "this operator can see no nodes at all".
	Total int
}

// Schedulable reports whether at least one node could run the provisioning Job now.
func (v PlacementVerdict) Schedulable() bool { return len(v.Eligible) > 0 }

// Drained reports whether the reason nothing can run the Job is that every node the pin
// admits is cordoned — the drain case, and the one that gets its own refusal code at the
// gRPC boundary so an operator is not sent looking for a provisioning bug.
func (v PlacementVerdict) Drained() bool {
	return !v.Schedulable() && len(v.Cordoned) > 0
}

// CheckPlacement reads the node inventory and decides whether the provisioning Job could
// be scheduled. An error means the question could not be ANSWERED — the caller must
// refuse on it rather than assume yes.
func (p *Provisioner) CheckPlacement(ctx context.Context) (PlacementVerdict, error) {
	list, err := p.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return PlacementVerdict{}, fmt.Errorf("list nodes to check provisioning-job placement: %w", err)
	}
	v := PlacementVerdict{Selector: controlPlaneOnly, Total: len(list.Items)}
	for i := range list.Items {
		n := &list.Items[i]
		switch {
		case !matchesSelector(n.Labels, controlPlaneOnly):
			if carriesAnyKey(n.Labels, controlPlaneOnly) {
				v.Mismatched = append(v.Mismatched, describeMismatch(n.Name, n.Labels, controlPlaneOnly))
			}
		case n.Spec.Unschedulable:
			v.Cordoned = append(v.Cordoned, n.Name)
		case !isReady(n):
			v.NotReady = append(v.NotReady, n.Name)
		default:
			if t, blocked := blockingTaint(n, tolerateControlPlane); blocked {
				v.Tainted = append(v.Tainted, fmt.Sprintf("%s (%s=%s:%s)", n.Name, t.Key, t.Value, t.Effect))
				continue
			}
			v.Eligible = append(v.Eligible, n.Name)
		}
	}
	sort.Strings(v.Eligible)
	sort.Strings(v.Cordoned)
	sort.Strings(v.NotReady)
	sort.Strings(v.Tainted)
	sort.Strings(v.Mismatched)
	return v, nil
}

// Reason states, in an operator's terms, why the Job cannot be scheduled. Empty when it
// can — a caller reading a reason off a schedulable verdict has a bug, not a message.
//
// EVERY BRANCH NAMES THE NODE AND THE REPAIR. The point of the whole file is that the
// operator does not go looking in the wrong place, so "unschedulable" alone would fail
// its own purpose: a drain, an estate labelled for the wrong distribution, and a kubelet
// that fell over are three different mornings.
func (v PlacementVerdict) Reason() string {
	if v.Schedulable() {
		return ""
	}
	pin := selectorText(v.Selector)
	switch {
	case v.Drained():
		return fmt.Sprintf(
			"the control plane is drained: %s cordoned, and the node-provisioning job runs nowhere else. "+
				"It is pinned to %s because it mounts the cluster-admission token (K3S_TOKEN) and an SSH "+
				"bootstrap key, which must not land on a worker — that pin is deliberate and is not relaxed "+
				"to work around a drain. Accepting this would create a job that sits Pending until its "+
				"15-minute deadline expires, holding those credentials the whole time. Uncordon the node "+
				"(POST /v1/control/nodes/{name}/uncordon) or finish the maintenance, then retry.%s",
			nodePhrase(v.Cordoned), pin, v.alsoBlocked(causeCordoned))
	case v.Total == 0:
		return fmt.Sprintf(
			"the operator can see no nodes at all, so it cannot place the node-provisioning job (which "+
				"requires %s). This is not a drain: check the operator's ClusterRole still grants list on "+
				"nodes before looking at the estate", pin)
	case len(v.NotReady) > 0:
		return fmt.Sprintf(
			"%s not Ready, and the node-provisioning job runs nowhere else (it is pinned to %s). This is "+
				"NOT a drain — the node is still schedulable, its kubelet is not reporting Ready, so "+
				"uncordoning changes nothing. Provisioning resumes when the node does.%s",
			nodePhrase(v.NotReady), pin, v.alsoBlocked(causeNotReady))
	case len(v.Tainted) > 0:
		return fmt.Sprintf(
			"every node the node-provisioning job may run on carries a taint it does not tolerate: %s. It "+
				"is pinned to %s and tolerates only %s:NoSchedule. This is not a drain: a taint added to "+
				"the control plane makes provisioning unschedulable until the job's tolerations cover it",
			strings.Join(v.Tainted, ", "), pin, controlPlaneRoleLabel)
	default:
		return fmt.Sprintf(
			"no node carries %s, the pin the node-provisioning job requires, so it could never be "+
				"scheduled (%d nodes seen). This is not a drain: a nodeSelector is an exact string match, "+
				"and the label's value is distribution-specific — k3s writes \"true\", kubeadm and kind "+
				"write an empty value.%s", pin, v.Total, v.alsoBlocked(causeNoCandidate))
	}
}

// placementCause names which disqualification a Reason() branch already led with, so
// alsoBlocked does not repeat it.
type placementCause int

const (
	causeCordoned placementCause = iota
	causeNotReady
	causeNoCandidate
)

// alsoBlocked appends the candidates disqualified for a reason OTHER than the one the
// message led with. Without it a two-node control plane reports the first fault, is
// repaired, and reveals the second — one interruption per node.
func (v PlacementVerdict) alsoBlocked(lead placementCause) string {
	var parts []string
	if lead != causeNotReady && len(v.NotReady) > 0 {
		parts = append(parts, fmt.Sprintf("%s not Ready", nodePhrase(v.NotReady)))
	}
	if len(v.Tainted) > 0 {
		parts = append(parts, "untolerated taints on "+strings.Join(v.Tainted, ", "))
	}
	if len(v.Mismatched) > 0 {
		parts = append(parts, "these nodes carry the label with a value the selector does not match: "+
			strings.Join(v.Mismatched, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return " Also blocking: " + strings.Join(parts, "; ") + "."
}

// matchesSelector is the scheduler's nodeSelector rule: EVERY key must be present with
// exactly that value. Written out rather than delegated to labels.Selector so the
// exact-match semantics the refusal messages talk about are visible here.
func matchesSelector(nodeLabels, selector map[string]string) bool {
	for k, want := range selector {
		if got, ok := nodeLabels[k]; !ok || got != want {
			return false
		}
	}
	return true
}

// carriesAnyKey reports whether the node carries any of the selector's KEYS, whatever
// their value — the signal that this node was meant to match and does not.
func carriesAnyKey(nodeLabels, selector map[string]string) bool {
	for k := range selector {
		if _, ok := nodeLabels[k]; ok {
			return true
		}
	}
	return false
}

// describeMismatch renders `name (key="actual", want "expected")` for each selector key
// the node spells differently.
func describeMismatch(name string, nodeLabels, selector map[string]string) string {
	var parts []string
	for _, k := range sortedKeys(selector) {
		got, ok := nodeLabels[k]
		if !ok || got == selector[k] {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%q, want %q", k, got, selector[k]))
	}
	return fmt.Sprintf("%s (%s)", name, strings.Join(parts, ", "))
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// isReady reports whether the node's Ready condition is True. Absent or Unknown is NOT
// ready — the same deny-by-default rule estate.readyStatus applies, for the same reason:
// a node nobody can hear from must not be counted as one that can run the Job.
func isReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// blockingTaint returns the first taint that would keep the Job's pod off this node.
//
// It considers only NoSchedule and NoExecute — PreferNoSchedule is a preference the
// scheduler may overrule, so treating it as blocking would refuse a request that would
// have succeeded. Taints that merely restate a condition reported above (the cordon's
// own unschedulable taint, and the not-ready/unreachable pair) are skipped: they are
// already explained, and by a message that names the repair.
func blockingTaint(n *corev1.Node, tolerations []corev1.Toleration) (corev1.Taint, bool) {
	for _, t := range n.Spec.Taints {
		if t.Effect != corev1.TaintEffectNoSchedule && t.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if explainedElsewhere(t.Key) {
			continue
		}
		if !toleratesTaint(tolerations, t) {
			return t, true
		}
	}
	return corev1.Taint{}, false
}

// explainedElsewhere lists the node-condition taints whose cause CheckPlacement already
// reports directly and more usefully.
func explainedElsewhere(key string) bool {
	switch key {
	case corev1.TaintNodeUnschedulable, corev1.TaintNodeNotReady, corev1.TaintNodeUnreachable:
		return true
	}
	return false
}

// toleratesTaint applies the Kubernetes toleration-matching rule: an empty Effect
// tolerates every effect, Exists ignores the value, and an empty Key with Exists
// tolerates everything.
func toleratesTaint(tolerations []corev1.Toleration, t corev1.Taint) bool {
	for _, tol := range tolerations {
		if tol.Effect != "" && tol.Effect != t.Effect {
			continue
		}
		if tol.Key == "" {
			if tol.Operator == corev1.TolerationOpExists {
				return true
			}
			continue
		}
		if tol.Key != t.Key {
			continue
		}
		switch tol.Operator {
		case corev1.TolerationOpExists:
			return true
		default: // Equal, and the empty operator which defaults to Equal
			if tol.Value == t.Value {
				return true
			}
		}
	}
	return false
}

// selectorText renders a selector map the way kubectl prints one, so the refusal quotes
// something an operator can paste into `kubectl get nodes -l`.
func selectorText(selector map[string]string) string {
	if len(selector) == 0 {
		return "(no selector)"
	}
	parts := make([]string, 0, len(selector))
	for _, k := range sortedKeys(selector) {
		parts = append(parts, k+"="+selector[k])
	}
	return strings.Join(parts, ",")
}

// nodePhrase renders a node list with the verb that agrees with it, so the messages read
// as sentences on a one-node estate (the only estate that exists today) and stay correct
// on a larger one.
func nodePhrase(names []string) string {
	switch len(names) {
	case 0:
		return "no node is"
	case 1:
		return fmt.Sprintf("node %q is", names[0])
	default:
		return fmt.Sprintf("nodes %s are", strings.Join(quoteAll(names), ", "))
	}
}

func quoteAll(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, fmt.Sprintf("%q", n))
	}
	return out
}
