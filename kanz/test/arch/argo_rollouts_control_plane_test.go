package arch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type rolloutReleaseLock struct {
	Version         string `json:"version"`
	SourceCommit    string `json:"sourceCommit"`
	ManifestURL     string `json:"manifestURL"`
	ManifestSHA256  string `json:"manifestSHA256"`
	ControllerImage string `json:"controllerImage"`
}

type rolloutKustomization struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Namespace  string   `yaml:"namespace"`
	Resources  []string `yaml:"resources"`
	Images     []struct {
		Name    string `yaml:"name"`
		NewName string `yaml:"newName"`
		Digest  string `yaml:"digest"`
		NewTag  string `yaml:"newTag"`
	} `yaml:"images"`
	Patches []struct {
		Path   string `yaml:"path"`
		Target struct {
			Group   string `yaml:"group"`
			Version string `yaml:"version"`
			Kind    string `yaml:"kind"`
			Name    string `yaml:"name"`
		} `yaml:"target"`
	} `yaml:"patches"`
}

func TestArgoRolloutsControlPlaneLocksSupplyChainAndAvailability(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, "infra", "controllers", "argo-rollouts")

	lockRaw, err := os.ReadFile(filepath.Join(dir, "release-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock rolloutReleaseLock
	if err := json.Unmarshal(lockRaw, &lock); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(lock.Version) {
		t.Errorf("release version %q is not exact semver", lock.Version)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(lock.SourceCommit) {
		t.Errorf("source commit %q is not immutable", lock.SourceCommit)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(lock.ManifestSHA256) {
		t.Errorf("manifest checksum %q is not SHA-256", lock.ManifestSHA256)
	}
	wantURL := "https://github.com/argoproj/argo-rollouts/releases/download/" + lock.Version + "/install.yaml"
	if lock.ManifestURL != wantURL {
		t.Errorf("manifest URL = %q, want %q", lock.ManifestURL, wantURL)
	}
	imageParts := strings.Split(lock.ControllerImage, "@")
	if len(imageParts) != 2 || imageParts[0] != "quay.io/argoproj/argo-rollouts" ||
		!regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(imageParts[1]) {
		t.Fatalf("controller image %q is not an immutable approved image", lock.ControllerImage)
	}

	kustomizationRaw, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var k rolloutKustomization
	if err := yaml.UnmarshalStrict(kustomizationRaw, &k); err != nil {
		t.Fatal(err)
	}
	if k.Namespace != "argo-rollouts" {
		t.Errorf("controller namespace = %q", k.Namespace)
	}
	assertSameStrings(t, "Argo Rollouts resources", k.Resources, []string{"namespace.yaml", "upstream-install.yaml", "pdb.yaml", "network-policy.yaml"})
	if len(k.Images) != 1 || k.Images[0].Name != imageParts[0] || k.Images[0].NewName != imageParts[0] ||
		k.Images[0].Digest != imageParts[1] || k.Images[0].NewTag != "" {
		t.Fatalf("controller image transform does not match release lock: %+v", k.Images)
	}
	if len(k.Patches) != 1 || k.Patches[0].Path != "deployment-patch.yaml" ||
		k.Patches[0].Target.Group != "apps" || k.Patches[0].Target.Version != "v1" ||
		k.Patches[0].Target.Kind != "Deployment" || k.Patches[0].Target.Name != "argo-rollouts" {
		t.Fatalf("controller hardening patch target is not exact: %+v", k.Patches)
	}
	if _, err := os.Stat(filepath.Join(dir, "upstream-install.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("runtime-fetched upstream-install.yaml must not be committed or retained")
	}

	namespaceRaw, err := os.ReadFile(filepath.Join(dir, "namespace.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var namespace struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name   string            `yaml:"name"`
			Labels map[string]string `yaml:"labels"`
		} `yaml:"metadata"`
	}
	if err := yaml.UnmarshalStrict(namespaceRaw, &namespace); err != nil {
		t.Fatal(err)
	}
	if namespace.Metadata.Name != "argo-rollouts" ||
		namespace.Metadata.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Fatalf("controller namespace is not restricted: %+v", namespace.Metadata)
	}

	patchRaw, err := os.ReadFile(filepath.Join(dir, "deployment-patch.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var deploymentPatch struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
		Spec struct {
			Replicas             int `yaml:"replicas"`
			RevisionHistoryLimit int `yaml:"revisionHistoryLimit"`
			Strategy             struct {
				Type          string `yaml:"type"`
				RollingUpdate struct {
					MaxUnavailable int `yaml:"maxUnavailable"`
					MaxSurge       int `yaml:"maxSurge"`
				} `yaml:"rollingUpdate"`
			} `yaml:"strategy"`
			Template struct {
				Spec struct {
					Containers []struct {
						Name            string         `yaml:"name"`
						ImagePullPolicy string         `yaml:"imagePullPolicy"`
						ReadinessProbe  map[string]any `yaml:"readinessProbe"`
						Resources       struct {
							Requests map[string]string `yaml:"requests"`
							Limits   map[string]string `yaml:"limits"`
						} `yaml:"resources"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.UnmarshalStrict(patchRaw, &deploymentPatch); err != nil {
		t.Fatal(err)
	}
	containers := deploymentPatch.Spec.Template.Spec.Containers
	if deploymentPatch.Spec.Replicas != 1 || deploymentPatch.Spec.Strategy.RollingUpdate.MaxUnavailable != 0 || len(containers) != 1 {
		t.Fatalf("controller single-node availability patch is incomplete: %+v", deploymentPatch.Spec)
	}
	c := containers[0]
	if c.Name != "argo-rollouts" || c.ImagePullPolicy != "IfNotPresent" || c.ReadinessProbe == nil ||
		len(c.Resources.Requests) != 3 || len(c.Resources.Limits) != 3 {
		t.Fatalf("controller resource/readiness bounds are incomplete: %+v", c)
	}

	pdbRaw, err := os.ReadFile(filepath.Join(dir, "pdb.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var pdb struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
		Spec struct {
			MinAvailable int `yaml:"minAvailable"`
			Selector     struct {
				MatchLabels map[string]string `yaml:"matchLabels"`
			} `yaml:"selector"`
		} `yaml:"spec"`
	}
	if err := yaml.UnmarshalStrict(pdbRaw, &pdb); err != nil {
		t.Fatal(err)
	}
	if pdb.Spec.MinAvailable != 1 || pdb.Spec.Selector.MatchLabels["app.kubernetes.io/name"] != "argo-rollouts" {
		t.Fatalf("controller disruption guard is incomplete: %+v", pdb.Spec)
	}
}
