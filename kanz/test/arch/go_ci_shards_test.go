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

// This is a reviewed CI fixture recipe, not proof of arbitrary shell equivalence
// or production permission to resume. Runtime webhook cases still require fills.
func checkGoShardOMSFixture(data []byte) error {
	// Explicit YAML tags avoid treating the hyphenated working-directory as absent.
	var parsed struct {
		Jobs map[string]struct {
			Steps []struct {
				Name      string `yaml:"name"`
				Run       string `yaml:"run"`
				If        string `yaml:"if"`
				Directory string `yaml:"working-directory"`
			}
		}
	}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return err
	}
	start, resume, race := -1, -1, -1
	for i, step := range parsed.Jobs["go-race"].Steps {
		switch step.Name {
		case "Start the OMS on the bus (for the M1 loop certification)":
			start = i
		case tvFullRaceStep:
			race = i
		case "Resume only the disposable simulated OMS fixture":
			if resume != -1 || step.If != "" || step.Directory != "kanz" {
				return fmt.Errorf("each isolated runner must establish its own fixture posture")
			}
			resume = i
			required := []string{`for i in $(seq 1 30); do`, `grep -Fq '"msg":"halt gate armed — this process now hears the platform brake"' /tmp/oms.log`, `test "$armed" = true`, `go run ./cmd/kanz-halt --nats nats://localhost:4222`, `--resume --by operator:ci-fixture`, `--reason 'disposable CI simulated OMS fixture' --tenant __system__`}
			for _, part := range required {
				if !strings.Contains(step.Run, part) {
					return fmt.Errorf("fixture resume must preserve closed startup, confirmed subscription and exact synthetic loopback command")
				}
			}
			if strings.Index(step.Run, `test "$armed" = true`) > strings.Index(step.Run, "go run ./cmd/kanz-halt") {
				return fmt.Errorf("resume precedes subscription readiness")
			}
		}
	}
	if start < 0 || resume <= start || race <= resume {
		return fmt.Errorf("fixture resume must follow OMS startup and precede race tests")
	}
	return nil
}

func TestGoCIShardsExplicitlyPrepareSimulatedOMS(t *testing.T) {
	data := mustReadArchFile(t, filepath.Join(filepath.Dir(moduleRoot(t)), ".github/workflows/kanz-ci.yml"))
	if err := checkGoShardOMSFixture(data); err != nil {
		t.Fatal(err)
	}
	text := string(data)
	begin := strings.Index(text, "      - name: Resume only the disposable simulated OMS fixture")
	end := strings.Index(text[begin:], "      # TEST_KAFKA_BROKERS") + begin
	step := text[begin:end]
	without := text[:begin] + text[end:]
	reordered := strings.Replace(without, "      - name: Start the OMS on the bus (for the M1 loop certification)", step+"      - name: Start the OMS on the bus (for the M1 loop certification)", 1)
	if checkGoShardOMSFixture([]byte(reordered)) == nil {
		t.Fatal("resume before OMS readiness accepted")
	}
	for _, mutation := range [][2]string{
		{"Resume only the disposable simulated OMS fixture", "Removed fixture posture"},
		{"name: Resume only the disposable simulated OMS fixture", "name: Resume only the disposable simulated OMS fixture\n        if: matrix.shard == 0"},
		{"go run ./cmd/kanz-halt --nats nats://localhost:4222", "go run ./cmd/kanz-halt --nats nats://elsewhere:4222"},
		{"--resume --by operator:ci-fixture", "--resume --by system:ci-fixture"},
		{"disposable CI simulated OMS fixture' --tenant __system__", "different reason' --tenant other"},
		{`test "$armed" = true`, "true"},
		{`grep -Fq '"msg":"halt gate armed — this process now hears the platform brake"'`, `grep -Fq '"msg":"oms subscribing"'`},
		{"Start the OMS on the bus (for the M1 loop certification)", "Removed OMS startup"},
	} {
		changed := strings.Replace(string(data), mutation[0], mutation[1], 1)
		if changed == string(data) {
			t.Fatal("mutation missed target")
		}
		if checkGoShardOMSFixture([]byte(changed)) == nil {
			t.Fatal("incomplete isolated fixture accepted")
		}
	}
}
