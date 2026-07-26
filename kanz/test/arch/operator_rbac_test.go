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
// ClusterRole grants exactly get/list on nodes/pods and nothing else. The
// operator is the first workload with Kubernetes-API RBAC; this guard keeps
// it least-privilege — a write verb or extra resource fails the build.
func TestOperatorClusterRoleIsNodeReadOnly(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "infra", "deploy", "operator-deploy.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	allowedResources := map[string]bool{"nodes": true, "pods": true}
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
				if !allowedResources[res] {
					t.Errorf("operator-node-reader grants resource %q; only nodes/pods (read) are allowed", res)
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

// TestOperatorNodeWriterRoleIsBounded asserts the node-writer ClusterRole grants
// exactly patch-on-nodes + create-on-pods/eviction, and NO node create/delete and NO
// verbs on the bare pods resource — so drain is eviction-only (PDB-honoring) and the
// operator can never remove a node object.
func TestOperatorNodeWriterRoleIsBounded(t *testing.T) {
	docs := decodeOperatorManifest(t)
	forbiddenNodeVerbs := map[string]bool{"create": true, "delete": true, "deletecollection": true, "update": true, "*": true}

	var found bool
	for _, doc := range docs {
		if doc.Kind != "ClusterRole" || doc.Metadata.Name != "operator-node-writer" {
			continue
		}
		found = true
		for _, r := range doc.Rules {
			for _, res := range r.Resources {
				switch res {
				case "nodes":
					for _, v := range r.Verbs {
						lv := strings.ToLower(v)
						if forbiddenNodeVerbs[lv] {
							t.Errorf("operator-node-writer grants %q on nodes — only patch is allowed (no create/delete/update)", v)
						}
						if lv != "patch" {
							t.Errorf("operator-node-writer grants verb %q on nodes; only patch (cordon) is allowed", v)
						}
					}
				case "pods/eviction":
					for _, v := range r.Verbs {
						if strings.ToLower(v) != "create" {
							t.Errorf("operator-node-writer grants %q on pods/eviction; only create is allowed", v)
						}
					}
				case "pods":
					// A bare "pods: delete" would let drain force-delete, bypassing PDBs.
					t.Errorf("operator-node-writer must not grant verbs on the bare pods resource (only pods/eviction) — got verbs %v", r.Verbs)
				default:
					t.Errorf("operator-node-writer grants resource %q; only nodes + pods/eviction are allowed", res)
				}
			}
		}
	}
	if !found {
		t.Fatalf("no ClusterRole operator-node-writer found — S3a RBAC missing")
	}
}

// roleDoc / saDoc reuse the yaml.v3 multi-doc decode pattern from
// TestOperatorClusterRoleIsNodeReadOnly.
type roleDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	AutomountServiceAccountToken *bool `yaml:"automountServiceAccountToken"`
	ImagePullSecrets            []struct {
		Name string `yaml:"name"`
	} `yaml:"imagePullSecrets"`
	Rules []struct {
		APIGroups []string `yaml:"apiGroups"`
		Resources []string `yaml:"resources"`
		Verbs     []string `yaml:"verbs"`
	} `yaml:"rules"`
}

// TestOperatorProvisionerRoleIsBounded asserts the S2a operator-provisioner
// Role is namespaced (never cluster-scoped), never grants a verb on nodes,
// and only ever grants jobs/secrets — the resource set the provisioning
// workflow actually needs. A drift toward node access or a new resource
// fails the build.
func TestOperatorProvisionerRoleIsBounded(t *testing.T) {
	docs := decodeOperatorManifest(t)
	allowedResources := map[string]bool{"jobs": true, "secrets": true}
	var found bool
	for _, d := range docs {
		if d.Kind != "Role" || d.Metadata.Name != "operator-provisioner" {
			continue
		}
		found = true
		if d.Metadata.Namespace != "kanz-operator" {
			t.Errorf("operator-provisioner Role must be namespaced to kanz-operator, got %q", d.Metadata.Namespace)
		}
		for _, r := range d.Rules {
			for _, res := range r.Resources {
				if res == "nodes" {
					t.Errorf("operator-provisioner grants a verb on NODES — the operator must never write node objects")
				}
				if !allowedResources[res] {
					t.Errorf("operator-provisioner grants resource %q; only jobs/secrets are allowed", res)
				}
			}
		}
	}
	if !found {
		t.Fatalf("no namespaced Role operator-provisioner found — S2a RBAC missing")
	}
}

// TestProvisionerServiceAccountHasNoToken asserts the kanz-node-provisioner
// ServiceAccount never automounts a token: the provisioning Job needs no
// Kubernetes API access at all, only outbound SSH.
func TestProvisionerServiceAccountHasNoToken(t *testing.T) {
	docs := decodeOperatorManifest(t)
	var found bool
	for _, d := range docs {
		if d.Kind != "ServiceAccount" || d.Metadata.Name != "kanz-node-provisioner" {
			continue
		}
		found = true
		if d.AutomountServiceAccountToken == nil || *d.AutomountServiceAccountToken {
			t.Errorf("kanz-node-provisioner must set automountServiceAccountToken: false — the Job needs no k8s API access")
		}
	}
	if !found {
		t.Fatalf("no ServiceAccount kanz-node-provisioner found — provisioner isolation missing")
	}
}

// TestProvisionerServiceAccountHasPullSecret asserts kanz-node-provisioner carries
// imagePullSecrets: [{name: ghcr-pull}].
//
// WHY THIS SA NEEDS ITS OWN TEST. TestPrivateImagesHavePullSecrets (supplychain_test.go)
// walks infra/ for pod-bearing manifests that reference a private image and checks the
// ServiceAccount each one names — but it finds ServiceAccounts by finding the WORKLOAD
// first. kanz-node-provisioner has no workload manifest to find: the two Jobs that use
// it (jobSpec, probeJobSpec) are built in Go inside services/operator/internal/provision,
// not declared anywhere under infra/. That guard's own walk is structurally blind to this
// ServiceAccount, so deleting its imagePullSecrets block passes the entire arch suite
// while silently restoring ErrImagePull on every Add Node and Test Connection run.
func TestProvisionerServiceAccountHasPullSecret(t *testing.T) {
	docs := decodeOperatorManifest(t)
	const pullSecretName = "ghcr-pull"
	var found bool
	for _, d := range docs {
		if d.Kind != "ServiceAccount" || d.Metadata.Name != "kanz-node-provisioner" {
			continue
		}
		found = true
		has := false
		for _, s := range d.ImagePullSecrets {
			if s.Name == pullSecretName {
				has = true
			}
		}
		if !has {
			t.Errorf("kanz-node-provisioner must carry imagePullSecrets: [{name: %s}] — its two "+
				"Jobs (jobSpec, probeJobSpec in services/operator/internal/provision/provision.go) "+
				"are built in Go and have no manifest under infra/, so TestPrivateImagesHavePullSecrets "+
				"cannot see them; this ServiceAccount is the only thing standing between them and "+
				"ErrImagePull on a node that has not pre-loaded kanz-provisioner", pullSecretName)
		}
	}
	if !found {
		t.Fatalf("no ServiceAccount kanz-node-provisioner found — provisioner isolation missing")
	}
}

// decodeOperatorManifest reads infra/deploy/operator-deploy.yaml into typed docs.
func decodeOperatorManifest(t *testing.T) []roleDoc {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "infra", "deploy", "operator-deploy.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var docs []roleDoc
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	for {
		var d roleDoc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		docs = append(docs, d)
	}
	if len(docs) == 0 {
		t.Fatalf("no docs decoded from %s", path)
	}
	return docs
}
