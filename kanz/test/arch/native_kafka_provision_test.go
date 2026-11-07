package arch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestNativeKafkaOffboardRefusesBeforeAnyLifecycleStep(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal("bash required for existing tenantctl contract")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bash, "infra/tenancy/tenantctl.sh", "offboard")
	cmd.Dir = moduleRoot(t)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TENANT=native-probe", "TENANTCTL_KAFKA_MODE=native", "KUBECONFIG=" + filepath.Join(t.TempDir(), "never-created")}
	out, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 || !strings.Contains(string(out), "native Kafka offboard is unsupported; no automatic legacy fallback") || strings.Contains(string(out), ">> [") {
		t.Fatal("native offboard did not refuse before lifecycle mutation")
	}
}

func TestNativeKafkaTenantctlClusterGrammarBeforeEffects(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal("bash required")
	}
	for _, tc := range []struct {
		id    string
		valid bool
	}{
		{"cluster-a", true}, {"redpanda.safe_custom-1", true}, {"", false},
		{"redpanda.initializing", false}, {"redpanda.unknown", false}, {"redpanda.x.y", false}, {"redpanda.$(id)", false},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, bash, "infra/tenancy/tenantctl.sh", "onboard")
		cmd.Dir = moduleRoot(t)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TENANT=native-probe", "TENANTCTL_KAFKA_MODE=native", "NATIVE_KAFKA_CLUSTER_ID=" + tc.id, "KUBECONFIG=" + filepath.Join(t.TempDir(), "never-created")}
		out, runErr := cmd.CombinedOutput()
		cancel()
		if runErr == nil || strings.Contains(string(out), ">> [") || strings.Contains(string(out), "unbound variable") || strings.Contains(string(out), "native exact expected Kafka ClusterID required") == tc.valid {
			t.Fatal("tenantctl cluster preflight did not discriminate before effects")
		}
	}
}

func TestNativeKafkaPrincipalEncodingRefusesBeforeEffects(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal("bash required")
	}
	for _, tc := range []struct {
		encoding, peer string
		refused        bool
	}{
		{"spiffe-uri", "kafka", false}, {"redpanda-rule-escaped-uri", "redpanda", false},
		{"redpanda-rule-escaped-uri", "kafka", true}, {"unknown", "redpanda", true},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, bash, "infra/tenancy/tenantctl.sh", "onboard")
		cmd.Dir = moduleRoot(t)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TENANT=native-probe", "TENANTCTL_KAFKA_MODE=native",
			"NATIVE_KAFKA_PRINCIPAL_ENCODING=" + tc.encoding, "NATIVE_KAFKA_BROKER_SPIFFE_ID=spiffe://kanz.internal/ns/kanz-messaging/sa/" + tc.peer,
			"KUBECONFIG=" + filepath.Join(t.TempDir(), "never-created")}
		out, runErr := cmd.CombinedOutput()
		cancel()
		if runErr == nil || strings.Contains(string(out), ">> [") || strings.Contains(string(out), "native principal encoding") != tc.refused {
			t.Fatal("encoding preflight did not discriminate before effects")
		}
	}
}

func checkNativeKafkaOwner(files map[string]string) bool {
	main := files["main"]
	engine := files["engine"]
	shell := files["shell"]
	for _, s := range []string{"brokerIdentity(*peer)", "transport.NewSource(ctx, *socket)", "transport.ClientTLSConfig(pinned, transport.AuthorizeServices(peerID))", "pinned.GetX509SVID()", "defer source.Close()", "defer tr.CloseIdleConnections()", "context.WithTimeout(parent, *work)", "provision.ReadManifest(*manifest)", "provision.NewPlan(raw, *digest, *tenant, *principal, *clusterID)"} {
		if strings.Count(main, s) != 1 {
			return false
		}
	}
	if strings.Contains(main, "AuthorizeMesh") || strings.Contains(main, "InsecureSkipVerify") {
		return false
	}
	encoding := strings.Index(main, "provision.WithPrincipalEncoding(p, *encoding, *peer)")
	durability := strings.Index(main, "provision.WithDurabilityProfile(p, *durability, *peer)")
	if encoding < 0 || encoding > strings.Index(main, "transport.NewSource(ctx, *socket)") ||
		durability < 0 || durability > strings.Index(main, "transport.NewSource(ctx, *socket)") ||
		!strings.Contains(engine, "PrincipalFilter: p.BrokerPrincipal") ||
		!strings.Contains(shell, "-durability-profile=${NATIVE_KAFKA_DURABILITY_PROFILE}") ||
		!strings.Contains(shell, "-principal-encoding=${NATIVE_KAFKA_PRINCIPAL_ENCODING}") {
		return false
	}
	if !strings.Contains(engine, `m.ClusterID != clusterID || m.ClusterID == ""`) {
		return false
	}
	for _, s := range []string{"AlterConfigs", "DeleteTopics", "DeleteACLs", "CreatePartitions"} {
		if strings.Contains(engine, s) {
			return false
		}
	}
	for _, s := range []string{"native Kafka offboard is unsupported; no automatic legacy fallback", "--ignore-not-found -o json", ".immutable==true", ".data=={\"topics-job.yaml\":$manifest}", "generateName: kafka-native-", "-manifest-sha256=${NATIVE_KAFKA_MANIFEST_SHA256}", "-principal=User:${prin}", "-broker-spiffe-id=${NATIVE_KAFKA_BROKER_SPIFFE_ID}", "-expected-cluster-id=${NATIVE_KAFKA_CLUSTER_ID}", "backoffLimit: 0", "activeDeadlineSeconds: 270"} {
		if !strings.Contains(shell, s) {
			return false
		}
	}
	start := strings.Index(shell, "native_kafka_tenant_job()")
	end := strings.Index(shell, "# Both principal shapes")
	if start < 0 || end <= start || strings.Contains(shell[start:end], "kubectl apply") || strings.Contains(shell[start:end], "kubectl delete") {
		return false
	}
	return true
}

func TestNativeKafkaSourceBoundaryAndMutations(t *testing.T) {
	root := moduleRoot(t)
	files := map[string]string{}
	for key, path := range map[string]string{"main": "cmd/kanz-kafka-provision/main.go", "engine": "cmd/kanz-kafka-provision/internal/provision/engine.go", "shell": "infra/tenancy/tenantctl.sh"} {
		b, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		files[key] = string(b)
	}
	if !checkNativeKafkaOwner(files) {
		t.Fatal("native owner boundary changed")
	}
	for _, m := range []struct{ key, old, new string }{
		{"main", "provision.WithDurabilityProfile(p, *durability, *peer)", "provision.WithDurabilityProfile(p, \"redpanda-raft\", *peer)"},
		{"main", "provision.WithPrincipalEncoding(p, *encoding, *peer)", "provision.WithPrincipalEncoding(p, \"spiffe-uri\", *peer)"},
		{"engine", "PrincipalFilter: p.BrokerPrincipal", "PrincipalFilter: p.Principal"},
		{"main", "transport.AuthorizeServices(peerID)", "transport.AuthorizeMesh()"},
		{"main", "brokerIdentity(*peer)", "spiffeid.FromString(*peer)"},
		{"main", "transport.ClientTLSConfig(pinned,", "transport.ClientTLSConfig(source,"},
		{"main", "provision.NewPlan(raw, *digest, *tenant, *principal, *clusterID)", "unboundPlan()"},
		{"engine", `m.ClusterID != clusterID || m.ClusterID == ""`, `false`},
		{"shell", "-expected-cluster-id=${NATIVE_KAFKA_CLUSTER_ID}", "-expected-cluster-id=guessed"},
		{"shell", ".immutable==true", ".immutable!=null"},
		{"shell", "-manifest-sha256=${NATIVE_KAFKA_MANIFEST_SHA256}", "-manifest-sha256=ignored"},
		{"shell", "generateName: kafka-native-", "name: shared-job"},
		{"shell", "native Kafka offboard is unsupported; no automatic legacy fallback", "offboard fallback accepted"},
	} {
		copy := map[string]string{}
		for k, v := range files {
			copy[k] = v
		}
		if !strings.Contains(copy[m.key], m.old) {
			t.Fatal("mutation anchor absent")
		}
		copy[m.key] = strings.Replace(copy[m.key], m.old, m.new, 1)
		if checkNativeKafkaOwner(copy) {
			t.Fatal("mutation escaped native source guard")
		}
	}
}

const nativeFullCondition = "${{ !(github.event_name == 'workflow_dispatch' && inputs.mode == 'image-diagnostics' && startsWith(github.ref, 'refs/heads/') && github.ref != 'refs/heads/main') }}"
const nativeDiagnosticCondition = "${{ github.event_name == 'workflow_dispatch' && inputs.mode == 'image-diagnostics' && startsWith(github.ref, 'refs/heads/') && github.ref != 'refs/heads/main' }}"

func nativeKafkaQualificationBoundary(raw string) bool {
	var w struct {
		On struct {
			Dispatch struct {
				Inputs map[string]struct {
					Type    string   `yaml:"type"`
					Default string   `yaml:"default"`
					Options []string `yaml:"options"`
				} `yaml:"inputs"`
			} `yaml:"workflow_dispatch"`
		} `yaml:"on"`
		Jobs map[string]struct {
			If       any `yaml:"if"`
			Continue any `yaml:"continue-on-error"`
			Steps    []struct {
				Name     string `yaml:"name"`
				Run      string `yaml:"run"`
				If       any    `yaml:"if"`
				Continue any    `yaml:"continue-on-error"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if yaml.Unmarshal([]byte(raw), &w) != nil {
		return false
	}
	job, ok := w.Jobs["native-topic-provisioning"]
	if !ok || job.If != nativeFullCondition || job.Continue != nil {
		return false
	}
	original, ok := w.Jobs["developmental-redpanda-identity"]
	diagnostic, diagnosticOK := w.Jobs["native-image-diagnostics"]
	mode := w.On.Dispatch.Inputs["mode"]
	if !ok || original.If != nativeFullCondition || original.Continue != nil || !diagnosticOK || diagnostic.If != nativeDiagnosticCondition || diagnostic.Continue != nil || mode.Type != "choice" || mode.Default != "full" || strings.Join(mode.Options, ",") != "full,image-diagnostics" {
		return false
	}
	diagnosticRuns := 0
	for _, step := range diagnostic.Steps {
		if step.Run == "python3 -B tools/native-kafka-provision/image_diagnostic.py" {
			if step.If != nil || step.Continue != nil {
				return false
			}
			diagnosticRuns++
		}
	}
	if diagnosticRuns != 1 {
		return false
	}
	seen, cleanup := 0, 0
	for _, step := range job.Steps {
		if step.Run == "python3 -B tools/native-kafka-provision/qualify.py" {
			if step.If != nil || step.Continue != nil {
				return false
			}
			seen++
		}
		if step.Run == "python3 -B tools/native-kafka-provision/owned_cleanup.py" {
			if step.If != "always()" || step.Continue != nil {
				return false
			}
			cleanup++
		}
	}
	return seen == 1 && cleanup == 1
}

func TestNativeKafkaQualificationCannotBeOptional(t *testing.T) {
	raw, e := os.ReadFile(filepath.Join(filepath.Dir(moduleRoot(t)), ".github/workflows/redpanda-identity-proof.yml"))
	if e != nil {
		t.Fatal(e)
	}
	source := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !nativeKafkaQualificationBoundary(source) {
		t.Fatal("native proof wiring missing")
	}
	for _, mutation := range []struct{ old, new string }{
		{nativeFullCondition, "false"},
		{nativeDiagnosticCondition, "true"},
		{"default: full", "default: image-diagnostics"},
		{"github.event_name == 'workflow_dispatch'", "github.event_name == 'pull_request'"},
		{"github.ref != 'refs/heads/main'", "true"},
		{"run: python3 -B tools/native-kafka-provision/image_diagnostic.py", "run: echo skipped"},
		{"  native-topic-provisioning:\n", "  native-topic-provisioning:\n    continue-on-error: true\n"},
		{"run: python3 -B tools/native-kafka-provision/qualify.py", "if: false\n        run: python3 -B tools/native-kafka-provision/qualify.py"},
		{"run: python3 -B tools/native-kafka-provision/qualify.py", "run: echo skipped"},
		{"run: python3 -B tools/native-kafka-provision/owned_cleanup.py", "run: echo skipped"},
	} {
		if !strings.Contains(source, mutation.old) {
			t.Fatal("mutation anchor missing")
		}
		if nativeKafkaQualificationBoundary(strings.Replace(source, mutation.old, mutation.new, 1)) {
			t.Fatal("optional proof escaped guard")
		}
	}
}
