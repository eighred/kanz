package arch

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

// Two tunnel connectors can route one browser to different in-memory stores.
// replicas: 1 alone permits that split during the default rolling update.
func TestBFFRolloutPreservesSingleSessionOwner(t *testing.T) {
	root := moduleRoot(t)
	canonical, err := os.ReadFile(filepath.Join(root, "infra/deploy/web-bff-deploy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := exec.Command("kubectl", "kustomize", "--load-restrictor=LoadRestrictionsNone", filepath.Join(root, "infra/overlays/testnet-tokyo")).Output()
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{"canonical": canonical, "Tokyo": rendered} {
		t.Run(name, func(t *testing.T) {
			found := 0
			for _, doc := range bytes.Split(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")), []byte("\n---\n")) {
				var d appsv1.Deployment
				if err := yaml.Unmarshal(doc, &d); err != nil {
					t.Fatal(err)
				}
				if d.Kind != "Deployment" || d.Name != "web-bff" {
					continue
				}
				found++
				if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 || d.Labels[drSingletonLabel] != "true" {
					t.Fatal("BFF must retain its labelled singleton pin until sessions are shared")
				}
				if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || d.Spec.Strategy.RollingUpdate != nil {
					t.Fatal("BFF rollout can admit overlapping session owners")
				}
			}
			if found != 1 {
				t.Fatalf("found %d BFF Deployments, want one", found)
			}
		})
	}
}
