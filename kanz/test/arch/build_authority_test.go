package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

type imageAuthorityJob struct {
	If          string
	Permissions map[string]string
	Strategy    map[string]any
	Steps       []map[string]any
}

type imageAuthorityWorkflow struct {
	On struct {
		PullRequest struct {
			Paths []string
		} `yaml:"pull_request"`
	}
	Permissions map[string]string
	Jobs        map[string]imageAuthorityJob
}

// A push-only step condition does not restrict the token issued to a PR job.
// Check the job authority and its event boundary, including alias expansion.
func validateImageAuthority(w imageAuthorityWorkflow) error {
	pr, ok := w.Jobs["image"]
	if !ok || pr.If != "github.event_name == 'pull_request'" {
		return fmt.Errorf("image validation must be isolated to pull requests")
	}
	permissions := w.Permissions
	if pr.Permissions != nil {
		permissions = pr.Permissions
	}
	for name, level := range permissions {
		if level == "write" {
			return fmt.Errorf("PR image job has %s write authority", name)
		}
	}
	if permissions["contents"] != "read" || permissions["packages"] != "read" {
		return fmt.Errorf("PR validation needs checkout and private base-image read access")
	}
	publish, ok := w.Jobs["publish"]
	if !ok || publish.If != "github.event_name == 'push' && github.ref == 'refs/heads/main' && github.repository == 'eighred/kanz'" {
		return fmt.Errorf("publisher must be restricted to canonical main pushes")
	}
	if publish.Permissions["packages"] != "write" || publish.Permissions["id-token"] != "write" {
		return fmt.Errorf("publisher lacks explicit registry/federation authority")
	}
	if len(pr.Steps) == 0 || !reflect.DeepEqual(pr.Steps, publish.Steps) || !reflect.DeepEqual(pr.Strategy, publish.Strategy) {
		return fmt.Errorf("validation and publication must use the same nonempty build definition")
	}
	for _, required := range []string{"kanz/**", "kanz-py/**", "web/**", ".dockerignore", ".github/actions/**", ".github/scripts/**"} {
		found := false
		for _, path := range w.On.PullRequest.Paths {
			found = found || path == required
		}
		if !found {
			return fmt.Errorf("image input %s does not trigger validation", required)
		}
	}
	return nil
}

func TestImageValidationHasNoPublisherAuthority(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "build.yml"))
	if err != nil {
		t.Fatal(err)
	}
	read := func() imageAuthorityWorkflow {
		var w imageAuthorityWorkflow
		if err := yaml.Unmarshal(data, &w); err != nil {
			t.Fatal(err)
		}
		return w
	}
	if err := validateImageAuthority(read()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*imageAuthorityWorkflow){
		"global write": func(w *imageAuthorityWorkflow) { w.Permissions["packages"] = "write" },
		"PR OIDC": func(w *imageAuthorityWorkflow) {
			j := w.Jobs["image"]
			j.Permissions = map[string]string{"contents": "read", "packages": "read", "id-token": "write"}
			w.Jobs["image"] = j
		},
		"unrestricted publisher": func(w *imageAuthorityWorkflow) {
			j := w.Jobs["publish"]
			j.If = ""
			w.Jobs["publish"] = j
		},
		"missing input": func(w *imageAuthorityWorkflow) { w.On.PullRequest.Paths = nil },
		"different published build": func(w *imageAuthorityWorkflow) {
			j := w.Jobs["publish"]
			j.Steps = nil
			w.Jobs["publish"] = j
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := read()
			mutate(&w)
			if err := validateImageAuthority(w); err == nil {
				t.Fatal("unsafe image workflow accepted")
			}
		})
	}
}
