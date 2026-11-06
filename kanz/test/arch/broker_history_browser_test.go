package arch

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Source wiring guard, not browser layout proof. Chromium owns the real scroll
// and route-race assertions; the existing web job must execute it without skips.
func checkBrokerHistoryBrowser(data []byte) error {
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name     string `yaml:"name"`
				Run      string `yaml:"run"`
				Continue any    `yaml:"continue-on-error"`
				If       string `yaml:"if"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return err
	}
	for _, step := range workflow.Jobs["web"].Steps {
		if step.Name != "Qualify broker history rendering in Chromium" {
			continue
		}
		if step.Continue != nil || step.If != "" {
			return fmt.Errorf("browser proof cannot be conditional or ignored")
		}
		for _, required := range []string{"set -euo pipefail", "npx --no-install playwright install --with-deps chromium", "node e2e/broker-history.cjs"} {
			if !strings.Contains(step.Run, required) {
				return fmt.Errorf("missing browser proof command %s", required)
			}
		}
		return nil
	}
	return fmt.Errorf("broker history browser proof absent")
}
func TestBrokerHistoryRequiresActualBrowser(t *testing.T) {
	data := mustReadArchFile(t, filepath.Join(filepath.Dir(moduleRoot(t)), ".github/workflows/kanz-ci.yml"))
	if err := checkBrokerHistoryBrowser(data); err != nil {
		t.Fatal(err)
	}
	for _, change := range [][2]string{{"node e2e/broker-history.cjs", "echo skipped"}, {"name: Qualify broker history rendering in Chromium", "name: Qualify broker history rendering in Chromium\n        continue-on-error: true"}, {"name: Qualify broker history rendering in Chromium", "name: omitted"}} {
		mutated := strings.Replace(string(data), change[0], change[1], 1)
		if mutated == string(data) {
			t.Fatal("mutation missed")
		}
		if checkBrokerHistoryBrowser([]byte(mutated)) == nil {
			t.Fatal("missing browser proof accepted")
		}
	}
}
