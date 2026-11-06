package arch

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func checkGoShardMatrix(data []byte) error {
	var workflow struct {
		Jobs map[string]struct {
			Strategy struct {
				FailFast *bool `yaml:"fail-fast"`
				Matrix   struct{ Shard []int }
			}
			Needs any
			If    string
			Steps []struct {
				Name, Run, If, Uses string
				With                map[string]string
				Env                 map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return err
	}
	race := workflow.Jobs["go-race"]
	if race.Strategy.FailFast == nil || *race.Strategy.FailFast || !reflect.DeepEqual(race.Strategy.Matrix.Shard, []int{0, 1}) {
		return fmt.Errorf("race matrix must run both isolated shards without cancellation")
	}
	complete := false
	for _, step := range race.Steps {
		if step.Name == tvFullRaceStep {
			complete = step.If == "" && strings.Contains(step.Run, `go test -race -p 1 -count=1 -json "${packages[@]}"`) && strings.Contains(step.Run, `go_shard_evidence.py select`)
		}
	}
	if !complete {
		return fmt.Errorf("each shard must execute its entire fresh serial race inventory")
	}
	aggregate := workflow.Jobs["go"]
	if !reflect.DeepEqual(aggregate.Needs, []any{"scope", "go-race"}) || aggregate.If != "${{ always() && needs.scope.outputs.application == 'true' }}" {
		return fmt.Errorf("Go union must run after every shard outcome")
	}
	var runs []string
	for _, step := range aggregate.Steps {
		if step.If != "" {
			return fmt.Errorf("Go union cannot conditionally omit proof")
		}
		runs = append(runs, step.Run)
	}
	text := strings.Join(runs, "\n")
	for _, required := range []string{`test "$SHARD_RESULT" = success`, `go list -race ./...`, `go_shard_evidence.py aggregate --packages "$RUNNER_TEMP/gotest-all-packages.txt" --directory "$RUNNER_TEMP/go-race-artifacts"`} {
		if !strings.Contains(text, required) {
			return fmt.Errorf("missing complete union check")
		}
	}
	return nil
}

func TestGoCIShardsRequireCompleteUnion(t *testing.T) {
	data := mustReadArchFile(t, filepath.Join(filepath.Dir(moduleRoot(t)), ".github/workflows/kanz-ci.yml"))
	if err := checkGoShardMatrix(data); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range [][2]string{{"shard: [0, 1]", "shard: [0]"}, {"fail-fast: false", "fail-fast: true"}, {"needs: [scope, go-race]", "needs: [scope]"}, {`test "$SHARD_RESULT" = success`, "true"}, {"go_shard_evidence.py aggregate", "go_shard_evidence.py select"}, {`go test -race -p 1 -count=1 -json "${packages[@]}"`, `go test -p 1 -json "${packages[@]}"`}} {
		changed := strings.Replace(string(data), mutation[0], mutation[1], 1)
		if changed == string(data) {
			t.Fatal("mutation missed target")
		}
		if checkGoShardMatrix([]byte(changed)) == nil {
			t.Fatal("incomplete shard proof accepted")
		}
	}
}
