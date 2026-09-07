package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type testnetKustomization struct {
	APIVersion        string            `yaml:"apiVersion"`
	Kind              string            `yaml:"kind"`
	Resources         []string          `yaml:"resources"`
	CommonAnnotations map[string]string `yaml:"commonAnnotations"`
	Images            []struct {
		Name    string `yaml:"name"`
		NewName string `yaml:"newName"`
		Digest  string `yaml:"digest"`
	} `yaml:"images"`
	Patches []struct {
		Target struct {
			Group   string `yaml:"group"`
			Version string `yaml:"version"`
			Kind    string `yaml:"kind"`
			Name    string `yaml:"name"`
		} `yaml:"target"`
		Patch string `yaml:"patch"`
	} `yaml:"patches"`
}

func TestTokyoTestnetOverlayLocksEveryCapitalPathImageToECR(t *testing.T) {
	root := moduleRoot(t)
	overlayDir := filepath.Join(root, "infra", "overlays", "testnet-tokyo")
	raw, err := os.ReadFile(filepath.Join(overlayDir, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var overlay testnetKustomization
	if err := yaml.UnmarshalStrict(raw, &overlay); err != nil {
		t.Fatal(err)
	}

	wantResources := []string{
		"../../deploy/accounting-deploy.yaml",
		"../../deploy/api-gateway-deploy.yaml",
		"../../deploy/audit-deploy.yaml",
		"../../deploy/compliance-deploy.yaml",
		"../../deploy/identity-deploy.yaml",
		"../../deploy/oms-deploy.yaml",
		"../../deploy/risk-engine-rollout.yaml",
		"../../deploy/venue-binance-deploy.yaml",
		"../../deploy/venue-okx-deploy.yaml",
	}
	assertSameStrings(t, "overlay resources", overlay.Resources, wantResources)

	const registry = "012619468098.dkr.ecr.ap-northeast-1.amazonaws.com/"
	wantImages := []string{
		"accounting", "api-gateway", "audit", "compliance", "identity",
		"kanz-migrate", "oms", "risk-engine", "venue-binance", "venue-okx",
	}
	digestPattern := regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	mapped := make(map[string]struct{}, len(overlay.Images))
	gotImages := make([]string, 0, len(overlay.Images))
	for _, image := range overlay.Images {
		service := strings.TrimPrefix(image.Name, "ghcr.io/eighred/")
		if service == image.Name {
			t.Errorf("source image %q is outside the canonical Eighred registry", image.Name)
		}
		if image.NewName != registry+service {
			t.Errorf("image %q rewrites to %q, want %q", image.Name, image.NewName, registry+service)
		}
		if !digestPattern.MatchString(image.Digest) {
			t.Errorf("image %q has non-immutable digest %q", image.Name, image.Digest)
		}
		if _, duplicate := mapped[image.Name]; duplicate {
			t.Errorf("source image %q is mapped more than once", image.Name)
		}
		mapped[image.Name] = struct{}{}
		gotImages = append(gotImages, service)
	}
	assertSameStrings(t, "overlay images", gotImages, wantImages)

	imagePattern := regexp.MustCompile(`(?m)^\s*image:\s*([^\s@]+)(?:@[^\s]+)?\s*$`)
	for _, resource := range overlay.Resources {
		base, readErr := os.ReadFile(filepath.Clean(filepath.Join(overlayDir, resource)))
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, match := range imagePattern.FindAllSubmatch(base, -1) {
			name := string(match[1])
			if _, ok := mapped[name]; !ok {
				t.Errorf("%s contains image %q without an ECR digest mapping", resource, name)
			}
		}
	}

	commitPattern := regexp.MustCompile(`^[0-9a-f]{40}$`)
	if commit := overlay.CommonAnnotations["kanz.io/release-commit"]; !commitPattern.MatchString(commit) {
		t.Errorf("release commit %q is not an immutable Git commit", commit)
	}
	if overlay.CommonAnnotations["kanz.io/testnet-region"] != "ap-northeast-1" {
		t.Error("Tokyo overlay is not pinned to ap-northeast-1")
	}

	var removesPullSecret, scalesDeployments, scalesRisk bool
	for _, patch := range overlay.Patches {
		switch {
		case patch.Target.Kind == "ServiceAccount" && strings.Contains(patch.Patch, "/imagePullSecrets"):
			removesPullSecret = true
		case patch.Target.Kind == "Deployment" && strings.Contains(patch.Patch, "/spec/replicas") && strings.Contains(patch.Patch, "value: 1"):
			scalesDeployments = true
		case patch.Target.Kind == "Rollout" && patch.Target.Name == "risk-engine" && strings.Contains(patch.Patch, "/spec/replicas") && strings.Contains(patch.Patch, "value: 1"):
			scalesRisk = true
		}
	}
	if !removesPullSecret || !scalesDeployments || !scalesRisk {
		t.Fatalf("testnet patches incomplete: removesPullSecret=%t scalesDeployments=%t scalesRisk=%t", removesPullSecret, scalesDeployments, scalesRisk)
	}
}

func TestTokyoWorkloadInstallerProvesMergedInputsAndRunningDigests(t *testing.T) {
	root := moduleRoot(t)
	raw := string(mustReadArchFile(t, filepath.Join(filepath.Dir(root), "tools", "Install-TestnetWorkloads.ps1")))
	for _, required := range []string{
		"fetch origin main", "HEAD $headCommit is not exact origin/main", "git -C $repoRoot diff --quiet",
		"resourceCount -ne 34", "Expected 16 rendered container images", "sha256sum --check --status",
		"condition=Ready cluster/kanz-testnet-postgres", "condition=complete job/postgres-migrations",
		"rollout status statefulset/nats", "rollout status statefulset/redis",
		"apply --server-side --dry-run=server", "rollout status \"deployment/${deployment}\"",
		"condition=Available deployment/argo-rollouts", "status.phase}''=Healthy rollout/risk-engine",
		"status.imageID", "capture(\"@(?<digest>sha256:[0-9a-f]{64})$\")",
		"kubernetes.io/dockerconfigjson", "workload-registry-secrets-absent",
	} {
		if !strings.Contains(raw, required) {
			t.Errorf("Tokyo workload installer is missing fail-closed proof %q", required)
		}
	}
}

func assertSameStrings(t *testing.T, name string, got, want []string) {
	t.Helper()
	got = append([]string(nil), got...)
	want = append([]string(nil), want...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("%s mismatch: got=%v want=%v", name, got, want)
	}
}
