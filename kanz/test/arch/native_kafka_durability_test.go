package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nativeDurabilityBoundary(files map[string]string) bool {
	for key, anchors := range map[string][]string{
		"plan":      {`ConfigName: "write.caching", ConfigValue: "false"`, `ConfigName: "min.insync.replicas", ConfigValue: "2"`},
		"fixture":   {"RequiredAcks: kafka.RequireAll", "nativeQuorum(ctx, client, mode)", `nativeReadContains(ctx, c, topic, "quorum-majority")`, `nativeQuorumMetadata(ctx, client, mode, os.Getenv("KANZ_NATIVE_EXPECTED_CLUSTER"))`, "len(seen) == 2 && seen[0] && seen[1]", "len(seen) == 2 && seen[1] && seen[2]"},
		"qualifier": {"('quorum-minority',())", "('quorum-majority',topology.BROKERS[:2])", "apply(topology.policies(connected))", "-durability-profile=redpanda-raft", "stable_broker(broker,pods.get('items'),evidence['brokerPodsBefore'][broker])", "allocation!=str(topology.FALLOC_BYTES)"},
		"topology":  {"--unsafe-bypass-fsync=false", "'write_caching_default': 'false'"},
	} {
		for _, anchor := range anchors {
			if !strings.Contains(files[key], anchor) {
				return false
			}
		}
	}
	return true
}

func TestNativeDurabilityQualificationAndMutations(t *testing.T) {
	root := moduleRoot(t)
	files := map[string]string{}
	for key, path := range map[string]string{
		"plan":      "cmd/kanz-kafka-provision/internal/provision/plan.go",
		"fixture":   "cmd/kanz-kafka-provision/native_fixture_test.go",
		"qualifier": "../tools/native-kafka-provision/qualify.py",
		"topology":  "../tools/native-kafka-provision/topology.py",
	} {
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		files[key] = string(raw)
	}
	if !nativeDurabilityBoundary(files) {
		t.Fatal("durability qualification boundary changed")
	}
	for _, m := range []struct{ key, old, new string }{
		{"fixture", "RequiredAcks: kafka.RequireAll", "RequiredAcks: kafka.RequireOne"},
		{"fixture", "len(seen) == 2 && seen[0] && seen[1]", "len(seen) == 3"},
		{"fixture", "len(seen) == 2 && seen[1] && seen[2]", "len(seen) >= 1"},
		{"fixture", `nativeReadContains(ctx, c, topic, "quorum-majority")`, `nativeReadContains(ctx, c, topic, "unrelated")`},
		{"qualifier", "('quorum-minority',())", "('quorum-minority',topology.BROKERS)"},
		{"qualifier", "apply(topology.policies(connected))", "apply(topology.policies())"},
		{"qualifier", "stable_broker(broker,pods.get('items'),evidence['brokerPodsBefore'][broker])", "stable_broker(broker,pods.get('items'))"},
		{"qualifier", "allocation!=str(topology.FALLOC_BYTES)", "False"},
		{"topology", "--unsafe-bypass-fsync=false", "--unsafe-bypass-fsync=true"},
		{"plan", `ConfigName: "write.caching", ConfigValue: "false"`, `ConfigName: "write.caching", ConfigValue: "true"`},
		{"plan", `ConfigName: "min.insync.replicas", ConfigValue: "2"`, `ConfigName: "min.insync.replicas", ConfigValue: "1"`},
	} {
		copy := map[string]string{}
		for k, v := range files {
			copy[k] = v
		}
		if !strings.Contains(copy[m.key], m.old) {
			t.Fatal("mutation anchor absent")
		}
		copy[m.key] = strings.Replace(copy[m.key], m.old, m.new, 1)
		if nativeDurabilityBoundary(copy) {
			t.Fatal("weakened quorum proof escaped guard")
		}
	}
}
