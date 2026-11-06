package arch

import (
	"fmt"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

var tvEarlyProofs = []string{
	"Prove tv-sync separate roles and checkpoint recovery",
	"Prove durable catch-up before tv-sync Broker readiness",
	"Qualify restored tv-sync snapshot without writes",
}

var tvProofPrerequisites = []string{
	"Provision non-superuser DB role (so RLS is enforced, not bypassed)",
	"Generate kanz-schemas Go SDK",
	"Download reviewed Go dependencies with bounded transport retries",
	"Start NATS (JetStream)",
	"Bootstrap the NATS topology (the script production applies)",
	"Install PyYAML",
	"Build the real MFA browser boundary",
}

const tvFullRaceStep = "Test (race) — incl. arch + contract + Postgres + real-bus + M1 loop"

// Qualification runs sequentially after setup, before the long suite. Its
// existing guards still own exact commands, dependencies, and no-skip evidence.
func checkTVProofOrder(names []string) error {
	positions := map[string]int{}
	for i, name := range names {
		if name == "" {
			continue
		}
		if _, exists := positions[name]; exists {
			return fmt.Errorf("duplicate CI step %q", name)
		}
		positions[name] = i
	}
	race, exists := positions[tvFullRaceStep]
	if !exists {
		return fmt.Errorf("missing complete race suite")
	}
	previous := -1
	for _, name := range tvEarlyProofs {
		proof, exists := positions[name]
		if !exists || proof >= race || proof <= previous {
			return fmt.Errorf("TV proof %q must precede full race in qualification order", name)
		}
		for _, dependency := range tvProofPrerequisites {
			setup, exists := positions[dependency]
			if !exists || setup >= proof {
				return fmt.Errorf("TV proof precedes prerequisite %q", dependency)
			}
		}
		previous = proof
	}
	return nil
}

func TestTVQualificationRunsAfterSetupBeforeFullRace(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct{ Name string }
		}
	}
	data := mustReadArchFile(t, filepath.Join(filepath.Dir(moduleRoot(t)), ".github/workflows/kanz-ci.yml"))
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, step := range workflow.Jobs["go-race"].Steps {
		names = append(names, step.Name)
	}
	if err := checkTVProofOrder(names); err != nil {
		t.Fatal(err)
	}
	for _, target := range append(append([]string{}, tvEarlyProofs...), tvProofPrerequisites...) {
		t.Run(target, func(t *testing.T) {
			var moved []string
			found := false
			for _, name := range names {
				if name == target {
					found = true
					continue
				}
				moved = append(moved, name)
			}
			if !found {
				t.Fatal("mutation missed step")
			}
			if checkTVProofOrder(moved) == nil {
				t.Fatal("missing required step accepted")
			}
			if checkTVProofOrder(append(moved, target)) == nil {
				t.Fatal("late proof or prerequisite accepted")
			}
		})
	}
}
