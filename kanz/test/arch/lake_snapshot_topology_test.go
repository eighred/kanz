package arch

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/lake/snapshotprofile"
	"github.com/nats-io/nats.go/jetstream"
	"gopkg.in/yaml.v3"
)

// An exact optional capability, not a lake.* exemption. The default bootstrap
// remains authoritative for every other subject. This resolves source wiring;
// the separate mandatory owner job proves both actual broker versions.
func optionalSnapshotTopology(files map[string]string, profile jetstream.StreamConfig) error {
	want := jetstream.StreamConfig{Name: "LAKE_SNAPSHOT", Subjects: []string{"lake.snapshot.requested", "lake.snapshot.capturing", "lake.snapshot.published", "lake.snapshot.failed"}, Retention: jetstream.LimitsPolicy, Storage: jetstream.FileStorage, Discard: jetstream.DiscardNew, MaxBytes: 64 << 20, MaxMsgs: 10000, MaxMsgSize: 64 << 10, MaxMsgsPerSubject: -1, MaxAge: 0, MaxConsumers: 2, Replicas: 1}
	if !reflect.DeepEqual(profile, want) {
		return fmt.Errorf("optional snapshot retained profile changed")
	}
	markers := map[string][]string{
		"internal/lake/snapshotprofile/profile.go":                 {"[]string{SubjectRequested, SubjectCapturing, SubjectPublished, SubjectFailed}"},
		"services/lake-sink/internal/snapshot/store.go":            {"const SubjectRequested = snapshotprofile.SubjectRequested", "subject = snapshotprofile.SubjectCapturing", "subject = snapshotprofile.SubjectPublished", "subject = snapshotprofile.SubjectFailed"},
		"services/lake-sink/internal/snapshot/runtime.go":          {"client.RequireAccountIdentity(startup, cfg.NATSAccount, svid.ID.String())", "client.RequireDurableRetention(startup, snapshotprofile.Stream())", `client.RequireRegisteredDefaultDurable(startup, snapshotprofile.Name, "lake.snapshot.>", "archiver")`, "client.RequireCanonicalParking(startup, SubjectRequested, int64(snapshotprofile.Stream().MaxMsgSize))"},
		"infra/nats/snapshot_candidate.go":                         {"snapshotprofile.Stream()", "hex.EncodeToString(sum[:]) != baselineSHA256", "uniqueUsers(accounts)"},
		"../.github/scripts/check_lake_snapshot_owner_evidence.py": {`VERSIONS = ("2.12.6", "2.14.5")`, `verify(events, {PACKAGE: NAMES})`, `TestSnapshotActualCanonicalArchiverFourFactsRestartAndTenantRefusal`, `TestSnapshotActualOwnerKillReplaysPersistedCut`, `TestSnapshotActualParkingAndRegisteredArchiverPreflight`, `TestSnapshotActualBrokerAccountProfileAndRestartRefusal`, `TestSnapshotActualRuntimeDurablePublicationQueryAndEstablishedChannelFailure`, `check_source_inventory(args.root)`, `check_manifest(proof,args.sha,args.tree,args.run,args.attempt,hashes)`},
		"../.github/scripts/release_ci.py":                         {`"architecture-migration.yml": {"compatibility", "finite-snapshot", "finite-snapshot-owner"}`},
	}
	for path, required := range markers {
		for _, marker := range required {
			if !strings.Contains(files[path], marker) {
				return fmt.Errorf("optional snapshot binding missing: %s", path)
			}
		}
	}
	var workflow struct {
		Jobs map[string]struct {
			If       any `yaml:"if"`
			Continue any `yaml:"continue-on-error"`
			Steps    []struct {
				Name     string            `yaml:"name"`
				If       any               `yaml:"if"`
				Continue any               `yaml:"continue-on-error"`
				Run      string            `yaml:"run"`
				Uses     string            `yaml:"uses"`
				With     map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if yaml.Unmarshal([]byte(files["../.github/workflows/architecture-migration.yml"]), &workflow) != nil {
		return fmt.Errorf("invalid snapshot workflow")
	}
	job, ok := workflow.Jobs["finite-snapshot-owner"]
	if !ok || job.If != nil || job.Continue != nil {
		return fmt.Errorf("mandatory snapshot owner job unavailable")
	}
	required := map[string][]string{
		"Build source-bound owner worker gateway archiver and CLI once": {"cargo +1.90.0 build --release --locked", "sudo install -o root -g root -m 0555", "./services/lake-sink/internal/snapshot", "./services/api-gateway/cmd/api-gateway", "./services/archiver/internal/archive", "./cmd/kanz-lake-snapshot"},
		"Install two checksum-pinned native brokers":                    {"for version in 2.12.6 2.14.5", "77fe7dd69ff5144126026b78355900be0ab0bb4339dc61a7621dcc9b9e9d07a6", "5e3b603d47c447bda1f77f9ac16dbf91c90aac4ff3681f8fbbc7201e4ed99355", "sha256sum --check --status", "--binary-inventory"},
		"Provision disposable scoped PostgreSQL and canonical Redpanda": {"NOSUPERUSER NOBYPASSRLS", "psql -X -w -h 127.0.0.1", "SELECT 1", "--wait-timeout 120 kafka", "snapshot-owner-proof/owned.json"},
		"Actual finite owner on both broker versions without skips":     {"for version in 2.12.6 2.14.5", "--regex --root .", "go tool test2json -t -p github.com/eighred/kanz/services/lake-sink/internal/snapshot", "-test.count=1", "-test.run \"$selection\"", "|| result=1", "exit \"$result\""},
		"Stop owned snapshot fixture dependencies and prove absence":    {"snapshot-owner-proof/owned.json", "down --volumes", "snapshot-owner-proof/cleanup.json", "$pg_after$compose_after$volumes_after$networks_after"},
		"Verify complete source-bound owner evidence":                   {"test_check_lake_snapshot_owner_evidence.py", "--seal --root .", "--binaries", "--sha", "--tree", "--run", "--attempt"},
	}
	seen := map[string]bool{}
	for _, step := range job.Steps {
		if step.Continue != nil {
			return fmt.Errorf("owner step failure bypass")
		}
		if patterns, ok := required[step.Name]; ok {
			if seen[step.Name] {
				return fmt.Errorf("duplicate mandatory owner step")
			}
			seen[step.Name] = true
			if step.Name == "Stop owned snapshot fixture dependencies and prove absence" {
				if step.If != "always()" {
					return fmt.Errorf("cleanup must always execute")
				}
			} else if step.If != nil {
				return fmt.Errorf("mandatory owner proof conditional")
			}
			for _, pattern := range patterns {
				if !strings.Contains(step.Run, pattern) {
					return fmt.Errorf("owner step binding missing: %s", step.Name)
				}
			}
		}
		if step.Name == "Retain source-bound finite owner proof" {
			if step.If != "always()" || step.With["name"] != "finite-snapshot-owner-${{ github.run_id }}-${{ github.run_attempt }}" || step.With["if-no-files-found"] != "error" || !strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
				return fmt.Errorf("owner artifact identity missing")
			}
			seen[step.Name] = true
		}
	}
	if len(seen) != len(required)+1 {
		return fmt.Errorf("missing mandatory owner steps")
	}
	return nil
}

func optionalSnapshotFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, path := range []string{"internal/lake/snapshotprofile/profile.go", "services/lake-sink/internal/snapshot/store.go", "services/lake-sink/internal/snapshot/runtime.go", "infra/nats/snapshot_candidate.go", "../.github/scripts/check_lake_snapshot_owner_evidence.py", "../.github/scripts/release_ci.py", "../.github/workflows/architecture-migration.yml"} {
		files[path] = read(t, filepath.Join(root, path))
	}
	return files
}

func optionalSnapshotSubjects(t *testing.T, root string) []string {
	t.Helper()
	profile := snapshotprofile.Stream()
	if err := optionalSnapshotTopology(optionalSnapshotFiles(t, root), profile); err != nil {
		t.Fatal(err)
	}
	return profile.Subjects
}

func TestLakeSnapshotOptionalTopologyRequiresSeparateActualProof(t *testing.T) {
	files := optionalSnapshotFiles(t, moduleRoot(t))
	profile := snapshotprofile.Stream()
	if err := optionalSnapshotTopology(files, profile); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct{ path, old, replacement string }{
		{"services/lake-sink/internal/snapshot/runtime.go", "client.RequireAccountIdentity", "client.OmittedIdentity"},
		{"services/lake-sink/internal/snapshot/runtime.go", "client.RequireRegisteredDefaultDurable", "client.OmittedArchive"},
		{"services/lake-sink/internal/snapshot/runtime.go", "client.RequireCanonicalParking", "client.OmittedParking"},
		{"services/lake-sink/internal/snapshot/store.go", "subject = snapshotprofile.SubjectFailed", "subject = SubjectRequested"},
		{"infra/nats/snapshot_candidate.go", "snapshotprofile.Stream()", "otherProfile()"},
		{"../.github/workflows/architecture-migration.yml", "  finite-snapshot-owner:", "  removed-owner:"},
		{"../.github/workflows/architecture-migration.yml", "  finite-snapshot-owner:", "  finite-snapshot-owner:\n    continue-on-error: true"},
		{"../.github/workflows/architecture-migration.yml", "  finite-snapshot-owner:", "  finite-snapshot-owner:\n    if: false"},
		{"../.github/workflows/architecture-migration.yml", "for version in 2.12.6 2.14.5", "for version in 2.14.5"},
		{"../.github/workflows/architecture-migration.yml", "--seal --root .", "--regex --root ."},
		{"../.github/workflows/architecture-migration.yml", "NOSUPERUSER NOBYPASSRLS", "SUPERUSER BYPASSRLS"},
		{"../.github/workflows/architecture-migration.yml", "--wait-timeout 120 kafka", "--wait-timeout 120 nats"},
		{"../.github/workflows/architecture-migration.yml", "|| result=1", "|| true"},
		{"../.github/scripts/check_lake_snapshot_owner_evidence.py", "verify(events, {PACKAGE: NAMES})", "pass"},
		{"../.github/scripts/release_ci.py", `, "finite-snapshot-owner"`, ""},
	} {
		changed := map[string]string{}
		for key, value := range files {
			changed[key] = value
		}
		if !strings.Contains(changed[mutation.path], mutation.old) {
			t.Fatal("mutation anchor absent")
		}
		changed[mutation.path] = strings.ReplaceAll(changed[mutation.path], mutation.old, mutation.replacement)
		if optionalSnapshotTopology(changed, profile) == nil {
			t.Errorf("weakening accepted: %s", mutation.old)
		}
	}
	for _, change := range []func(*jetstream.StreamConfig){func(p *jetstream.StreamConfig) { p.Subjects = p.Subjects[:3] }, func(p *jetstream.StreamConfig) { p.Subjects = []string{"lake.>"} }, func(p *jetstream.StreamConfig) { p.Discard = jetstream.DiscardOld }, func(p *jetstream.StreamConfig) { p.MaxBytes = -1 }, func(p *jetstream.StreamConfig) { p.MaxConsumers = 1 }} {
		p := snapshotprofile.Stream()
		change(&p)
		if optionalSnapshotTopology(files, p) == nil {
			t.Error("changed optional profile accepted")
		}
	}
}
