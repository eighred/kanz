package arch

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

// Two tunnel connectors can route one browser to different in-memory stores.
// replicas: 1 alone permits that split during the default rolling update.
// #1288 must verify shared-store production placement and DR before this pin changes.
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
				for _, container := range d.Spec.Template.Spec.Containers {
					for _, env := range container.Env {
						if env.Name == "WEB_BFF_SESSION_MODE" && (env.Value != "memory" || env.ValueFrom != nil) {
							t.Fatal("shared-session production activation requires #1288 rollout proof")
						}
						if env.Name == "WEB_BFF_SESSION_DSN" || env.Name == "WEB_BFF_SESSION_DSN_FILE" || env.Name == "WEB_BFF_SESSION_KEY" || env.Name == "WEB_BFF_SESSION_KEY_FILE" {
							t.Fatal("production shared-session secret wiring requires #1288 rollout proof")
						}
					}
				}
				if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 || d.Labels[drSingletonLabel] != "true" {
					t.Fatal("BFF must retain its labelled singleton pin until sessions are shared")
				}
				if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || d.Spec.Strategy.RollingUpdate != nil {
					t.Fatal("BFF rollout can admit overlapping session owners")
				}
				var object map[string]any
				if err := yaml.Unmarshal(doc, &object); err != nil {
					t.Fatal(err)
				}
				strategy := object["spec"].(map[string]any)["strategy"].(map[string]any)
				if value, present := strategy["rollingUpdate"]; !present || value != nil {
					t.Fatal("Recreate must explicitly clear the existing API-defaulted rollingUpdate field")
				}
				if name == "Tokyo" {
					release := d.Spec.Template.Labels["kanz.io/release"]
					if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(release) || d.Annotations["kanz.io/release-commit"] != release || d.Spec.Template.Annotations["kanz.io/release-commit"] != release {
						t.Fatal("BFF release provenance must survive the overlay's common annotations")
					}
				}
			}
			if found != 1 {
				t.Fatalf("found %d BFF Deployments, want one", found)
			}
		})
	}
}
