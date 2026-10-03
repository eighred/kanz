package arch

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Serial package execution does not serialize tests against the background OMS.
// Its outbox relay must never drain fixture facts, and test migrations must never
// replace its tables (#1316). Keep its entire database outside fixture teardown.
func TestCILiveOMSDatabaseIsSeparateFromFixtures(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(moduleRoot(t)), ".github/workflows/kanz-ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Env map[string]string `yaml:"env"`
				Run string            `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	database := func(name, value string) string {
		t.Helper()
		u, err := url.Parse(value)
		if err != nil || u == nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" || u.Path == "" || u.Path == "/" {
			t.Fatalf("%s must declare a concrete PostgreSQL database", name)
		}
		port := u.Port()
		if port == "" {
			port = "5432"
		}
		return net.JoinHostPort(strings.ToLower(u.Hostname()), port) + u.Path
	}
	var live string
	var fixtures []string
	for _, step := range workflow.Jobs["go"].Steps {
		if value := step.Env["TEST_POSTGRES_URL"]; value != "" {
			fixtures = append(fixtures, database("TEST_POSTGRES_URL", value))
		}
		if value := step.Env["OMS_DATABASE_URL"]; value != "" {
			if live != "" {
				t.Fatal("multiple live OMS database declarations require explicit isolation coverage")
			}
			live = database("OMS_DATABASE_URL", value)
			if database("KANZ_MIGRATE_DATABASE_URL", step.Env["KANZ_MIGRATE_DATABASE_URL"]) != live {
				t.Fatal("CI must migrate the same database its live OMS opens")
			}
		}
		for _, line := range strings.Split(step.Run, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "#") && strings.Contains(line, "OMS_DATABASE_URL=") {
				t.Fatal("declare the live OMS database in step.env so the isolation check sees its effective value")
			}
		}
	}
	if live == "" || len(fixtures) == 0 {
		t.Fatal("isolation check found no live OMS or fixture database")
	}
	for _, fixture := range fixtures {
		if live == fixture {
			t.Fatal("live OMS shares a fixture database: its relay can consume test facts and teardown can destroy its tables")
		}
	}
}
