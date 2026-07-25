package tui

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// kubectl runs one read-only query against the cluster and returns its STDOUT only.
//
// Every assertion in this package goes through here rather than through the TUI's
// rendering: a UI that shows its optimistic intent instead of observed state must FAIL
// these proofs, and it can only do that if the oracle is independent of it.
//
// Stdout and stderr are kept apart deliberately. CombinedOutput() merged them, and every
// oracle assertion in this file parses the result — so anything kubectl wrote to stderr
// became part of the value under test. That is not hypothetical: on a k3s host
// /usr/local/bin/kubectl is the k3s binary, which logs
//
//	level=info msg="Acquiring lock file /var/lib/rancher/k3s/data/.lock"
//	level=info msg="Preparing data dir ..."
//
// to stderr on EVERY invocation. Under CombinedOutput, clusterNodeNames' strings.Fields
// split those two lines into a dozen tokens and reported a one-node cluster as many,
// silently skipping the live-join proof. The same contamination reaches nodeLabel,
// secretDataKeys and the rest, where it can just as easily produce a false PASS — an
// assertion satisfied by a substring of a log line rather than by cluster state.
//
// stderr is still captured, and still reported when the command fails, because that is
// where kubectl explains itself. It simply never reaches a caller as data.
func kubectl(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("kubectl", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kubectl %s: %v\nstderr: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// kubectlLines splits an oracle query into non-empty lines.
func kubectlLines(t *testing.T, args ...string) []string {
	t.Helper()
	out := kubectl(t, args...)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// nodeSchedulable reports whether the node accepts new pods. Absent means schedulable
// — .spec.unschedulable is only set when cordoned.
func nodeSchedulable(t *testing.T, node string) bool {
	t.Helper()
	return kubectl(t, "get", "node", node, "-o", "jsonpath={.spec.unschedulable}") != "true"
}

// waitForSchedulable polls the CLUSTER until the node reaches the wanted state.
func waitForSchedulable(t *testing.T, node string, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if nodeSchedulable(t, node) == want {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("node %s did not become schedulable=%v within %s — the keypress did not reach "+
		"Kubernetes, whatever the TUI rendered", node, want, timeout)
}

// waitForRegion polls the CLUSTER until node's topology.kubernetes.io/region label
// equals want, the same shape as waitForSchedulable above: poll the cluster until X,
// else t.Fatalf. A test that instead `return`ed from inside its own poll loop on
// success would skip any deferred cleanup registered after the loop was entered.
func waitForRegion(t *testing.T, node, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if nodeLabel(t, node, "topology.kubernetes.io/region") == want {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("topology.kubernetes.io/region on %s never became %q within %s — the keypress "+
		"did not reach Kubernetes, whatever the TUI rendered", node, want, timeout)
}

func nodeLabel(t *testing.T, node, key string) string {
	t.Helper()
	return kubectl(t, "get", "node", node, "-o", "jsonpath={.metadata.labels."+
		strings.ReplaceAll(key, ".", "\\.")+"}")
}

// podsOnNode lists non-terminated pods bound to a node, DaemonSets included — the
// drain proof needs to distinguish what may be evicted from what may not.
//
// The selector excludes Succeeded/Failed rather than requiring phase=Running: a pod
// stuck Pending or ContainerCreating is non-terminated and is exactly the kind the
// drain proof must still see, since it still needs evicting. Filtering to Running
// alone would silently drop those and let a drain assertion pass wrongly.
func podsOnNode(t *testing.T, node string) []string {
	t.Helper()
	out := kubectl(t, "get", "pods", "-A", "--field-selector",
		"spec.nodeName="+node+",status.phase!=Succeeded,status.phase!=Failed",
		"-o", "jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name} {end}")
	if out == "" {
		return nil
	}
	return strings.Fields(out)
}

// secretDataKeys lists the key names in a Secret's data map.
//
// This uses go-template, not jsonpath: kubectl's jsonpath has no two-variable range
// form, so "{range $k, $v := .data}" is Go template syntax that jsonpath's parser
// rejects outright ("unrecognized character in action: U+002C ','"). go-template
// supports it. Do not "simplify" this back to jsonpath — it does not parse.
func secretDataKeys(t *testing.T, ns, name string) []string {
	t.Helper()
	out := kubectl(t, "-n", ns, "get", "secret", name,
		"-o", "go-template={{range $k, $v := .data}}{{$k}} {{end}}")
	return strings.Fields(out)
}

// waitForSecret waits for a Secret to exist and returns its data keys. Separate from
// secretDataKeys because a write driven through a UI is asynchronous: the form returns
// before the RPC completes, and failing on the first miss would be a race, not a proof.
//
// It shells out directly rather than through kubectl() for the existence probe alone,
// for the same reason nodeExists does — "not there yet" is the expected state on every
// pass but the last, and kubectl() is fatal on a non-zero exit. Once the object exists,
// the read of its keys goes back through kubectl(), where a failure IS fatal.
func waitForSecret(t *testing.T, ns, name string, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := exec.Command("kubectl", "-n", ns, "get", "secret", name,
			"-o", "name").Run(); err == nil {
			return secretDataKeys(t, ns, name)
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("secret %s/%s never appeared within %s — the form did not produce it", ns, name, timeout)
	return nil
}

// secretValueEncodedLen returns the LENGTH of a Secret value's base64 encoding — never
// the value, which is never printed, logged or returned by anything in this package.
//
// It exists because the presence of a data key proves almost nothing about the write
// that produced it. services/operator/internal/secrets/kube.go writes api-key and
// api-secret UNCONDITIONALLY, empty string included, so a form that submitted nothing at
// all still yields a Secret carrying both names. A leak check then searches for a canary
// that never entered the credential path in the first place, and reports clean because
// there was nothing to find. Comparing this length against the canary's own encoded
// length pins that the exact bytes typed into the masked field are the bytes that landed.
//
// go-template, not jsonpath, and index rather than a field selector: the data keys are
// HYPHENATED (api-secret), which neither expression language can address with a dot.
func secretValueEncodedLen(t *testing.T, ns, name, key string) int {
	t.Helper()
	out := kubectl(t, "-n", ns, "get", "secret", name,
		"-o", `go-template={{len (index .data "`+key+`")}}`)
	n, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("length of %s/%s data key %q: kubectl returned %q, not a number: %v",
			ns, name, key, out, err)
	}
	return n
}

// deploymentLogs returns the recent logs of EVERY pod behind a deployment, concatenated.
//
// `kubectl logs deploy/x` reads ONE pod. api-gateway runs two replicas
// (infra/deploy/api-gateway-deploy.yaml), and the operator's write goes through whichever
// one the Service happened to pick — so a leak check written against the deployment
// searches half the surface and reports the other half clean without having looked at it.
// Enumerating the pods and reading each by name closes that, and the empty-list Fatal
// stops the check from passing because it found nothing to read.
//
// Both deployments on this path label their pods app=<deployment name>, which is what
// makes one selector enough here.
func deploymentLogs(t *testing.T, ns, deployment, since string) string {
	t.Helper()
	pods := kubectlLines(t, "-n", ns, "get", "pods", "-l", "app="+deployment, "-o", "name")
	if len(pods) == 0 {
		t.Fatalf("no pods matched app=%s in %s, so a leak check against its logs would pass "+
			"having read nothing at all", deployment, ns)
	}
	var b strings.Builder
	for _, pod := range pods {
		b.WriteString(kubectl(t, "-n", ns, "logs", pod, "--all-containers", "--since="+since))
		b.WriteByte('\n')
	}
	return b.String()
}

// nodeExists reports whether the node is present. It shells out directly rather than
// through kubectl() because a missing node is an expected outcome here, not a fatal
// one — but a bool alone can't tell "no such node" apart from an RBAC denial or an
// unreachable API server, so log kubectl's own words on failure instead of discarding
// them, the same way every other helper in this file threads output into its failure.
func nodeExists(t *testing.T, node string) bool {
	t.Helper()
	out, err := exec.Command("kubectl", "get", "node", node, "-o", "name").CombinedOutput()
	if err != nil {
		t.Logf("nodeExists(%s): %v\n%s", node, err, out)
		return false
	}
	return true
}

// evictablePodsOnNode lists the pods on a node that a drain is REQUIRED to remove, as
// "namespace/name" — the same form podsOnNode returns, so the two sets can be compared
// directly.
//
// The rule mirrors services/operator/internal/estate.IsEvictable, which is what the
// drain loop itself consults: a pod is exempt if it is already terminating, if it is a
// mirror pod (kubelet gives those an ownerReference of kind Node), or if a DaemonSet
// owns it. Drain is eviction-only by design and never force-deletes, so those three are
// exactly what must SURVIVE a drain. Encoding the same rule here rather than asserting
// "the node has no pods" is what keeps the proof from demanding behaviour the drain is
// deliberately not allowed to have.
//
// The phase filter matches podsOnNode's and deliberately does NOT narrow to
// status.phase=Running. A pod that is Pending or ContainerCreating but already BOUND to
// this node (spec.nodeName set) still needs evicting, and a Running-only filter would
// make it invisible — the drain proof would then report a node as drained while an
// evictable pod sat on it, which is a false PASS in the one direction that matters.
func evictablePodsOnNode(t *testing.T, node string) []string {
	t.Helper()
	out := kubectl(t, "get", "pods", "-A", "--field-selector",
		"spec.nodeName="+node+",status.phase!=Succeeded,status.phase!=Failed",
		"-o", "jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}"+
			":{.metadata.ownerReferences[0].kind}:{.metadata.deletionTimestamp} {end}")
	var evictable []string
	for _, rec := range strings.Fields(out) {
		name, rest, ok := strings.Cut(rec, ":")
		if !ok {
			continue
		}
		kind, deleting, _ := strings.Cut(rest, ":")
		if deleting != "" || kind == "DaemonSet" || kind == "Node" {
			continue
		}
		evictable = append(evictable, name)
	}
	return evictable
}

// podSettleWindow is how long a node's evictable pod set must hold still before it is
// trusted as a baseline, and podSettleTimeout bounds the wait for that to happen.
const (
	podSettleWindow  = 6 * time.Second
	podSettleTimeout = 2 * time.Minute
)

// waitForStableEvictablePods returns a node's evictable pod set once it has been
// unchanged for podSettleWindow.
//
// This is a precondition, not a retry. A drain's eviction loop runs in the BACKGROUND
// for up to 15 minutes (drainDeadline, services/operator/internal/nodeops), so a re-run
// started while a previous drain is still evicting would take a baseline that is
// already dissolving underneath it — and the abort branch, whose whole claim is that
// this set is untouched, would then fail for something no keypress did. Refusing to
// start until the node is quiet turns that into one sentence instead of a mystery, and
// it is the reason this proof is safely re-runnable.
func waitForStableEvictablePods(t *testing.T, node string) []string {
	t.Helper()
	deadline := time.Now().Add(podSettleTimeout)
	prev := evictablePodsOnNode(t, node)
	changedAt := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		cur := evictablePodsOnNode(t, node)
		if len(missingFrom(prev, cur)) > 0 || len(missingFrom(cur, prev)) > 0 {
			prev, changedAt = cur, time.Now()
			continue
		}
		if time.Since(changedAt) >= podSettleWindow {
			return cur
		}
	}
	t.Fatalf("the evictable pod set on %s never held still for %s (last seen: %v). A drain "+
		"evicts in the background for up to 15 minutes, so a previous run's drain is the "+
		"likely cause — wait for it to finish rather than re-running into it.",
		node, podSettleWindow, prev)
	return nil
}

// waitForPodsEvicted waits until none of want remain on the node.
//
// It asserts that the pods that were there are GONE, rather than that the node ends up
// with no evictable pods at all. Those are different claims, and only the first is
// about the drain: a controller whose replica is pinned to this node will create a
// replacement the moment its pod is evicted, and whether that replacement can land here
// depends on the pod's own tolerations, not on the drain. A proof written as "no
// evictable pods remain" fails on a workload that tolerates the unschedulable taint
// even though the drain did precisely its job.
func waitForPodsEvicted(t *testing.T, node string, want []string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := stillPresent(want, podsOnNode(t, node))
		if len(remaining) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still on %s after %s: %v. If a PodDisruptionBudget is blocking, that is "+
				"CORRECT behaviour — check `kubectl get pdb -A` and whether the blocked pod "+
				"should have been on this node at all.", node, timeout, remaining)
		}
		time.Sleep(3 * time.Second)
	}
}

// missingFrom returns the members of want that are absent from have, and stillPresent
// returns those that are not. Both report the offending NAMES rather than a count: a
// drain assertion that fails with "3 != 4" sends the next reader back to the cluster to
// work out which pod it meant, and by then the pod is gone.
func missingFrom(want, have []string) []string { return filterByMembership(want, have, false) }

func stillPresent(want, have []string) []string { return filterByMembership(want, have, true) }

func filterByMembership(want, have []string, keepPresent bool) []string {
	present := make(map[string]struct{}, len(have))
	for _, h := range have {
		present[h] = struct{}{}
	}
	var out []string
	for _, w := range want {
		if _, ok := present[w]; ok == keepPresent {
			out = append(out, w)
		}
	}
	return out
}

// controlPlaneNodeName returns THE control-plane node, failing unless there is exactly
// one.
//
// The count guard is not defensive dressing. The drain proof uses this name to assert
// the rest of the estate was untouched, and jsonpath answers a filter matching nothing
// with an EMPTY string and one matching several with a space-separated list. Either
// would reach podsOnNode as a nodeName no pod carries, so the before and after counts
// would both be zero and the assertion would hold no matter what the drain did.
func controlPlaneNodeName(t *testing.T) string {
	t.Helper()
	out := kubectl(t, "get", "nodes", "-o",
		"jsonpath={range .items[?(@.metadata.labels.node-role\\.kubernetes\\.io/control-plane)]}"+
			"{.metadata.name} {end}")
	names := strings.Fields(out)
	if len(names) != 1 {
		t.Fatalf("expected exactly one node labelled node-role.kubernetes.io/control-plane, "+
			"got %v — this proof needs an unambiguous name for the node a drain must NOT touch",
			names)
	}
	return names[0]
}

// waitForNodeReady blocks until a second node reports Ready, polling the CLUSTER.
// The TUI's own strip is not evidence that a node joined.
//
// "NOT THE CONTROL PLANE" IS DECIDED BY NAME, NEVER BY ADDRESS. This used to exclude
// the control plane with `!strings.Contains(name, "172-26-11-140")` — the current k3s
// server's IP, baked in. A node's identity here is its NAME, and an IP is neither
// stable nor unique to it: replace, rebuild or renumber the server and that substring
// matches nothing, so the very first Ready node — the control plane — is returned as
// "the node that joined". Every caller then acts on the wrong node while still passing:
// cordon/uncordon would cordon the control plane, move-region would relabel it, and
// worst, the join proof's count guard would see one node and be handed that same node
// back as the second one, so A JOIN THAT NEVER HAPPENED REPORTS PASS. controlPlaneNodeName
// answers the question correctly and portably (it reads the control-plane label, and
// fails unless exactly one node carries it), and TestDrainConfirmAbortsAndProceeds
// already cross-checks against it — the other proofs are now consistent with that.
func waitForNodeReady(t *testing.T, timeout time.Duration) string {
	t.Helper()
	// Resolved once, outside the loop: the control plane does not change identity while
	// we wait for someone else to join, and its own guard should fail immediately rather
	// than once per poll.
	controlPlane := controlPlaneNodeName(t)

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out := kubectl(t, "get", "nodes",
			"-o", "jsonpath={range .items[*]}{.metadata.name}={.status.conditions[?(@.type==\"Ready\")].status} {end}")
		for _, pair := range strings.Fields(out) {
			name, status, _ := strings.Cut(pair, "=")
			if status == "True" && name != controlPlane {
				return name
			}
		}
		time.Sleep(5 * time.Second) // polling the CLUSTER, not a workflow's feedback
	}
	t.Fatalf("no second node reached Ready within %s. Check `journalctl -u k3s-agent` on "+
		"node 2 — on a 414MB host an OOM-killed kubelet presents as a node that never "+
		"appears, which reads as a join failure and is not one.", timeout)
	return ""
}
