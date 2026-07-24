package arch

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// clusterRoleDoc captures only the fields this guard inspects from a
// multi-document manifest.
type clusterRoleDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Rules []struct {
		APIGroups []string `yaml:"apiGroups"`
		Resources []string `yaml:"resources"`
		Verbs     []string `yaml:"verbs"`
	} `yaml:"rules"`
}

// TestOperatorClusterRoleIsNodeReadOnly asserts the operator service's
// ClusterRole grants exactly get/list on nodes and nothing else. The operator
// is the first workload with Kubernetes-API RBAC; this guard keeps it
// least-privilege — a write verb or extra resource fails the build.
func TestOperatorClusterRoleIsNodeReadOnly(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "infra", "deploy", "operator-deploy.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	allowedVerbs := map[string]bool{"get": true, "list": true}
	writeVerbs := map[string]bool{
		"create": true, "update": true, "patch": true,
		"delete": true, "deletecollection": true, "*": true,
	}

	var found bool
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var doc clusterRoleDoc
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse %s as YAML: %v", path, err)
		}
		if doc.Kind != "ClusterRole" || doc.Metadata.Name != "operator-node-reader" {
			continue
		}
		found = true
		if len(doc.Rules) == 0 {
			t.Fatalf("operator-node-reader has no rules")
		}
		for _, r := range doc.Rules {
			for _, res := range r.Resources {
				if res != "nodes" {
					t.Errorf("operator-node-reader grants resource %q; only nodes is allowed", res)
				}
			}
			for _, v := range r.Verbs {
				lv := strings.ToLower(v)
				if writeVerbs[lv] {
					t.Errorf("operator-node-reader grants WRITE verb %q — the operator read spine must never mutate", v)
				}
				if !allowedVerbs[lv] {
					t.Errorf("operator-node-reader grants verb %q; only get/list are allowed", v)
				}
			}
		}
	}

	// Non-vacuity: a guard that finds nothing to check is a guard that passes
	// forever after the manifest is renamed or the role deleted.
	if !found {
		t.Fatalf("no ClusterRole named operator-node-reader found in %s", path)
	}
}
