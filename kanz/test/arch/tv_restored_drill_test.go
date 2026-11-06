package arch

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

var tvRestoreMarkers = map[string][]string{
	"tools/Invoke-TestnetDataPlaneRecovery.ps1": {"[switch]$QualifyTVSync", "mktemp -d /var/tmp/kanz-recovery-source.XXXXXX", "readonly work", "rmdir --", "$head -cne $releaseCommit", "verify_replay_exact.go", "Read-ReviewedTvSyncRelease", "Resolve-TvReplayPlatform"},
	"tools/Resolve-TvReplayPlatform.ps1":        {"$actual -cne $Digest", "$platforms.Count -ne 1", "-ceq 'amd64'"},
	"tools/tv-sync-restored-proof.sh":           {"preconditions:{uid:$uid}", "prior drill storage still exists", ".metadata.uid==$pv.spec.claimRef.uid", "PGSSLMODE=verify-full", "PGSSLROOTCERT=/run/postgres-ca/ca.crt", "current_setting('log_min_error_statement')='panic'", "automountServiceAccountToken:false", "9007199254740991", "have_decnum", "tv_exact_json_available || return 1", "$checked >= $started", ".uid==$uid and .controller==true", "--limit-bytes=8192", "qualify_tv_restored_snapshot()"},
	"tools/testnet-data-plane-recovery.sh":      {"DATABASES+=(tv_sync)", "86ea9839-5806-461d-b2d1-afd1ff5d87ff", "qualify_tv_restored_snapshot", "cleanup_tv_restore_namespace"},
	".github/workflows/kanz-ci.yml":             {"test_tv_sync_restored_proof.py", "Test-TvReplayPlatform.ps1", "run: ./tools/Test-TvReplayPreflight.ps1", "command -v jq"},
}

func restoredTVBoundary(files map[string]string) error {
	for name, markers := range tvRestoreMarkers {
		for _, marker := range markers {
			if !strings.Contains(files[name], marker) {
				return fmt.Errorf("%s missing %q", name, marker)
			}
		}
	}
	return nil
}
func TestRestoredTVDrillKeepsIsolationAndExactProof(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	files := map[string]string{}
	for name := range tvRestoreMarkers {
		files[name] = string(mustReadArchFile(t, filepath.Join(root, name)))
	}
	if err := restoredTVBoundary(files); err != nil {
		t.Fatal(err)
	}
	for name, markers := range tvRestoreMarkers {
		for _, marker := range markers {
			t.Run(name+marker, func(t *testing.T) {
				changed := map[string]string{}
				for k, v := range files {
					changed[k] = v
				}
				changed[name] = strings.ReplaceAll(changed[name], marker, "REMOVED")
				if err := restoredTVBoundary(changed); err == nil {
					t.Fatal("unsafe mutation escaped")
				}
			})
		}
	}
}
