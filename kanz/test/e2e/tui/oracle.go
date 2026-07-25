package tui

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// kubectl runs a read-only query against the cluster. Every assertion in this package
// goes through here rather than through the TUI's rendering: a UI that shows its
// optimistic intent instead of observed state must FAIL these proofs, and it can only
// do that if the oracle is independent of it.
// kubectl runs one kubectl command and returns its STDOUT only.
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

// waitForNodeReady blocks until a second node reports Ready, polling the CLUSTER.
// The TUI's own strip is not evidence that a node joined.
func waitForNodeReady(t *testing.T, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out := kubectl(t, "get", "nodes",
			"-o", "jsonpath={range .items[*]}{.metadata.name}={.status.conditions[?(@.type==\"Ready\")].status} {end}")
		for _, pair := range strings.Fields(out) {
			name, status, _ := strings.Cut(pair, "=")
			if status == "True" && !strings.Contains(name, "172-26-11-140") {
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
