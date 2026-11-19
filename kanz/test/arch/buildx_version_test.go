package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The client supplies the registry token callback to the BuildKit daemon.
// Pin it in each image job so a runner update cannot change the retry budget.
func TestBuildxReadinessFixIsPinned(t *testing.T) {
	for name, requiredJobs := range map[string][]string{
		"build.yml":                  {"image", "publish", "go-cache-qualification"},
		"release.yml":                {"release"},
		"architecture-migration.yml": {"compatibility", "finite-snapshot-owner"},
		"base-image-mirror.yml":      {"rust-audit-tools"},
	} {
		data, err := os.ReadFile(filepath.Join(filepath.Dir(moduleRoot(t)), ".github", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		var workflow struct {
			Jobs map[string]struct {
				Steps []struct {
					Uses string
					With map[string]string
				}
			}
		}
		if err := yaml.Unmarshal(data, &workflow); err != nil {
			t.Fatal(err)
		}
		seen := make(map[string]bool)
		for jobID, job := range workflow.Jobs {
			for _, step := range job.Steps {
				if strings.HasPrefix(step.Uses, "docker/setup-buildx-action@") {
					if seen[jobID] {
						t.Errorf("%s job %s: multiple Buildx setup steps", name, jobID)
					}
					seen[jobID] = true
					if step.With["version"] != "v0.38.2" {
						t.Errorf("%s: Buildx must pin the reviewed readiness and registry token retry fixes, got %q", name, step.With["version"])
					}
				}
			}
		}
		for _, jobID := range requiredJobs {
			if !seen[jobID] {
				t.Errorf("%s job %s: no Buildx setup to verify", name, jobID)
			}
		}
	}
}
