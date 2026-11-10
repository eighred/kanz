package arch

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The smoke stack must exercise the schema and role used by the service,
// without exposing the public fixture credentials outside the local host.
func checkDevCompose(raw, initSQL string) error {
	var doc struct {
		Services map[string]struct {
			Ports       []string          `yaml:"ports"`
			Environment map[string]string `yaml:"environment"`
			Command     []string          `yaml:"command"`
			Volumes     []string          `yaml:"volumes"`
			DependsOn   map[string]struct {
				Condition string `yaml:"condition"`
			} `yaml:"depends_on"`
		} `yaml:"services"`
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
	if !strings.Contains(strings.Join(bootstrap.Volumes, "\n"), "../infra/nats/bootstrap-job.yaml:/bootstrap.yaml:ro") ||
		!strings.Contains(strings.Join(bootstrap.Command, "\n"), "exec sh /tmp/bootstrap.sh") {
		return fmt.Errorf("NATS must run the canonical stream bootstrap")
	}
	return nil
}

func TestDevComposeStartupAndLocalAuthority(t *testing.T) {
	root := moduleRoot(t)
	raw := readFile(t, filepath.Join(root, "dev", "docker-compose.yml"))
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
