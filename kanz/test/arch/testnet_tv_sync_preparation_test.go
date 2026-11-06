package arch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var tvPreparationMarkers = map[string][]string{
	"tools/Invoke-VenueSecrets.ps1":                                          {"[switch]$PrepareTVSync", "TV preparation requires DataPlane Bootstrap mode", "TV bootstrap requires exact origin/main", "TV bootstrap source differs from reviewed main", "012619468098", "i-0ee1235614219f089", "if ($PrepareTVSync) { ' tv-sync' }"},
	"tools/Install-TestnetDataPlane.ps1":                                     {"[switch]$PrepareTVSync", "HEAD $headCommit is not exact origin/main", "git -C $repoRoot diff --quiet", "i-0ee1235614219f089", "012619468098", "if ($PrepareTVSync)", "Expected four preparation resources", "Resolve-ReviewedTestnetReleaseTag", "prepare-testnet-tv-sync.sh"},
	"tools/prepare-testnet-tv-sync.sh":                                       {"expected_cluster_uid=86ea9839-5806-461d-b2d1-afd1ff5d87ff", "sha256sum --check --status", "terminal_job", "preparation-refused-base-job-not-complete", "preparation-refused-existing-job-not-complete", "--dry-run=server", "preconditions:{uid:$uid}", "kanz.io/preparation-owner", "batch.kubernetes.io/controller-uid=$uid", ".uid==$uid and .controller==true", ".imageID|endswith", "preparation-rollback-unverified", ".metadata=($before[0].metadata|del(.managedFields))", "tv-sync-database-preparation-verified"},
	"tools/bootstrap-testnet-data-plane.sh":                                  {"write_read_policy tv-sync kanz/tv-sync-runtime kanz-services tv-sync", "if field_exists \"$runtime_path\" migrate_dsn; then", "absence is unverified", "if [ \"$service\" != tv-sync ]; then"},
	"kanz/infra/overlays/testnet-tokyo/data/postgres-provisioner.yaml":       {"case \"${1:-base}\" in", "provision_database tv-sync tv_sync tv_sync", "unsupported provisioner mode", "SELECT count(*) = 16 AS roles_ok"},
	"kanz/infra/overlays/testnet-tokyo/tv-sync-preparation/provisioner.yaml": {"command: [bash, /scripts/provision.sh, tv-sync]", "secretProviderClass: tv-sync-role-passwords", "serviceAccountName: postgres-provisioner"},
	"kanz/infra/overlays/testnet-tokyo/data/tv-sync-migration.yaml":          {"secretProviderClass: tv-sync-role-passwords", "serviceAccountName: postgres-provisioner", "147db5e4edbb6151f44f1b2c799d14d44437298dc00e19d8ad983d54ab333a24"},
	".github/workflows/kanz-ci.yml":                                          {"-run '^TestTokyoTVSyncSeparateRolesAndRecovery$' -count=1 -json", "check_tv_sync_evidence.py"},
}

func checkTVPreparation(files map[string]string) error {
	for path, markers := range tvPreparationMarkers {
		for _, marker := range markers {
			if !strings.Contains(files[path], marker) {
				return fmt.Errorf("%s missing preparation boundary %q", path, marker)
			}
		}
	}
	return nil
}

func TestTokyoTVSyncPreparationBoundaries(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	files := map[string]string{}
	for path := range tvPreparationMarkers {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		files[path] = string(data)
	}
	if err := checkTVPreparation(files); err != nil {
		t.Fatal(err)
	}
	for path, markers := range tvPreparationMarkers {
		for _, marker := range markers {
			t.Run(path+"/"+marker, func(t *testing.T) {
				original := files[path]
				files[path] = strings.ReplaceAll(original, marker, "REMOVED")
				defer func() { files[path] = original }()
				if checkTVPreparation(files) == nil {
					t.Fatal("removed preparation boundary accepted")
				}
			})
		}
	}
}

func TestTokyoTVSyncPreparationDoesNotActivateWorkloads(t *testing.T) {
	root := moduleRoot(t)
	for _, fixture := range []struct {
		path  string
		count int
		want  []string
	}{
		{"infra/overlays/testnet-tokyo/data", 37, nil},
		{"infra/overlays/testnet-tokyo/tv-sync-preparation", 4, []string{"name: tv-sync-provisioner", "name: tv-sync-migration", "name: tv-sync-role-passwords"}},
	} {
		output, err := exec.Command("kubectl", "kustomize", "--load-restrictor=LoadRestrictionsNone", filepath.Join(root, fixture.path)).Output()
		if err != nil {
			t.Fatal(err)
		}
		body := string(output)
		if strings.Count(body, "\nkind:") != fixture.count {
			t.Fatalf("%s resource count changed", fixture.path)
		}
		for _, marker := range fixture.want {
			if !strings.Contains(body, marker) {
				t.Fatalf("missing %s", marker)
			}
		}
		if fixture.want == nil && (strings.Contains(body, "name: tv-sync-provisioner") || strings.Contains(body, "name: tv-sync-role-passwords") || strings.Contains(body, "name: tv-sync-migration")) {
			t.Fatal("ordinary data install depends on unactivated tv-sync")
		}
	}
	active, err := os.ReadFile(filepath.Join(root, "infra/overlays/testnet-tokyo/kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(active), "tv-sync-activation") || strings.Contains(string(active), "tv-sync-preparation") || strings.Contains(string(active), "../../deploy/tv-sync-deploy.yaml") {
		t.Fatal("preparation activated tv-sync workload")
	}
}

func TestTokyoTVSyncPreparationOperatorRefusals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unchanged operator engine fixture executes in Linux CI")
	}
	root := filepath.Dir(moduleRoot(t))
	cmd := exec.Command("python3", "-m", "unittest", "discover", "-s", filepath.Join(root, "tools"), "-p", "test_tv_sync_preparation.py", "-v")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("operator fixture: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "Ran 9 tests") || strings.Contains(string(output), "skipped") {
		t.Fatalf("operator fixture did not complete all nine required cases: %s", output)
	}
}

func TestReviewedTVSyncReleaseTagBinding(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	shell, err := exec.LookPath("pwsh")
	if err != nil {
		shell, err = exec.LookPath("powershell")
	}
	if err != nil {
		t.Fatal("PowerShell required for release binding proof")
	}
	if output, err := exec.Command(shell, "-NoProfile", "-NonInteractive", "-File", filepath.Join(root, "tools/Test-ReviewedTvSyncRelease.ps1"), "-FixtureDirectory", t.TempDir()).CombinedOutput(); err != nil {
		t.Fatalf("release binding: %v\n%s", err, output)
	}
}
