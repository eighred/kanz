package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Optional coordination must not become an active manifest selection merely
// because its preparation is merged. Runtime/image acceptance remains #111.
func TestRiskSharedDedupComponentRemainsUnselected(t *testing.T) {
	root := moduleRoot(t)
	component := filepath.Join(root, "infra", "overlays", "risk_shared_dedup")
	for path := range argoSyncedPaths(t, root) {
		selected := filepath.Join(filepath.Dir(root), filepath.FromSlash(path))
		if selected == component || strings.HasPrefix(component, selected+string(filepath.Separator)) {
			t.Fatalf("Argo path %s automatically selects the optional risk dedup component", path)
		}
	}
	inspected := 0
	for _, path := range infraYAMLFiles(t) {
		if filepath.Base(path) != "kustomization.yaml" || filepath.Dir(path) == component {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var selection struct {
			Resources  []string `yaml:"resources"`
			Components []string `yaml:"components"`
			Bases      []string `yaml:"bases"`
		}
		if err := yaml.Unmarshal(body, &selection); err != nil {
			t.Fatal(err)
		}
		inspected++
		refs := append(append(selection.Resources, selection.Components...), selection.Bases...)
		for _, ref := range refs {
			selected := filepath.Clean(filepath.Join(filepath.Dir(path), ref))
			if selected == component || strings.HasPrefix(selected, component+string(filepath.Separator)) {
				t.Fatalf("%s selects unqualified shared dedup through %s", path, ref)
			}
		}
	}
	if inspected < 2 {
		t.Fatal("kustomization inventory is incomplete; unselected proof would be vacuous")
	}
	// The base still selects per-instance mode and no secret projection.
	base, err := os.ReadFile(filepath.Join(root, "infra", "deploy", "risk-engine-rollout.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stripYAMLComments(string(base))), "RISK_ENGINE_REDIS_URL") {
		t.Fatal("canonical risk workload selects shared dedup before explicit promotion")
	}
}

func TestRiskSharedDedupSecretIsDedicatedAndReadOnly(t *testing.T) {
	root := filepath.Join(moduleRoot(t), "infra", "overlays", "risk_shared_dedup")
	body, err := os.ReadFile(filepath.Join(root, "secret-provider-class.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var secret struct {
		Spec struct {
			Parameters struct {
				Role    string `yaml:"roleName"`
				Objects string `yaml:"objects"`
			} `yaml:"parameters"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(body, &secret); err != nil {
		t.Fatal(err)
	}
	var objects []struct {
		Name string `yaml:"objectName"`
		Path string `yaml:"secretPath"`
		Key  string `yaml:"secretKey"`
	}
	if err := yaml.Unmarshal([]byte(secret.Spec.Parameters.Objects), &objects); err != nil {
		t.Fatal(err)
	}
	if secret.Spec.Parameters.Role != "risk-engine-dedup" || len(objects) != 1 ||
		objects[0].Name != "url" || objects[0].Path != "kv/data/kanz/risk-engine-dedup" || objects[0].Key != "url" {
		t.Fatal("optional dedup may project only its dedicated endpoint record/field through its separate Vault role")
	}
	component, err := os.ReadFile(filepath.Join(root, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Kind    string `yaml:"kind"`
		Patches []struct {
			Patch string `yaml:"patch"`
		} `yaml:"patches"`
	}
	if err := yaml.Unmarshal(component, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Kind != "Component" || len(manifest.Patches) != 1 {
		t.Fatal("optional selection must be an explicit component with one bounded runtime patch")
	}
	var ops []struct {
		Op    string         `yaml:"op"`
		Path  string         `yaml:"path"`
		Value map[string]any `yaml:"value"`
	}
	if err := yaml.Unmarshal([]byte(manifest.Patches[0].Patch), &ops); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 3 || ops[0].Op != "add" || ops[1].Op != "add" || ops[2].Op != "add" ||
		ops[0].Path != "/spec/template/spec/containers/0/env/-" || ops[0].Value["name"] != "RISK_ENGINE_REDIS_URL_FILE" ||
		ops[0].Value["value"] != "/run/secrets/risk-dedup/url" ||
		ops[1].Path != "/spec/template/spec/containers/0/volumeMounts/-" || ops[1].Value["readOnly"] != true ||
		ops[1].Value["mountPath"] != "/run/secrets/risk-dedup" || ops[1].Value["name"] != "risk-dedup" ||
		ops[2].Path != "/spec/template/spec/volumes/-" || ops[2].Value["name"] != "risk-dedup" {
		t.Fatal("selection must mount only the dedicated endpoint file into the runtime, read-only")
	}
	csi, ok := ops[2].Value["csi"].(map[string]any)
	if !ok || csi["readOnly"] != true || csi["driver"] != "secrets-store.csi.k8s.io" {
		t.Fatal("dedup credential volume must remain read-only CSI")
	}
	attrs, ok := csi["volumeAttributes"].(map[string]any)
	if !ok || attrs["secretProviderClass"] != "risk-engine-dedup" {
		t.Fatal("dedup volume must refer to the dedicated secret projection")
	}
}
