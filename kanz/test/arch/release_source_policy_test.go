package arch

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type releaseSourceStep struct {
	Uses            string            `yaml:"uses"`
	Run             string            `yaml:"run"`
	If              string            `yaml:"if"`
	ContinueOnError bool              `yaml:"continue-on-error"`
	With            map[string]string `yaml:"with"`
	Env             map[string]string `yaml:"env"`
}

type releaseSourceJob struct {
	Needs           string              `yaml:"needs"`
	If              string              `yaml:"if"`
	ContinueOnError bool                `yaml:"continue-on-error"`
	Permissions     map[string]string   `yaml:"permissions"`
	Steps           []releaseSourceStep `yaml:"steps"`
}

type releaseSourceWorkflow struct {
	Permissions map[string]string           `yaml:"permissions"`
	Jobs        map[string]releaseSourceJob `yaml:"jobs"`
}

// This guard checks the actual YAML control flow, not words in comments. Tag
// protection remains external; CI API semantics have offline Python fixtures.
// In-memory mutations below calibrate the executable workflow wiring coverage.
func checkReleaseSourcePolicy(workflow releaseSourceWorkflow) error {
	readOnly := func(permissions map[string]string) bool {
		return len(permissions) == 1 && permissions["contents"] == "read"
	}
	if !readOnly(workflow.Permissions) {
		return fmt.Errorf("workflow must default to contents: read only")
	}
	gate, ok := workflow.Jobs["source-policy"]
	if !ok || len(gate.Permissions) != 2 || gate.Permissions["contents"] != "read" || gate.Permissions["actions"] != "read" || gate.If != "" || gate.ContinueOnError {
		return fmt.Errorf("source-policy must run unconditionally with read-only permissions")
	}
	if len(gate.Steps) != 3 || !strings.HasPrefix(gate.Steps[0].Uses, "actions/checkout@") ||
		gate.Steps[0].With["fetch-depth"] != "0" ||
		gate.Steps[1].Run != "git fetch --no-tags origin +refs/heads/main:refs/remotes/origin/main" ||
		gate.Steps[2].Run != "python3 .github/scripts/release_source.py" ||
		len(gate.Steps[2].Env) != 1 || gate.Steps[2].Env["GH_TOKEN"] != "${{ github.token }}" {
		return fmt.Errorf("source-policy must fetch full history/current main before validating release source")
	}
	for _, step := range gate.Steps {
		if step.If != "" || step.ContinueOnError {
			return fmt.Errorf("source-policy steps cannot skip or ignore failures")
		}
	}
	release, ok := workflow.Jobs["release"]
	if !ok || release.Needs != "source-policy" || release.If != "" || release.ContinueOnError {
		return fmt.Errorf("release must require successful source-policy completion")
	}
	if len(release.Permissions) != 3 || release.Permissions["contents"] != "read" ||
		release.Permissions["packages"] != "write" || release.Permissions["id-token"] != "write" {
		return fmt.Errorf("release permissions must be explicitly scoped to source read, package publish and signing")
	}
	for _, name := range []string{"source-policy", "release", "release-notes", "pin-digests"} {
		job, exists := workflow.Jobs[name]
		if !exists {
			return fmt.Errorf("required release job %s is missing", name)
		}
		if name == "release-notes" || name == "pin-digests" {
			if job.Needs != "release" || job.If != "startsWith(github.ref, 'refs/tags/v')" || job.ContinueOnError {
				return fmt.Errorf("%s must require successful release completion", name)
			}
		}
		checkouts := 0
		for _, step := range job.Steps {
			if strings.HasPrefix(step.Uses, "actions/checkout@") {
				checkouts++
				if step.With["ref"] != "${{ github.sha }}" || step.If != "" || step.ContinueOnError {
					return fmt.Errorf("%s checkout must unconditionally pin the event commit", name)
				}
			}
		}
		if checkouts != 1 {
			return fmt.Errorf("%s must have exactly one pinned source checkout", name)
		}
	}
	for name, job := range workflow.Jobs {
		if name != "release" && (job.Permissions["id-token"] == "write" || job.Permissions["packages"] == "write") {
			return fmt.Errorf("%s grants signing or publishing outside the source-gated release job", name)
		}
	}
	return nil
}

func TestReleaseSourcePolicy(t *testing.T) {
	raw, err := os.ReadFile(releaseYML)
	if err != nil {
		t.Fatal(err)
	}
	load := func() releaseSourceWorkflow {
		t.Helper()
		var workflow releaseSourceWorkflow
		if err := yaml.Unmarshal(raw, &workflow); err != nil {
			t.Fatal(err)
		}
		return workflow
	}
	if err := checkReleaseSourcePolicy(load()); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*releaseSourceWorkflow){
		"gate action write authority": func(w *releaseSourceWorkflow) { w.Jobs["source-policy"].Permissions["actions"] = "write" },
		"gate missing evidence token": func(w *releaseSourceWorkflow) { delete(w.Jobs["source-policy"].Steps[2].Env, "GH_TOKEN") },
		"gate substituted token": func(w *releaseSourceWorkflow) {
			w.Jobs["source-policy"].Steps[2].Env["GH_TOKEN"] = "${{ secrets.PUBLISH_TOKEN }}"
		},
		"workflow signing permission":  func(w *releaseSourceWorkflow) { w.Permissions["id-token"] = "write" },
		"gate signing permission":      func(w *releaseSourceWorkflow) { w.Jobs["source-policy"].Permissions["id-token"] = "write" },
		"missing gate":                 func(w *releaseSourceWorkflow) { delete(w.Jobs, "source-policy") },
		"missing release dependency":   func(w *releaseSourceWorkflow) { j := w.Jobs["release"]; j.Needs = ""; w.Jobs["release"] = j },
		"release despite gate failure": func(w *releaseSourceWorkflow) { j := w.Jobs["release"]; j.If = "always()"; w.Jobs["release"] = j },
		"ignored gate failure": func(w *releaseSourceWorkflow) {
			j := w.Jobs["source-policy"]
			j.ContinueOnError = true
			w.Jobs["source-policy"] = j
		},
		"skipped source validation":  func(w *releaseSourceWorkflow) { w.Jobs["source-policy"].Steps[2].If = "false" },
		"ignored source validation":  func(w *releaseSourceWorkflow) { w.Jobs["source-policy"].Steps[2].ContinueOnError = true },
		"source validation replaced": func(w *releaseSourceWorkflow) { w.Jobs["source-policy"].Steps[2].Run = "true" },
		"main not refreshed":         func(w *releaseSourceWorkflow) { w.Jobs["source-policy"].Steps[1].Run = "true" },
		"shallow gate history":       func(w *releaseSourceWorkflow) { w.Jobs["source-policy"].Steps[0].With["fetch-depth"] = "1" },
		"release notes bypass": func(w *releaseSourceWorkflow) {
			j := w.Jobs["release-notes"]
			j.If = "always()"
			w.Jobs["release-notes"] = j
		},
		"additional ungated signer": func(w *releaseSourceWorkflow) {
			w.Jobs["other"] = releaseSourceJob{Permissions: map[string]string{"id-token": "write"}}
		},
	}
	for _, name := range []string{"source-policy", "release", "release-notes", "pin-digests"} {
		mutations[name+" mutable checkout"] = func(w *releaseSourceWorkflow) {
			for _, step := range w.Jobs[name].Steps {
				if strings.HasPrefix(step.Uses, "actions/checkout@") {
					step.With["ref"] = "${{ github.ref }}"
				}
			}
		}
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			workflow := load()
			mutate(&workflow)
			if checkReleaseSourcePolicy(workflow) == nil {
				t.Fatal("unsafe release wiring accepted")
			}
		})
	}
}
