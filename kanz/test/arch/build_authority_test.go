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
	Needs       string
	Outputs     map[string]string
	Permissions map[string]string
	Strategy    map[string]any
	Steps       []map[string]any
}

type imageAuthorityWorkflow struct {
	On struct {
		PullRequest yaml.Node `yaml:"pull_request"`
	}
	Permissions map[string]string
	Jobs        map[string]imageAuthorityJob
}

// A push-only step condition does not restrict the token issued to a PR job.
// Check the job authority and its event boundary, including alias expansion.
func validateImageAuthority(w imageAuthorityWorkflow) error {
	pr, ok := w.Jobs["image"]
	if !ok || pr.If != "github.event_name == 'pull_request' && needs.scope.outputs.application == 'true'" || pr.Needs != "scope" {
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
	if !ok || publish.If != "github.event_name == 'push' && github.ref == 'refs/heads/main' && github.repository == 'eighred/kanz' && needs.scope.outputs.application == 'true'" || publish.Needs != "scope" {
		return fmt.Errorf("publisher must be restricted to canonical main pushes")
	}
	if publish.Permissions["packages"] != "write" || publish.Permissions["id-token"] != "write" {
		return fmt.Errorf("publisher lacks explicit registry/federation authority")
	}
	if len(pr.Steps) == 0 || !reflect.DeepEqual(pr.Steps, publish.Steps) || !reflect.DeepEqual(pr.Strategy, publish.Strategy) {
		return fmt.Errorf("validation and publication must use the same nonempty build definition")
	}
	if w.On.PullRequest.Kind != yaml.ScalarNode || w.On.PullRequest.Tag != "!!null" {
		return fmt.Errorf("complete image scope must see every PR, including unknown inputs")
	}
	scope, ok := w.Jobs["scope"]
	if !ok || scope.If != "" || scope.Outputs["application"] != "${{ steps.scope.outputs.application }}" {
		return fmt.Errorf("image scope must run before any registry authority")
	}
	var history, selector bool
	for _, step := range scope.Steps {
		if step["uses"] == "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1" {
			with, ok := step["with"].(map[string]any)
			history = ok && with["fetch-depth"] == 0
		}
		selector = selector || (step["id"] == "scope" && step["run"] == "python3 .github/scripts/required_ci.py images")
	}
	if !history || !selector {
		return fmt.Errorf("image scope must use complete checked Git history and the shared policy")
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
		"missing input": func(w *imageAuthorityWorkflow) { w.On.PullRequest.Kind = yaml.MappingNode },
		"missing scope dependency": func(w *imageAuthorityWorkflow) {
			j := w.Jobs["publish"]
			j.Needs = ""
			w.Jobs["publish"] = j
		},
		"missing full history": func(w *imageAuthorityWorkflow) {
			j := w.Jobs["scope"]
			j.Steps = nil
			w.Jobs["scope"] = j
		},
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
