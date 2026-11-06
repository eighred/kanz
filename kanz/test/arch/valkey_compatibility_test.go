package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const valkeyProofImage = "valkey/valkey:9.1.3-alpine@sha256:bf09d58e96d6b4524a15396b65b242bbe18beebbcc15a04d8ee75ddac62fae0b"
const valkeyProofCommand = `go test -tags redis -p 1 -race -json -count=1 -run "$(python ../.github/scripts/check_valkey_evidence.py --pattern)" ./pkg/redisadapter ./services/webhook-ingest/internal/ingest ./services/api-gateway/internal/middleware | tee "$RUNNER_TEMP/valkey-proof.jsonl"`
const valkeyProofCheck = `python ../.github/scripts/check_valkey_evidence.py "$RUNNER_TEMP/valkey-proof.jsonl"`

func checkValkeyCompatibilityCI(data []byte) error {
	if err := checkGoShardMatrix(data); err != nil {
		return err
	}
	var workflow struct {
		Jobs map[string]struct {
			Services map[string]struct{ Image, Options string }
			Steps    []struct {
				Env             map[string]string
				Run             string
				If              string
				ContinueOnError bool `yaml:"continue-on-error"`
			}
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return err
	}
	job := workflow.Jobs["go-race"]
	server := job.Services["redis"]
	if server.Image != valkeyProofImage || !strings.Contains(server.Options, `--health-cmd "valkey-cli ping"`) {
		return fmt.Errorf("CI must start and health-check the pinned Valkey dependency")
	}
	for _, step := range job.Steps {
		lines := strings.Split(step.Run, "\n")
		proof, check := -1, -1
		for i, line := range lines {
			if strings.TrimSpace(line) == valkeyProofCommand {
				proof = i
			}
			if strings.TrimSpace(line) == valkeyProofCheck {
				check = i
			}
		}
		if proof >= 0 && check > proof && step.If == "matrix.kind == 'quality'" && !step.ContinueOnError &&
			strings.Contains(step.Run, "set -o pipefail\n") &&
			strings.Contains(step.Run, "python -m unittest discover -s ../.github/scripts -p test_check_valkey_evidence.py") &&
			step.Env["TEST_REDIS_URL"] == "redis://localhost:6379/0" && step.Env["TEST_VALKEY_VERSION"] == "9.1.3" {
			return nil
		}
	}
	return fmt.Errorf("CI must run and verify every real Valkey compatibility case without optional gating")
}

func TestValkeyCompatibilityUsesRealPinnedServer(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(moduleRoot(t)), ".github/workflows/kanz-ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.ReplaceAll(string(data), "\r\n", "\n"))
	if err := checkValkeyCompatibilityCI(data); err != nil {
		t.Fatal(err)
	}
	for name, mutation := range map[string][2]string{
		"redis instead":         {valkeyProofImage, "redis:7-alpine"},
		"mutable tag":           {valkeyProofImage, "valkey/valkey:9.1.3-alpine"},
		"wrong version":         {"TEST_VALKEY_VERSION: 9.1.3", "TEST_VALKEY_VERSION: 9.1.2"},
		"missing URL":           {"TEST_REDIS_URL: redis://localhost:6379/0", "TEST_REDIS_URL: ''"},
		"no identity execution": {"check_valkey_evidence.py --pattern)", "check_valkey_evidence.py --unused)"},
		"missing package":       {"./pkg/redisadapter ./services/webhook-ingest", "./services/webhook-ingest"},
		"no race":               {"-race -json", "-json"},
		"missing pipefail":      {"          set -o pipefail\n          python -m unittest", "          python -m unittest"},
		"missing checker tests": {"python -m unittest discover -s ../.github/scripts -p test_check_valkey_evidence.py", "echo no-checker-tests"},
		"missing verifier":      {valkeyProofCheck, "echo no-verification"},
		"commented proof":       {valkeyProofCommand, "# " + valkeyProofCommand},
	} {
		t.Run(name, func(t *testing.T) {
			mutated := strings.Replace(string(data), mutation[0], mutation[1], 1)
			if mutated == string(data) {
				t.Fatal("mutation did not reach its target")
			}
			if err := checkValkeyCompatibilityCI([]byte(mutated)); err == nil {
				t.Fatal("weakened proof was accepted")
			}
		})
	}
}
