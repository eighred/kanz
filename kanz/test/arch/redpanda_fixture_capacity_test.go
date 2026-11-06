package arch

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// This pins the reviewed fixture recipe, not arbitrary Python semantics or
// actual disk capacity. The Linux proof must observe the broker's own reserve.
var redpandaCapacityMarkers = map[string][]string{
	"tools/redpanda-identity/qualify.py": {
		"HEADROOM = 2 << 30", "validate_report(evidence, sha)", "capacity['availableBytes'] <= reserve + HEADROOM",
		"'rpk','cluster','config','get','storage_min_free_bytes','--ignore-profile'",
		"'admin.hosts=127.0.0.1:9644'", "evidence['storageObservations'].append(dict(storage, case=case))",
		"require_capacity(storage, storage['producerReserveBytes'])",
	},
	"tools/redpanda-identity/prepare_runner.py": {
		"SDK = Path('/usr/local/lib/android/sdk')", "DOCKER = Path('/var/lib/docker')",
		"MIN_FREE = 24 << 30", "env.get('RUNNER_ENVIRONMENT') != 'github-hosted'",
		"env.get('ImageOS') not in ('ubuntu24', 'ubuntu26')", "regular_tree_root(SDK)",
		"verify_sdk(os.environ)", "target.startswith(str(SDK) + '/')",
		"'timeout', '180', 'rm', '-rf', '--one-file-system'", "'--preserve-root=all', '--', str(SDK)",
		"if os.path.lexists(SDK):", "if after < MIN_FREE:",
	},
	".github/workflows/redpanda-identity-proof.yml": {
		"python3 -B tools/redpanda-identity/prepare_runner.py",
		"python3 -B -m unittest discover -s tools/redpanda-identity -p 'test_*.py'",
	},
}

func checkRedpandaCapacity(files map[string]string) error {
	for name, markers := range redpandaCapacityMarkers {
		for _, marker := range markers {
			if !strings.Contains(files[name], marker) {
				return fmt.Errorf("missing capacity boundary in %s", name)
			}
		}
	}
	source := files["tools/redpanda-identity/qualify.py"]
	if strings.Index(source, "require_capacity(storage, storage['producerReserveBytes'])") > strings.Index(source, "apply([job(case,sa,mode,image)])") {
		return fmt.Errorf("capacity gate follows broker write")
	}
	return nil
}

func TestRedpandaFixturePreservesBrokerStorageReserve(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	files := map[string]string{}
	for name := range redpandaCapacityMarkers {
		files[name] = string(mustReadArchFile(t, filepath.Join(root, filepath.FromSlash(name))))
	}
	if err := checkRedpandaCapacity(files); err != nil {
		t.Fatal(err)
	}
	for name, markers := range redpandaCapacityMarkers {
		for _, marker := range markers {
			original := files[name]
			files[name] = strings.ReplaceAll(original, marker, "removed capacity boundary")
			if checkRedpandaCapacity(files) == nil {
				t.Fatalf("weakening accepted: %s %s", name, marker)
			}
			files[name] = original
		}
	}
}
