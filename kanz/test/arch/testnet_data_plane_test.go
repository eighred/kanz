package arch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type testnetDataCluster struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name   string            `yaml:"name"`
		Labels map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
	Spec struct {
		Instances             int    `yaml:"instances"`
		ImageName             string `yaml:"imageName"`
		EnableSuperuserAccess bool   `yaml:"enableSuperuserAccess"`
		Postgresql            struct {
			Parameters map[string]string `yaml:"parameters"`
			PGHBA      []string          `json:"pg_hba" yaml:"pg_hba"`
		} `yaml:"postgresql"`
		Resources struct {
			Requests map[string]string `yaml:"requests"`
			Limits   map[string]string `yaml:"limits"`
		} `yaml:"resources"`
		Storage    map[string]string `yaml:"storage"`
		WALStorage map[string]string `yaml:"walStorage"`
	} `yaml:"spec"`
}

func TestTokyoDataPlaneCannotClaimHighAvailability(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, "infra", "overlays", "testnet-tokyo", "data")
	clusterRaw, err := os.ReadFile(filepath.Join(dir, "postgres.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cluster testnetDataCluster
	if err := yaml.Unmarshal(clusterRaw[:bytesBeforeNextDocument(clusterRaw)], &cluster); err != nil {
		t.Fatal(err)
	}
	if cluster.Kind != "Cluster" || cluster.Metadata.Name != "kanz-testnet-postgres" {
		t.Fatalf("unexpected testnet Postgres resource: kind=%q name=%q", cluster.Kind, cluster.Metadata.Name)
	}
	if cluster.Spec.Instances != 1 || cluster.Metadata.Labels["kanz.io/ha"] != "UNSUPPORTED" {
		t.Fatalf("single-node testnet must declare HA UNSUPPORTED: instances=%d label=%q", cluster.Spec.Instances, cluster.Metadata.Labels["kanz.io/ha"])
	}
	if cluster.Spec.EnableSuperuserAccess {
		t.Fatal("CloudNativePG network superuser access must remain disabled")
	}
	if !regexp.MustCompile(`^ghcr\.io/cloudnative-pg/postgresql:16\.10-standard-bookworm@sha256:[0-9a-f]{64}$`).MatchString(cluster.Spec.ImageName) {
		t.Fatalf("Postgres image is not immutable: %q", cluster.Spec.ImageName)
	}
	if cluster.Spec.Storage["size"] != "20Gi" || cluster.Spec.WALStorage["size"] != "4Gi" {
		t.Fatalf("Postgres persistent envelope changed: data=%q wal=%q", cluster.Spec.Storage["size"], cluster.Spec.WALStorage["size"])
	}
	if cluster.Spec.Resources.Requests["memory"] != "1536Mi" || cluster.Spec.Resources.Limits["memory"] != "2Gi" {
		t.Fatalf("Postgres memory envelope changed: requests=%v limits=%v", cluster.Spec.Resources.Requests, cluster.Spec.Resources.Limits)
	}
	if cluster.Spec.Postgresql.Parameters["archive_timeout"] != "30s" || cluster.Spec.Postgresql.Parameters["password_encryption"] != "scram-sha-256" {
		t.Fatalf("Postgres durability/security parameters changed: %v", cluster.Spec.Postgresql.Parameters)
	}
	if len(cluster.Spec.Postgresql.PGHBA) != 1 || !strings.Contains(cluster.Spec.Postgresql.PGHBA[0], "hostssl") || !strings.Contains(cluster.Spec.Postgresql.PGHBA[0], "scram-sha-256") {
		t.Fatalf("Postgres application authentication is not TLS + SCRAM: %v", cluster.Spec.Postgresql.PGHBA)
	}

	kustomization := mustReadArchFile(t, filepath.Join(dir, "kustomization.yaml"))
	if !regexp.MustCompile(`(?s)name: nats.*path: /spec/replicas\s+value: 1`).Match(kustomization) || !strings.Contains(string(kustomization), "path: /spec/volumeClaimTemplates/0/spec/resources/requests/storage") || !strings.Contains(string(kustomization), "value: 4Gi") {
		t.Fatal("NATS testnet patch must retain one replica on a bounded 4Gi persistent volume")
	}
	natsConfig := mustReadArchFile(t, filepath.Join(dir, "nats-config-patch.yaml"))
	if strings.Contains(string(natsConfig), "cluster {") || !strings.Contains(string(natsConfig), "max_file_store: 3GB") {
		t.Fatal("single-node NATS must be non-clustered with a bounded file store")
	}
	if !strings.Contains(string(natsConfig), "sync_interval: always") {
		t.Fatal("single-node NATS must fsync every FACT before acknowledging it")
	}
	natsBootstrap := mustReadArchFile(t, filepath.Join(dir, "nats-bootstrap-patch.yaml"))
	if !regexp.MustCompile(`(?s)name: NATS_REPLICAS\s+value: "1"`).Match(natsBootstrap) {
		t.Fatal("NATS stream bootstrap must use replica factor one")
	}

	if !strings.Contains(string(kustomization), "memory: 64Mi") || !strings.Contains(string(kustomization), "memory: 256Mi") {
		t.Fatal("Redis requests and limits must remain bounded")
	}
	if !strings.Contains(string(kustomization), "--appendfsync always") {
		t.Fatal("single-node Redis must fsync each nonce mutation before acknowledging it")
	}
	if !strings.Contains(string(kustomization), "path: /spec/template/spec/serviceAccountName") || !strings.Contains(string(kustomization), "value: redis") {
		t.Fatal("Redis must use the dedicated ServiceAccount bound by its Vault role")
	}
	if !strings.Contains(string(clusterRaw), "cidr: 10.71.0.15/32") || !strings.Contains(string(clusterRaw), "port: 6443") {
		t.Fatal("CloudNativePG instance Pods cannot reach the post-DNAT Tokyo API endpoint")
	}
	for _, image := range []string{"ghcr.io/spiffe/spiffe-helper", "nats", "natsio/nats-box", "redis", "ghcr.io/cloudnative-pg/postgresql"} {
		pattern := regexp.MustCompile(`(?m)^  - name: ` + regexp.QuoteMeta(image) + `\n    newName: ` + regexp.QuoteMeta(image) + `\n(?:    newTag: [^\n]+\n)?    digest: sha256:[0-9a-f]{64}$`)
		if !pattern.Match(kustomization) {
			t.Errorf("testnet data-plane image %q is not pinned by digest", image)
		}
	}
	migrations := mustReadArchFile(t, filepath.Join(dir, "postgres-migrations.yaml"))
	if !regexp.MustCompile(`012619468098\.dkr\.ecr\.ap-northeast-1\.amazonaws\.com/kanz-migrate@sha256:[0-9a-f]{64}`).Match(migrations) {
		t.Fatal("migration image must come from the immutable Tokyo ECR release")
	}
}

func TestTokyoDatabaseMigrationsRunBeforeCapitalPathAdmission(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, "infra", "overlays", "testnet-tokyo", "data")
	raw := string(mustReadArchFile(t, filepath.Join(dir, "postgres-migrations.yaml")))
	for _, service := range []string{"accounting", "audit", "identity", "oms", "regulatory", "risk-engine", "venue-binance", "venue-okx"} {
		if !strings.Contains(raw, "name: migrate-"+service) || !strings.Contains(raw, `"/migrations/`+service+`"`) {
			t.Errorf("migration job does not apply %s's schema", service)
		}
		if !strings.Contains(raw, "value: /run/migration-dsns/"+service) {
			t.Errorf("migration job does not give %s a file-backed DSN", service)
		}
	}
	for _, required := range []string{
		"serviceAccountName: postgres-provisioner", "secretProviderClass: postgres-role-passwords",
		"defaultMode: 0440", "readOnlyRootFilesystem: true", "select count(*) from schema_migrations",
		"relrowsecurity and not relforcerowsecurity", "activeDeadlineSeconds: 900",
	} {
		if !strings.Contains(raw, required) {
			t.Errorf("migration job is missing fail-closed boundary %q", required)
		}
	}
	if strings.Contains(raw, "secretObjects:") || strings.Contains(raw, "kind: Secret") {
		t.Fatal("migration credentials must stay in Vault CSI files and memory-backed emptyDir")
	}
}

func TestTokyoPostgresProvisionerKeepsApplicationRolesRLSConstrained(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, "infra", "overlays", "testnet-tokyo", "data")
	raw := mustReadArchFile(t, filepath.Join(dir, "postgres-provisioner.yaml"))
	text := string(raw)
	secretProviders := string(mustReadArchFile(t, filepath.Join(dir, "secret-provider-classes.yaml")))
	if strings.Contains(text, "LOGIN SUPERUSER") || strings.Contains(text, "LOGIN BYPASSRLS") || strings.Contains(text, "secretObjects:") {
		t.Fatal("provisioner must not grant RLS bypass or sync Vault values into Kubernetes Secrets")
	}
	if !strings.Contains(text, "ALTER ROLE %I LOGIN NOCREATEDB NOCREATEROLE CONNECTION LIMIT") {
		t.Fatal("provisioner no longer constrains role creation and connection capabilities")
	}
	if !strings.Contains(text, "AND rolsuper = false") || !strings.Contains(text, "AND rolbypassrls = false") {
		t.Fatal("provisioner no longer fails closed when a role can bypass tenant RLS")
	}
	if strings.Contains(text, "ALTER ROLE %I LOGIN NOSUPERUSER") || strings.Contains(text, "NOREPLICATION NOBYPASSRLS CONNECTION") {
		t.Fatal("non-superuser bootstrap role cannot restate superuser-only role attributes")
	}
	if !strings.Contains(text, "GRANT %I TO kanz_bootstrap', :'migrate_role'") || strings.Contains(text, "TO kanz_bootstrap WITH ADMIN OPTION") {
		t.Fatal("bootstrap needs SET ROLE membership without role-delegation authority")
	}
	services := []string{"accounting", "audit", "identity", "oms", "regulatory", "risk-engine", "venue-binance", "venue-okx"}
	for _, service := range services {
		if !strings.Contains(text, "provision_database "+service+" ") {
			t.Errorf("migration-owning capital-path service %q has no logical database", service)
		}
		if !strings.Contains(secretProviders, "objectName: "+service+"-app") || !strings.Contains(secretProviders, "objectName: "+service+"-migrate") {
			t.Errorf("service %q does not have separate app and migration credentials", service)
		}
	}
	if !strings.Contains(text, "ALTER DEFAULT PRIVILEGES FOR ROLE") {
		t.Fatal("migration-owned objects will not become usable by the application role")
	}
	if !strings.Contains(text, "secretProviderClass: postgres-role-passwords") {
		t.Fatal("role passwords are not delivered through Vault CSI")
	}
	if !strings.Contains(text, "fsGroup: 26") || !strings.Contains(text, "defaultMode: 0440") {
		t.Fatal("non-root provisioner cannot read the group-restricted bootstrap credential")
	}
}

func TestCloudNativePGReleaseIsChecksumAndDigestLocked(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, "infra", "controllers", "cloudnative-pg")
	raw := mustReadArchFile(t, filepath.Join(dir, "release-lock.json"))
	var lock struct {
		Version         string `json:"version"`
		SourceCommit    string `json:"sourceCommit"`
		ManifestURL     string `json:"manifestURL"`
		ManifestSHA256  string `json:"manifestSHA256"`
		ControllerImage string `json:"controllerImage"`
	}
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(lock.Version) || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(lock.SourceCommit) {
		t.Fatalf("CloudNativePG version/source are not immutable: %+v", lock)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(lock.ManifestSHA256) || !regexp.MustCompile(`@sha256:[0-9a-f]{64}$`).MatchString(lock.ControllerImage) {
		t.Fatalf("CloudNativePG manifest/image are not immutable: %+v", lock)
	}
	if !strings.Contains(lock.ManifestURL, lock.Version) {
		t.Fatalf("manifest URL %q does not match version %q", lock.ManifestURL, lock.Version)
	}
}

func TestCloudNativePGControllerTargetsTheRealTokyoAPIServerEndpoint(t *testing.T) {
	root := moduleRoot(t)
	raw := string(mustReadArchFile(t, filepath.Join(root, "infra", "controllers", "cloudnative-pg", "network-policy.yaml")))
	if strings.Contains(raw, "cidr: 10.43.0.1/32") || !regexp.MustCompile(`(?s)cidr: 10\.71\.0\.15/32.*port: 6443`).MatchString(raw) {
		t.Fatal("CloudNativePG controller egress must target the post-DNAT Tokyo API endpoint 10.71.0.15:6443")
	}
}

func TestCloudNativePGInstallerVerifiesAvailabilityAndExactImage(t *testing.T) {
	root := moduleRoot(t)
	raw := string(mustReadArchFile(t, filepath.Join(filepath.Dir(root), "tools", "Install-TestnetCloudNativePG.ps1")))
	if !strings.Contains(raw, "wait --for=condition=Available deployment/cnpg-controller-manager") ||
		!strings.Contains(raw, "containers[?(@.name==") || !strings.Contains(raw, "].image}") ||
		!strings.Contains(raw, `= "${expected_image}"`) {
		t.Fatal("CloudNativePG installer must prove the controller is Available and running the locked image")
	}
	if strings.Contains(raw, "rollout status deployment/cnpg-controller-manager") {
		t.Fatal("CloudNativePG installer must not treat a historical ProgressDeadlineExceeded as current availability")
	}
}

func TestTokyoDataPlaneInstallerFailsClosedOnRenderAndRuntimeState(t *testing.T) {
	root := moduleRoot(t)
	raw := string(mustReadArchFile(t, filepath.Join(filepath.Dir(root), "tools", "Install-TestnetDataPlane.ps1")))
	for _, required := range []string{
		"resourceCount -ne 37", "mutable image tag survived", "sha256sum --check --status",
		"data-plane-server-dry-run-namespace-substitute=default", "apply --server-side --dry-run=server", "condition=Ready cluster/kanz-testnet-postgres",
		"condition=complete job/postgres-provisioner", "condition=complete job/postgres-migrations", "condition=complete job/nats-bootstrap",
		"redis_sa", "not rolsuper and not rolbypassrls", "CONFIG GET appendfsync", "sync_interval: always",
	} {
		if !strings.Contains(raw, required) {
			t.Errorf("Tokyo data-plane installer is missing fail-closed proof %q", required)
		}
	}
}

func TestTokyoRecoveryKeepsCredentialsInPodsAndProvesAnIsolatedRestore(t *testing.T) {
	root := moduleRoot(t)
	script := string(mustReadArchFile(t, filepath.Join(filepath.Dir(root), "tools", "testnet-data-plane-recovery.sh")))
	wrapper := string(mustReadArchFile(t, filepath.Join(filepath.Dir(root), "tools", "Invoke-TestnetDataPlaneRecovery.ps1")))
	for _, required := range []string{
		"refuse_active_writers", "postgres-migrations", "pg_dump", "redis-cli -a", "account backup --check",
		"MANIFEST.sha256", "server-side-encryption aws:kms", "ObjectLockMode==\"GOVERNANCE\"",
		"kind: Cluster", "name: kanz-postgres-drill", "kind: NetworkPolicy",
		"pg_restore", "relrowsecurity and not relforcerowsecurity", "account restore --force",
		"status:\"RESTORE_VERIFIED\"", "rpo_seconds", "rto_seconds",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("data-plane recovery is missing fail-closed proof %q", required)
		}
	}
	if strings.Contains(script, "aws_access_key_id") || strings.Contains(script, "secretObjects:") {
		t.Fatal("recovery must not move AWS or Vault credentials into source or Kubernetes Secrets")
	}
	for _, required := range []string{"fetch origin main", "hash-object", "origin/main", "not the exact blob merged"} {
		if !strings.Contains(wrapper, required) {
			t.Errorf("recovery wrapper does not enforce merged-code provenance %q", required)
		}
	}
}

func mustReadArchFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func bytesBeforeNextDocument(raw []byte) int {
	if i := strings.Index(string(raw), "\n---\n"); i >= 0 {
		return i
	}
	return len(raw)
}
