package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Bounded source guard for the staged #232/#1413 fixture, not a proof of CNI
// semantics. The mandatory disposable workflow provides runtime enforcement.
var messagingStagingMarkers = map[string][]string{
	"tools/tokyo-messaging-proof/proof.py": {
		"feed.create_enforcing_cluster(name, scratch)",
		"return feed.command(['kubectl', '--kubeconfig', kubeconfig, '--context', 'kind-' + name, *args]", "except TimeoutError:",
		"subprocess.run(['kubectl', '--kubeconfig', kubeconfig, '--context', 'kind-' + name,",
		"nats('account', 'restore', '/tmp/fixture-backup')",
		"if stream_inventory() != before:",
		"if current['metadata']['uid'] != before_redis['metadata']['uid']:",
		"status['restartCount'] > before_restarts", "if observed != expected:",
		"report['passed'] &= report['cleanupPassed']",
		"validate_report(report, report['sourceSHA'])",
	},
	".github/workflows/tokyo-messaging-proof.yml": {
		"contents: read", "persist-credentials: false", "messaging-isolation:",
		"python3 -B tools/tokyo-messaging-proof/proof.py", "if-no-files-found: error",
	},
}

func checkMessagingStaging(files map[string]string) error {
	for path, markers := range messagingStagingMarkers {
		for _, marker := range markers {
			if !strings.Contains(files[path], marker) {
				return fmt.Errorf("%s missing %q", path, marker)
			}
		}
	}
	wf := files[".github/workflows/tokyo-messaging-proof.yml"]
	if strings.Contains(wf, "continue-on-error") || strings.Contains(wf, "id-token:") || strings.Contains(wf, "write") {
		return fmt.Errorf("messaging fixture cannot bypass failure or acquire write authority")
	}
	return nil
}

func TestTokyoMessagingStagingBoundary(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	files := map[string]string{}
	for path := range messagingStagingMarkers {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		files[path] = string(b)
	}
	if err := checkMessagingStaging(files); err != nil {
		t.Fatal(err)
	}
	for path, markers := range messagingStagingMarkers {
		for _, marker := range markers {
			t.Run(marker, func(t *testing.T) {
				mutated := map[string]string{}
				for name, body := range files {
					mutated[name] = body
				}
				mutated[path] = strings.Replace(mutated[path], marker, "removed-boundary", 1)
				if checkMessagingStaging(mutated) == nil {
					t.Fatal("boundary removal accepted")
				}
			})
		}
	}
}
