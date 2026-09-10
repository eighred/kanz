package arch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
		"../../security/secrets/secretproviderclass.yaml",
		"../../deploy/analysis-template.yaml",
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

func TestTokyoTestnetOverlayRetainsExactlyTheVaultClassesItsWorkloadsMount(t *testing.T) {
	root := moduleRoot(t)
	overlayDir := filepath.Join(root, "infra", "overlays", "testnet-tokyo")
	raw := mustReadArchFile(t, filepath.Join(overlayDir, "kustomization.yaml"))
	var overlay testnetKustomization
	if err := yaml.UnmarshalStrict(raw, &overlay); err != nil {
		t.Fatal(err)
	}

	var deleted *regexp.Regexp
	for _, patch := range overlay.Patches {
		if patch.Target.Kind == "SecretProviderClass" && strings.Contains(patch.Patch, "$patch: delete") {
			var err error
			deleted, err = regexp.Compile(patch.Target.Name)
			if err != nil {
				t.Fatalf("compile SecretProviderClass exclusion: %v", err)
			}
		}
	}
	if deleted == nil {
		t.Fatal("Tokyo overlay does not explicitly exclude unmounted canonical Vault classes")
	}

	type document struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec struct {
			Template struct {
				Spec struct {
					Volumes []struct {
						CSI struct {
							VolumeAttributes struct {
								SecretProviderClass string `yaml:"secretProviderClass"`
							} `yaml:"volumeAttributes"`
						} `yaml:"csi"`
					} `yaml:"volumes"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}

	var mounted []string
	for _, resource := range overlay.Resources {
		if resource == "../../security/secrets/secretproviderclass.yaml" || resource == "../../deploy/analysis-template.yaml" {
			continue
		}
		body := mustReadArchFile(t, filepath.Clean(filepath.Join(overlayDir, resource)))
		for _, part := range bytes.Split(body, []byte("\n---")) {
			var doc document
			if err := yaml.Unmarshal(part, &doc); err != nil {
				t.Fatalf("decode %s: %v", resource, err)
			}
			for _, volume := range doc.Spec.Template.Spec.Volumes {
				if name := volume.CSI.VolumeAttributes.SecretProviderClass; name != "" {
					mounted = append(mounted, name)
				}
			}
		}
	}

	canonical := mustReadArchFile(t, filepath.Join(root, "infra", "security", "secrets", "secretproviderclass.yaml"))
	var retained []string
	for _, part := range bytes.Split(canonical, []byte("\n---")) {
		var doc document
		if err := yaml.Unmarshal(part, &doc); err != nil {
			t.Fatalf("decode canonical Vault class: %v", err)
		}
		if doc.Kind == "SecretProviderClass" && !deleted.MatchString(doc.Metadata.Name) {
			retained = append(retained, doc.Metadata.Name)
		}
	}
	assertSameStrings(t, "retained Vault classes versus mounted classes", retained, mounted)
	if len(retained) != 12 {
		t.Fatalf("Tokyo minimum graph retained %d Vault classes, want 12", len(retained))
	}
}

func TestTokyoWorkloadInstallerProvesMergedInputsAndRunningDigests(t *testing.T) {
	root := moduleRoot(t)
	raw := string(mustReadArchFile(t, filepath.Join(filepath.Dir(root), "tools", "Install-TestnetWorkloads.ps1")))
	for _, required := range []string{
		"fetch origin main", "HEAD $headCommit is not exact origin/main", "git -C $repoRoot diff --quiet",
		"resourceCount -ne 47", "Expected 16 rendered container images", "sha256sum --check --status",
		"imageTag=$imageReleaseCommit", "Tokyo ECR does not retain $repository@$digest under release",
		"name: risk-engine-canary", "name: identity-signing-key", "name: venue-binance-keys", "name: venue-okx-keys",
		"condition=Ready cluster/kanz-testnet-postgres", "condition=complete job/postgres-migrations",
		"rollout status statefulset/nats", "rollout status statefulset/redis",
		"apply --server-side --dry-run=server", "rollout status \"deployment/${deployment}\"",
		"condition=Available deployment/argo-rollouts", "status.phase}''=Healthy rollout/risk-engine",
		"Verify-TestnetWorkloadPods.jq", "jq -e -f \"${pod_filter}\"",
		"kubernetes.io/dockerconfigjson", "workload-registry-secrets-absent",
		"workload installation refused: partial managed state", "fresh-install-rollback-started",
	} {
		if !strings.Contains(raw, required) {
			t.Errorf("Tokyo workload installer is missing fail-closed proof %q", required)
		}
	}
}

func TestTokyoWorkloadPodVerifierMatchesStatusesByContainerName(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is unavailable")
	}
	filter := filepath.Join(filepath.Dir(moduleRoot(t)), "tools", "Verify-TestnetWorkloadPods.jq")
	const digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	image := func(name, digest string) string {
		return "012619468098.dkr.ecr.ap-northeast-1.amazonaws.com/" + name + "@" + digest
	}
	fixture := func(runtimeDigest string, omitMainStatus bool) []byte {
		pods := make([]any, 0, 9)
		for i := 0; i < 9; i++ {
			statuses := []any{}
			if !omitMainStatus {
				statuses = append(statuses, map[string]any{
					"name": "app", "ready": true, "imageID": image("service", runtimeDigest),
				})
			}
			pods = append(pods, map[string]any{
				"spec": map[string]any{
					"initContainers": []any{map[string]any{"name": "migrate", "image": image("kanz-migrate", digestA)}},
					"containers":     []any{map[string]any{"name": "app", "image": image("service", digestB)}},
				},
				"status": map[string]any{
					"phase": "Running",
					"initContainerStatuses": []any{map[string]any{
						"name": "migrate", "imageID": image("kanz-migrate", digestA),
						"state": map[string]any{"terminated": map[string]any{"exitCode": 0}},
					}},
					"containerStatuses": statuses,
				},
			})
		}
		body, marshalErr := json.Marshal(map[string]any{"items": pods})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return body
	}
	run := func(body []byte) error {
		cmd := exec.Command(jq, "-e", "-f", filter)
		cmd.Stdin = bytes.NewReader(body)
		return cmd.Run()
	}
	if err := run(fixture(digestB, false)); err != nil {
		t.Fatalf("valid reviewed and observed digests were rejected: %v", err)
	}
	if err := run(fixture(digestA, false)); err == nil {
		t.Fatal("a runtime digest different from the reviewed spec was accepted")
	}
	if err := run(fixture(digestB, true)); err == nil {
		t.Fatal("a Pod missing its main container status was accepted")
	}
}

func TestTokyoOmsGoLiveVerifierRejectsEachUnarmedControl(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is unavailable")
	}
	filter := filepath.Join(filepath.Dir(moduleRoot(t)), "tools", "Verify-TestnetOmsGoLive.jq")
	env := map[string]string{
		"OMS_REQUIRE_MANDATE":           "true",
		"OMS_REQUIRE_VENUE_ACCOUNT":     "true",
		"OMS_REQUIRE_VERIFIED_ACCOUNT":  "true",
		"OMS_REQUIRE_DUAL_CONTROL":      "true",
		"OMS_DUAL_CONTROL_MIN_NOTIONAL": "1 USD",
		"OMS_VENUE_ACCOUNTS":            "tenant/portfolio@XBIN=binance-main,tenant/portfolio@XOKX=okx-sub-1",
	}
	fixture := func(values map[string]string, logs string, portfolios, mandates int, unverified, uncovered, marginCurrent float64) []byte {
		vars := make([]any, 0, len(values))
		for name, value := range values {
			vars = append(vars, map[string]any{"name": name, "value": value})
		}
		body, marshalErr := json.Marshal(map[string]any{
			"deployment":      map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "oms", "env": vars}}}}}},
			"pods":            map[string]any{"items": []any{map[string]any{"status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": "oms", "ready": true}}}}}},
			"recent_logs":     logs,
			"portfolio_count": portfolios,
			"mandate_count":   mandates,
			"oms_metrics": fmt.Sprintf("kanz_oms_unverified_venue_account_total %g\n"+
				"kanz_oms_venue_margin_uncovered_total %g\n"+
				"kanz_oms_venue_margin_accounts_current %g\n", unverified, uncovered, marginCurrent),
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return body
	}
	run := func(body []byte) error {
		cmd := exec.Command(jq, "-c", "-f", filter)
		cmd.Stdin = bytes.NewReader(body)
		output, runErr := cmd.Output()
		if runErr != nil {
			return runErr
		}
		var result struct {
			Verdict string `json:"verdict"`
		}
		if err := json.Unmarshal(output, &result); err != nil {
			return err
		}
		if result.Verdict != "PASS" {
			return fmt.Errorf("verdict = %s", result.Verdict)
		}
		return nil
	}
	if err := run(fixture(env, "healthy", 1, 1, 0, 0, 1)); err != nil {
		t.Fatalf("armed, Ready OMS was rejected: %v", err)
	}
	for _, name := range []string{"OMS_REQUIRE_MANDATE", "OMS_REQUIRE_VENUE_ACCOUNT", "OMS_REQUIRE_VERIFIED_ACCOUNT", "OMS_REQUIRE_DUAL_CONTROL"} {
		mutated := make(map[string]string, len(env))
		for key, value := range env {
			mutated[key] = value
		}
		mutated[name] = "false"
		if err := run(fixture(mutated, "healthy", 1, 1, 0, 0, 1)); err == nil {
			t.Errorf("%s=false passed the go-live verifier", name)
		}
	}
	for _, name := range []string{"OMS_DUAL_CONTROL_MIN_NOTIONAL", "OMS_VENUE_ACCOUNTS"} {
		mutated := make(map[string]string, len(env))
		for key, value := range env {
			mutated[key] = value
		}
		mutated[name] = ""
		if err := run(fixture(mutated, "healthy", 1, 1, 0, 0, 1)); err == nil {
			t.Errorf("empty %s passed the go-live verifier", name)
		}
	}
	for _, warning := range []string{
		"venue adapter registered with an UNVERIFIED account",
		"the exchange did not report part of this account's margin state",
	} {
		if err := run(fixture(env, warning, 1, 1, 0, 0, 1)); err == nil {
			t.Errorf("unsafe recent OMS posture %q passed the go-live verifier", warning)
		}
	}
	for _, tc := range []struct {
		name                           string
		portfolios, mandates           int
		unverified, uncovered, current float64
	}{
		{name: "no portfolios", portfolios: 0, mandates: 0, current: 1},
		{name: "missing mandate", portfolios: 1, mandates: 0, current: 1},
		{name: "unverified account", portfolios: 1, mandates: 1, unverified: 1, current: 1},
		{name: "unobserved OKX margin", portfolios: 1, mandates: 1},
		{name: "incomplete OKX margin", portfolios: 1, mandates: 1, uncovered: 1, current: 1},
	} {
		if err := run(fixture(env, "healthy", tc.portfolios, tc.mandates, tc.unverified, tc.uncovered, tc.current)); err == nil {
			t.Errorf("%s passed the go-live verifier", tc.name)
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
