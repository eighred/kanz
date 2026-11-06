package arch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func checkTVReplayVerifier(files map[string]string) error {
	required := map[string][]string{
		"services/tv-sync/internal/projection/verify_replay.go": {
			"IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly",
			"currentTenant != tenant", "unsafe", "rolsuper OR rolbypassrls", "relrowsecurity AND relforcerowsecurity",
			"octet_length(event_id)", "octet_length(event_type)", "maxID > 1024", "maxType > 1024",
			"foreignFacts != 0", "foreignCheckpoints != 0", "count(*) FILTER (WHERE tenant_id IS DISTINCT FROM $1)",
			"snap.count == 0", "ErrReplayNoHistory", "checkpointRows == 0", "ErrReplayNoCheckpoint",
			"full: true", "full.Rehydrate(ctx)", "restored.Rehydrate(ctx)", "!proto.Equal(want, got)", "tx.Rollback(ctx)",
			"compareReplayExact(full, restored)", "hashReplayPart(stateDigest, exactHash[:])",
		},
		"services/tv-sync/internal/projection/verify_replay_validation.go": {
			"RecursionLimit: 64", "len(m.GetUnknown()) != 0", "boundedReplayRational(s)", "a.TenantId != tenant", "cp.Seq != snap.checkpointSeq",
		},
		"services/tv-sync/cmd/tv-sync-replay/main.go": {
			"!version.IsStamped()", "expected != version.String()", `os.Getenv("TV_SYNC_DATABASE_URL") != ""`,
			`secret.ReadBounded("TV_SYNC_DATABASE_URL", 16<<10)`, "pg.NewTenantPool(ctx, dsn, tenant)", "projection.VerifyPostgresSnapshot(ctx, pool, tenant, retention)",
		},
		"pkg/secret/secret.go":                                        {"io.LimitReader(f, maxBytes+1)", "maxBytes > 1<<20", "!info.Mode().IsRegular()"},
		"pkg/secret/bounded_mount_linux.go":                           {"unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK"},
		"services/tv-sync/internal/projection/verify_replay_exact.go": {"a.Cmp(b) != 0", "{a.baseline, b.baseline}, {a.live, b.live}", "a.Num().Bytes()", "a.Denom().Bytes()"},
		"services/tv-sync/Dockerfile":                                 {"/out/tv-sync-replay ./services/tv-sync/cmd/tv-sync-replay", "COPY --from=build /out/tv-sync-replay /tv-sync-replay"},
	}
	for path, markers := range required {
		for _, marker := range markers {
			if !strings.Contains(files[path], marker) {
				return fmt.Errorf("%s lacks %s", path, marker)
			}
		}
	}
	verifier := files["services/tv-sync/internal/projection/verify_replay.go"]
	if strings.Contains(verifier, ".fold(") || strings.Contains(verifier, "replayEverythingForTest") {
		return fmt.Errorf("verifier must use the production Rehydrate fold")
	}
	command := files["services/tv-sync/cmd/tv-sync-replay/main.go"]
	for _, forbidden := range []string{"pkg/bus", "os/exec", "logger.", "fmt.Print", "err.Error()"} {
		if strings.Contains(command, forbidden) {
			return fmt.Errorf("verifier command must not publish or disclose errors")
		}
	}
	return nil
}

func checkTVReplayCI(data []byte) error {
	return checkTVProofCI(data, "check_tv_replay_evidence.py --pattern", "TEST_POSTGRES_URL", "postgres://kanzapp:kanz@localhost:5432/kanzapp?sslmode=disable", []string{"set -euo pipefail", "python3 -m unittest discover -s ../.github/scripts -p test_check_tv_replay_evidence.py", `go test -race -p 1 -json -count=1 -run "$(python3 ../.github/scripts/check_tv_replay_evidence.py --pattern)" ./services/tv-sync/internal/projection ./pkg/secret | tee "$RUNNER_TEMP/tv-replay-proof.jsonl"`, `python3 ../.github/scripts/check_tv_replay_evidence.py "$RUNNER_TEMP/tv-replay-proof.jsonl"`})
}

func checkTVProofCI(data []byte, pattern, envName, envValue string, want []string) error {
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Run, If  string
				Continue bool `yaml:"continue-on-error"`
				Env      map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return err
	}
	for _, step := range workflow.Jobs["go"].Steps {
		if !strings.Contains(step.Run, pattern) {
			continue
		}
		if step.If != "" || step.Continue || step.Env[envName] != envValue {
			return fmt.Errorf("replay proof must be unconditional with real non-superuser PostgreSQL")
		}
		lines := strings.Split(step.Run, "\n")
		found := 0
		for _, line := range lines {
			if found < len(want) && strings.TrimSpace(line) == want[found] {
				found++
			}
		}
		if found != len(want) {
			return fmt.Errorf("replay proof execution/checker must retain order and fail-closed shell")
		}
		return nil
	}
	return fmt.Errorf("missing real replay qualification")
}

func TestTVReplayVerifierRequiresActualPostgresEvidence(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(moduleRoot(t)), ".github/workflows/kanz-ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkTVReplayCI(data); err != nil {
		t.Fatal(err)
	}
	for _, change := range [][2]string{{`-race -p 1 -json -count=1 -run "$(python3 ../.github/scripts/check_tv_replay_evidence.py --pattern)"`, `-p 1 -json -count=1 -run "$(python3 ../.github/scripts/check_tv_replay_evidence.py --pattern)"`}, {"python3 -m unittest discover -s ../.github/scripts -p test_check_tv_replay_evidence.py", "echo no-tests"}, {`python3 ../.github/scripts/check_tv_replay_evidence.py "$RUNNER_TEMP/tv-replay-proof.jsonl"`, "echo no-proof"}, {"check_tv_replay_evidence.py --pattern", "check_tv_replay_evidence.py --unused"}} {
		mutated := strings.Replace(string(data), change[0], change[1], 1)
		if mutated == string(data) {
			t.Fatal("mutation missed target")
		}
		if checkTVReplayCI([]byte(mutated)) == nil {
			t.Fatal("weakened real proof accepted")
		}
	}
}

func TestTVReplayVerifierPreservesReadOnlyCanonicalBoundary(t *testing.T) {
	paths := []string{"services/tv-sync/internal/projection/verify_replay.go", "services/tv-sync/internal/projection/verify_replay_validation.go", "services/tv-sync/cmd/tv-sync-replay/main.go", "pkg/secret/secret.go", "services/tv-sync/Dockerfile", "pkg/secret/bounded_mount_linux.go", "services/tv-sync/internal/projection/verify_replay_exact.go"}
	files := map[string]string{}
	for _, path := range paths {
		b, err := os.ReadFile(filepath.Join(moduleRoot(t), path))
		if err != nil {
			t.Fatal(err)
		}
		files[path] = string(b)
	}
	if err := checkTVReplayVerifier(files); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct{ path, from, to string }{
		{paths[0], "AccessMode: pgx.ReadOnly", "AccessMode: pgx.ReadWrite"},
		{paths[0], "IsoLevel: pgx.RepeatableRead", "IsoLevel: pgx.ReadCommitted"},
		{paths[0], "currentTenant != tenant", "false"},
		{paths[0], "relrowsecurity AND relforcerowsecurity", "true"},
		{paths[0], "maxID > 1024", "false"},
		{paths[0], "maxType > 1024", "false"},
		{paths[0], "foreignFacts != 0", "false"},
		{paths[0], "foreignCheckpoints != 0", "false"},
		{paths[0], "full: true", "full: false"},
		{paths[0], "!proto.Equal(want, got)", "false"},
		{paths[0], "compareReplayExact(full, restored)", "compareRounded(full, restored)"},
		{paths[0], "hashReplayPart(stateDigest, exactHash[:])", "hashReplayPart(stateDigest, nil)"},
		{paths[1], "len(m.GetUnknown()) != 0", "false"},
		{paths[1], "boundedReplayRational(s)", "true"},
		{paths[2], "!version.IsStamped()", "false"},
		{paths[2], "expected != version.String()", "false"},
		{paths[3], "io.LimitReader(f, maxBytes+1)", "f"},
		{paths[5], "|unix.O_NONBLOCK", ""},
		{paths[6], "a.Cmp(b) != 0", "false"},
	} {
		original := files[mutation.path]
		changed := strings.Replace(original, mutation.from, mutation.to, 1)
		if changed == original {
			t.Fatalf("mutation missed %s", mutation.from)
		}
		files[mutation.path] = changed
		if checkTVReplayVerifier(files) == nil {
			t.Fatalf("accepted weakened %s", mutation.from)
		}
		files[mutation.path] = original
	}
}
