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
func podsOnNode(t *testing.T, node string) []string {
	t.Helper()
	out := kubectl(t, "get", "pods", "-A", "--field-selector",
		"spec.nodeName="+node+",status.phase=Running",
		"-o", "jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name} {end}")
	if out == "" {
		return nil
	}
	return strings.Fields(out)
}

func secretDataKeys(t *testing.T, ns, name string) []string {
	t.Helper()
	out := kubectl(t, "-n", ns, "get", "secret", name,
		"-o", "jsonpath={range $k, $v := .data}{$k} {end}")
	return strings.Fields(out)
}

func nodeExists(t *testing.T, node string) bool {
	t.Helper()
	out, err := exec.Command("kubectl", "get", "node", node, "-o", "name").CombinedOutput()
	_ = out
	return err == nil
}
