package arch

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// setup-go hashes these files into an immutable cache key. Other jobs must not
// seed the full race/tag job's profile with a smaller compilation inventory.
func checkGoCacheProfile(profile string, others []string) error {
	normalize := func(s string) []string {
		lines := strings.Fields(s)
		sort.Strings(lines)
		return lines
	}
	want := normalize("kanz/.gotmp/go-ci-cache-profile kanz/go.mod kanz/go.sum .github/go-ci-cache-profile kanz-schemas/buf.gen.yaml kanz-schemas/proto/**/*.proto")
	if !reflect.DeepEqual(normalize(profile), want) {
		return fmt.Errorf("complete Go job lost its reviewed compiler cache profile")
	}
	for _, other := range others {
		if reflect.DeepEqual(normalize(other), want) {
			return fmt.Errorf("another job can seed the complete Go compiler cache")
		}
	}
	return nil
}

func TestGoCICompilerCacheProfileIsExclusive(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	if strings.TrimSpace(string(mustReadArchFile(t, filepath.Join(root, ".github/go-ci-cache-profile")))) != "kanz-go-race-v1" {
		t.Fatal("reviewed compiler profile version changed")
	}
	var profile string
	var others []string
	for _, path := range workflowFiles(t, root) {
		var workflow struct {
			Jobs map[string]struct {
				Steps []struct {
					Uses string
					With map[string]string
				}
			}
		}
		if err := yaml.Unmarshal(mustReadArchFile(t, path), &workflow); err != nil {
			t.Fatal(err)
		}
		for job, config := range workflow.Jobs {
			for _, step := range config.Steps {
				if !strings.HasPrefix(step.Uses, "actions/setup-go@") {
					continue
				}
				value := step.With["cache-dependency-path"]
				if filepath.Base(path) == "kanz-ci.yml" && job == "go-race" {
					if profile != "" || step.With["cache"] == "false" {
						t.Fatal("duplicate or disabled complete Go cache")
					}
					profile = value
				} else {
					others = append(others, value)
				}
			}
		}
	}
	if err := checkGoCacheProfile(profile, others); err != nil {
		t.Fatal(err)
	}
	for _, remove := range strings.Fields(profile) {
		t.Run("missing-"+remove, func(t *testing.T) {
			if checkGoCacheProfile(strings.Replace(profile, remove, "", 1), others) == nil {
				t.Fatal("missing cache input accepted")
			}
		})
	}
	if checkGoCacheProfile(profile, append(others, profile)) == nil {
		t.Fatal("competing cache writer accepted")
	}
}
