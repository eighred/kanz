package tui

import (
	"os/exec"
	"strings"
	"testing"
)

// kubectl runs a read-only query against the cluster. Every assertion in this package
// goes through here rather than through the TUI's rendering: a UI that shows its
// optimistic intent instead of observed state must FAIL these proofs, and it can only
// do that if the oracle is independent of it.
func kubectl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("kubectl", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
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
