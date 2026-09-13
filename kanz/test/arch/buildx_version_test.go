package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Pin the tool as well as the setup action: an unpinned runner supplied the
// v0.37.0 readiness-deadline regression even though the action was pinned.
func TestBuildxReadinessFixIsPinned(t *testing.T) {
	for _, name := range []string{"build.yml", "release.yml"} {
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
		found := false
		for _, job := range workflow.Jobs {
			for _, step := range job.Steps {
				if strings.HasPrefix(step.Uses, "docker/setup-buildx-action@") {
					found = true
					if step.With["version"] != "v0.37.1" {
						t.Errorf("%s: Buildx must pin the reviewed readiness fix, got %q", name, step.With["version"])
					}
				}
			}
		}
		if !found {
			t.Errorf("%s: no Buildx setup to verify", name)
		}
	}
}
