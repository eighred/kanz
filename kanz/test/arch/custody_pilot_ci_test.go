package arch

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func checkCustodyPilotCI(data []byte) error {
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name     string
				Run      string
				If       string
				Uses     string
				With     map[string]string
				Continue any `yaml:"continue-on-error"`
			}
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return err
	}
	const quality = "matrix.kind == 'quality'"
	const pilot = "python3 -B kanz/dev/custody_pilot.py "
	const receipt = ` --receipt "$RUNNER_TEMP/custody-pilot/`
	recipes := []struct{ name, run string }{
		{"Reconcile signed custody statements through the operator browser", "set -euo pipefail\n" +
			`mkdir "$RUNNER_TEMP/custody-pilot"` + "\n" +
			pilot + "--pull-image" + receipt + `comparisons.json"` + "\n" +
			`git rev-parse HEAD 'HEAD^{tree}' > "$RUNNER_TEMP/custody-pilot/source.txt"`},
		{"Reconcile signed custody statements in the named Accounting schema",
			pilot + "--named-schema" + receipt + `named-comparisons.json"`},
		{"Recover signed custody comparisons in both Accounting layouts", "set -euo pipefail\n" +
			pilot + "--pitr" + receipt + `recovery.json"` + "\n" +
			pilot + "--pitr --named-schema" + receipt + `named-recovery.json"`},
	}
	next := 0
	archived := false
	for _, step := range workflow.Jobs["go-race"].Steps {
		for i, recipe := range recipes {
			if step.Name == recipe.name {
				if i != next || step.If != quality || step.Continue != nil || strings.TrimSpace(step.Run) != recipe.run {
					return fmt.Errorf("custody qualification must run the expected recipe in order: %s", recipe.name)
				}
				next++
			}
		}
		if step.Name == "Preserve signed custody comparison evidence" {
			if archived || next != len(recipes) || step.If != "success() && "+quality || step.Continue != nil ||
				!strings.HasPrefix(step.Uses, "actions/upload-artifact@") ||
				step.With["path"] != "${{ runner.temp }}/custody-pilot/" || step.With["if-no-files-found"] != "error" {
				return fmt.Errorf("custody evidence must be archived after every successful layout and recovery check")
			}
			archived = true
		}
	}
	if !archived {
		return fmt.Errorf("custody qualification evidence is not archived")
	}
	return nil
}

func TestCustodyPilotCIQualifiesBothAccountingLayoutsAndRecovery(t *testing.T) {
	data := mustReadArchFile(t, filepath.Join(filepath.Dir(moduleRoot(t)), ".github/workflows/kanz-ci.yml"))
	if err := checkCustodyPilotCI(data); err != nil {
		t.Fatal(err)
	}
	for _, change := range [][2]string{
		{"--named-schema --receipt", "--receipt"},
		{"--pitr --named-schema", "--named-schema"},
		{"--pitr --receipt", "--receipt"},
		{"name: Recover signed custody comparisons in both Accounting layouts",
			"name: Recover signed custody comparisons in both Accounting layouts\n        continue-on-error: true"},
		{"${{ runner.temp }}/custody-pilot/", "${{ runner.temp }}/custody-pilot/comparisons.json"},
	} {
		mutated := strings.Replace(string(data), change[0], change[1], 1)
		if mutated == string(data) {
			t.Fatalf("mutation missed %q", change[0])
		}
		if checkCustodyPilotCI([]byte(mutated)) == nil {
			t.Fatalf("lost proof accepted after changing %q", change[0])
		}
	}
}
