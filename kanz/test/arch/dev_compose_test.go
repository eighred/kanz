package arch

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The smoke stack must exercise the schema and role used by the service,
// without exposing the public fixture credentials outside the local host.
func checkDevCompose(raw, initSQL string) error {
	var doc struct {
		Services map[string]struct {
			Build struct {
				Context    string `yaml:"context"`
				Dockerfile string `yaml:"dockerfile"`
			} `yaml:"build"`
			Ports       []string          `yaml:"ports"`
			Environment map[string]string `yaml:"environment"`
			Command     []string          `yaml:"command"`
			Volumes     []string          `yaml:"volumes"`
			DependsOn   map[string]struct {
				Condition string `yaml:"condition"`
			} `yaml:"depends_on"`
		} `yaml:"services"`
		Volumes map[string]map[string]any `yaml:"volumes"`
	}
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return err
	}
	for name, svc := range doc.Services {
		for _, port := range svc.Ports {
			if !strings.HasPrefix(port, "127.0.0.1:") {
				return fmt.Errorf("%s exposes public dev credentials outside loopback", name)
			}
		}
	}
	for service, dockerfile := range map[string]string{
		"risk-migrate": "kanz/cmd/kanz_migrate/Dockerfile",
		"risk-engine":  "kanz/services/risk_engine/Dockerfile",
		"api-gateway":  "kanz/services/api_gateway/Dockerfile",
	} {
		build := doc.Services[service].Build
		if build.Context != "../.." || build.Dockerfile != dockerfile {
			return fmt.Errorf("%s must build its canonical Dockerfile from the repository root", service)
		}
	}
	if !reflect.DeepEqual(doc.Services["risk-migrate"].Command, []string{"--dir", "/migrations/risk_engine"}) {
		return fmt.Errorf("migration directory must match the bundled physical service name")
	}
	if len(doc.Volumes) != 3 {
		return fmt.Errorf("all three project-owned named volumes must be declared")
	}
	for _, name := range []string{"nats-data", "postgres-data", "redpanda-data"} {
		options, exists := doc.Volumes[name]
		if !exists || len(options) != 0 {
			return fmt.Errorf("%s must be a project-owned named volume without external overrides", name)
		}
	}
	for service, mounts := range map[string][]string{
		"nats":     {"nats-data:/tmp/nats"},
		"kafka":    {"redpanda-data:/var/lib/redpanda/data"},
		"postgres": {"postgres-data:/var/lib/postgresql/data", "./init-postgres.sql:/docker-entrypoint-initdb.d/kanz.sql:ro"},
	} {
		if !reflect.DeepEqual(doc.Services[service].Volumes, mounts) {
			return fmt.Errorf("%s must mount its intended named storage and initialization inputs", service)
		}
	}
	for _, edge := range [][3]string{
		{"risk-engine", "risk-migrate", "service_completed_successfully"},
		{"risk-engine", "nats-init", "service_completed_successfully"},
		{"risk-migrate", "postgres", "service_healthy"},
	} {
		if doc.Services[edge[0]].DependsOn[edge[1]].Condition != edge[2] {
			return fmt.Errorf("%s must wait for %s: %s", edge[0], edge[1], edge[2])
		}
	}
	const dsn = "postgres://kanzapp:kanz@postgres:5432/kanz_risk_dev?sslmode=disable"
	if doc.Services["risk-engine"].Environment["RISK_ENGINE_DATABASE_URL"] != dsn ||
		doc.Services["risk-migrate"].Environment["KANZ_MIGRATE_DATABASE_URL"] != dsn {
		return fmt.Errorf("migration and runtime must use the restricted service database, separate from test fixtures")
	}
	if !strings.Contains(initSQL, "NOSUPERUSER NOBYPASSRLS") ||
		!strings.Contains(initSQL, "CREATE DATABASE kanz_risk_dev OWNER kanzapp") ||
		!strings.Contains(initSQL, "CREATE DATABASE kanzapp OWNER kanzapp") {
		return fmt.Errorf("restricted role and separate databases must be bootstrapped")
	}
	bootstrap := doc.Services["nats-init"]
	if !reflect.DeepEqual(bootstrap.Volumes, []string{"../infra/nats/bootstrap-job.yaml:/bootstrap.yaml:ro"}) ||
		!strings.Contains(strings.Join(bootstrap.Command, "\n"), "exec sh /tmp/bootstrap.sh") {
		return fmt.Errorf("NATS must run the canonical stream bootstrap")
	}
	return nil
}

func TestDevComposeStartupAndLocalAuthority(t *testing.T) {
	root := moduleRoot(t)
	raw := strings.ReplaceAll(readFile(t, filepath.Join(root, "dev", "docker-compose.yml")), "\r\n", "\n")
	sql := readFile(t, filepath.Join(root, "dev", "init-postgres.sql"))
	if err := checkDevCompose(raw, sql); err != nil {
		t.Fatal(err)
	}
	for name, mutated := range map[string]string{
		"public-port":          strings.Replace(raw, "127.0.0.1:5432:5432", "5432:5432", 1),
		"superuser":            strings.ReplaceAll(raw, "postgres://kanzapp:kanz", "postgres://kanz:kanz"),
		"test-database":        strings.ReplaceAll(raw, "/kanz_risk_dev?", "/kanzapp?"),
		"unfinished-migration": strings.ReplaceAll(raw, "condition: service_completed_successfully", "condition: service_started"),
		"missing-bootstrap":    strings.Replace(raw, "exec sh /tmp/bootstrap.sh", "true", 1),
		"foreign-bootstrap":    strings.Replace(raw, "../infra/nats/bootstrap-job.yaml:/bootstrap.yaml:ro", "../infra/nats/bootstrap-job-dev-plaintext.yaml:/bootstrap.yaml:ro", 1),
		"writable-bootstrap":   strings.Replace(raw, "../infra/nats/bootstrap-job.yaml:/bootstrap.yaml:ro", "../infra/nats/bootstrap-job.yaml:/bootstrap.yaml", 1),
		"old-migrator-source":  strings.Replace(raw, "kanz/cmd/kanz_migrate/Dockerfile", "kanz/cmd/kanz-migrate/Dockerfile", 1),
		"old-risk-source":      strings.Replace(raw, "kanz/services/risk_engine/Dockerfile", "kanz/services/risk-engine/Dockerfile", 1),
		"old-gateway-source":   strings.Replace(raw, "kanz/services/api_gateway/Dockerfile", "kanz/services/api-gateway/Dockerfile", 1),
		"wrong-build-context":  strings.Replace(raw, "context: ../..", "context: ..", 1),
		"old-migration-dir":    strings.Replace(raw, "/migrations/risk_engine", "/migrations/risk-engine", 1),
		"missing-nats-volume":  strings.Replace(raw, "      - nats-data:/tmp/nats\n", "", 1),
		"wrong-postgres-path":  strings.Replace(raw, "postgres-data:/var/lib/postgresql/data", "postgres-data:/unused", 1),
		"wrong-kafka-volume":   strings.Replace(raw, "redpanda-data:/var/lib/redpanda/data", "foreign-data:/var/lib/redpanda/data", 1),
		"writable-init-sql":    strings.Replace(raw, "./init-postgres.sql:/docker-entrypoint-initdb.d/kanz.sql:ro", "./init-postgres.sql:/docker-entrypoint-initdb.d/kanz.sql", 1),
		"missing-volume-root":  strings.Replace(raw, "  postgres-data:\n", "", 1),
		"external-volume":      strings.Replace(raw, "  nats-data:\n", "  nats-data:\n    external: true\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if mutated == raw || checkDevCompose(mutated, sql) == nil {
				t.Fatal("unsafe Compose mutation accepted")
			}
		})
	}
	if checkDevCompose(raw, strings.ReplaceAll(sql, "NOSUPERUSER NOBYPASSRLS", "SUPERUSER")) == nil {
		t.Fatal("privileged fixture role accepted")
	}
}

// This checks the reviewed wrapper's direct delegation, not arbitrary shell
// semantics or actual process/volume cleanup. Make remains the implementation.
func checkDevStarter(source string) error {
	for _, command := range []string{"make up", "make down", "make reset CONFIRM=\"${CONFIRM:-}\""} {
		if !strings.Contains(source, "\n  "+command+"\n") {
			return fmt.Errorf("starter must delegate to the canonical Make lifecycle")
		}
	}
	for _, duplicate := range []string{"compose up", "compose down", "go run ./test/load/seed", "ensure_sdk()", "|| true"} {
		if strings.Contains(source, duplicate) {
			return fmt.Errorf("starter must not bypass canonical generation, startup or reset failures")
		}
	}
	return nil
}

func TestDevStarterDelegatesTheCanonicalLifecycle(t *testing.T) {
	source := strings.ReplaceAll(readFile(t, filepath.Join(moduleRoot(t), "start.sh")), "\r\n", "\n")
	if err := checkDevStarter(source); err != nil {
		t.Fatal(err)
	}
	for name, mutated := range map[string]string{
		"duplicate-startup": strings.Replace(source, "\n  make up\n", "\n  compose up -d --build\n", 1),
		"destructive-down":  strings.Replace(source, "\n  make down\n", "\n  compose down -v\n", 1),
		"unguarded-reset":   strings.Replace(source, "make reset CONFIRM=\"${CONFIRM:-}\"", "make reset CONFIRM=kanz-dev", 1),
		"swallowed-failure": strings.Replace(source, "\n  make up\n", "\n  make up || true\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if mutated == source || checkDevStarter(mutated) == nil {
				t.Fatal("unsafe starter mutation accepted")
			}
		})
	}
}
