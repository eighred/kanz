package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var tvTokyoBoundaries = map[string][]string{
	"kanz/infra/overlays/testnet-tokyo/kustomization.yaml": {
		"../../deploy/tv-sync-deploy.yaml", "path: tv-sync-activation.yaml",
		"digest: sha256:88e14a7fb596dc6c17670bba3bdcd43dc380f3affea09c1b6aae5d4cc9aeb91e",
	},
	"kanz/infra/overlays/testnet-tokyo/tv-sync-activation.yaml": {
		"kanz.io/release: f7217a76b48d9b6075a0683ddf35be50fbdf37cb",
		"image: 012619468098.dkr.ecr.ap-northeast-1.amazonaws.com/kanz-migrate@sha256:147db5e4edbb6151f44f1b2c799d14d44437298dc00e19d8ad983d54ab333a24",
	},
	"kanz/infra/overlays/testnet-tokyo/data/postgres-provisioner.yaml": {
		"provision_database tv-sync tv_sync tv_sync", "'tv_sync_app','tv_sync_migrate'",
		"ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %I",
		"ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO %I",
	},
	"kanz/infra/overlays/testnet-tokyo/data/postgres-migrations.yaml": {
		"name: migrate-tv-sync", "value: /run/migration-dsns/tv-sync",
		"image: 012619468098.dkr.ecr.ap-northeast-1.amazonaws.com/kanz-migrate@sha256:147db5e4edbb6151f44f1b2c799d14d44437298dc00e19d8ad983d54ab333a24",
	},
	"tools/testnet-data-plane-recovery.sh": {"DATABASES=(accounting audit identity oms regulatory risk_engine venue_binance venue_okx tv_sync)"},
	"tools/Install-TestnetWorkloads.ps1": {
		"'kanz/infra/deploy/tv-sync-deploy.yaml'", "'tv_sync_preexisting=0'",
		"then tv_sync_preexisting=1; fi; capture_workloads",
		"delete deployment/tv-sync --ignore-not-found=true --wait=true",
		"then deployment_restored tv-sync || rollback_ok=0; fi;",
		"capture_policies > \"${policy_rollback}\"", "rollback_policies || rollback_ok=0",
		"networkpolicy/allow-gateway-runtime-egress networkpolicy/allow-gateway-to-tv-sync",
	},
	".github/workflows/kanz-ci.yml": {
		"TV_SYNC_TEST_ADMIN_URL: postgres://kanz:kanz@localhost:5432/kanz?sslmode=disable",
		"-run '^TestTokyoTVSyncSeparateRolesAndRecovery$' -count=1 -json",
		"python3 ../.github/scripts/check_tv_sync_evidence.py",
		"-p 'test_check_tv_sync_evidence.py'",
	},
	".github/scripts/check_tv_sync_evidence.py": {"from check_migration_evidence import verify", "required=REQUIRED", "ROOT + \"/TenantIsolation\"", "ROOT + \"/CheckpointAndTail\"", "ROOT + \"/FutureTableGrant\"", "ROOT + \"/RuntimeCannotDDL\""},
}

func checkTVTokyo(files map[string]string) error {
	for path, markers := range tvTokyoBoundaries {
		for _, marker := range markers {
			if !strings.Contains(files[path], marker) {
				return fmt.Errorf("%s missing tv-sync activation boundary %q", path, marker)
			}
		}
	}
	return nil
}

func TestTokyoTVSyncActivationBoundaries(t *testing.T) {
	root := filepath.Dir(moduleRoot(t))
	files := make(map[string]string)
	for path := range tvTokyoBoundaries {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		files[path] = string(data)
	}
	if err := checkTVTokyo(files); err != nil {
		t.Fatal(err)
	}
	for path, markers := range tvTokyoBoundaries {
		for _, marker := range markers {
			t.Run(path+"/"+marker, func(t *testing.T) {
				original := files[path]
				files[path] = strings.Replace(original, marker, "REMOVED_BOUNDARY", 1)
				defer func() { files[path] = original }()
				if checkTVTokyo(files) == nil {
					t.Fatal("missing activation boundary accepted")
				}
			})
		}
	}
}
