package tui

import "testing"

func TestOracleReadsNodeState(t *testing.T) {
	env := requireEnv(t) // skips unless the E2E env is configured
	node1 := kubectl(t, "get", "nodes", "-o", "jsonpath={.items[0].metadata.name}")
	if node1 == "" {
		t.Fatal("oracle returned no node name; kubectl is not usable from here, so no proof " +
			"in this package can assert against the cluster")
	}
	if !nodeSchedulable(t, node1) {
		t.Errorf("node %s reports unschedulable at rest — check nothing left it cordoned", node1)
	}
	_ = env
}
